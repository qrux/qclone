package copier

// Rescue: resuming interrupted files and mapping unreadable regions.
//
// A file in progress has a hidden .part file beside its destination. Every two
// seconds, and when a run stops, tclone records which byte ranges of the part
// are written (after an fsync, so the record never claims more than the disk
// has) in a .map file next to it: the same idea as ddrescue's mapfile. A rerun
// with the same source (same size and modification time) copies only what the
// map says is missing.
//
// A chunk that still can't be read after its retries is split in halves, and
// the halves that fail are split again, down to 4 KiB sectors: the readable
// pieces are kept, and the unreadable ranges are recorded. Without --rescue
// such a file is not installed; its part and map stay for the next run, which
// retries only those ranges. With --rescue the ranges are filled with zeros,
// the file is installed, and a .bad marker beside it lists the ranges, so a
// later run doesn't take the file as finished (its size and time match) but
// re-reads just those ranges into a clone of it.

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
)

// sector is the smallest piece bisection reads.
const sector = 4096

// salvageBudget is how many failed reads one chunk may spend on bisection.
// A dying drive can take seconds per failed read; past the budget, what is
// left unresolved is recorded as one coarser bad range (a rerun retries it).
const salvageBudget = 64

// flushEvery is how often progress maps are written.
const flushEvery = 2 * time.Second

type span struct{ Off, Len int64 }

func (s span) end() int64 { return s.Off + s.Len }

// ranges is a sorted set of non-overlapping, non-touching spans.
type ranges []span

func (r *ranges) add(off, n int64) {
	if n <= 0 {
		return
	}
	s := span{off, n}
	rs := *r
	i := sort.Search(len(rs), func(k int) bool { return rs[k].end() >= s.Off })
	j := i
	for j < len(rs) && rs[j].Off <= s.end() {
		if rs[j].Off < s.Off {
			s.Len += s.Off - rs[j].Off
			s.Off = rs[j].Off
		}
		if rs[j].end() > s.end() {
			s.Len = rs[j].end() - s.Off
		}
		j++
	}
	out := append(rs[:i:i], s)
	*r = append(out, rs[j:]...)
}

func (r ranges) total() int64 {
	var t int64
	for _, s := range r {
		t += s.Len
	}
	return t
}

func (r ranges) last() int64 {
	if len(r) == 0 {
		return 0
	}
	return r[len(r)-1].end()
}

// gaps is everything in [0, size) not in r.
func (r ranges) gaps(size int64) ranges {
	var g ranges
	var at int64
	for _, s := range r {
		if s.Off > at {
			g = append(g, span{at, s.Off - at})
		}
		at = s.end()
	}
	if at < size {
		g = append(g, span{at, size - at})
	}
	return g
}

func (r ranges) clone() ranges { return append(ranges(nil), r...) }

func (r ranges) pairs() [][2]int64 {
	p := make([][2]int64, len(r))
	for i, s := range r {
		p[i] = [2]int64{s.Off, s.Len}
	}
	return p
}

func fromPairs(p [][2]int64, size int64) (ranges, bool) {
	var r ranges
	for _, x := range p {
		if x[0] < 0 || x[1] <= 0 || x[0]+x[1] > size {
			return nil, false
		}
		r.add(x[0], x[1])
	}
	return r, true
}

// count is "1 range", "2 ranges".
func count(n int, word string) string {
	if n == 1 {
		return "1 " + word
	}
	return fmt.Sprintf("%d %ss", n, word)
}

// describe lists ranges for the error file: "4096 bytes at 2105344, ...".
func (r ranges) describe(max int) string {
	var parts []string
	for i, s := range r {
		if i == max {
			parts = append(parts, fmt.Sprintf("and %d more (all in the .map or .bad file)", len(r)-max))
			break
		}
		parts = append(parts, fmt.Sprintf("%d bytes at %d", s.Len, s.Off))
	}
	return strings.Join(parts, ", ")
}

// mapFile is the record kept beside a part (.map) or a rescued file (.bad).
type mapFile struct {
	V     int        `json:"v"`
	Name  string     `json:"name"`     // the destination file's name
	Size  int64      `json:"size"`     // the source's size...
	MTime int64      `json:"mtime_ns"` // ...and modification time, when the record was made
	Done  [][2]int64 `json:"done"`     // [offset, length] written to the part
	Bad   [][2]int64 `json:"bad"`      // [offset, length] that could not be read
}

func loadMap(path string) (*mapFile, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var m mapFile
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	if m.V != 1 {
		return nil, fmt.Errorf("unknown map version %d", m.V)
	}
	return &m, nil
}

// saveMap writes path atomically: a crash leaves the old record or the new one.
func saveMap(path string, m *mapFile) error {
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	_, err = f.Write(append(b, '\n'))
	if err == nil {
		err = syscall.Fsync(int(f.Fd()))
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp, path)
	}
	if err != nil {
		os.Remove(tmp)
	}
	return err
}

// matches says whether a record was made for this source.
func (m *mapFile) matches(f *file) bool {
	return m.Name == filepath.Base(f.dst) && m.Size == f.size && m.MTime == f.mtime.UnixNano()
}

func partPath(base string) string   { return base + ".part" }
func mapPath(base string) string    { return base + ".map" }
func markerPath(base string) string { return base + ".bad" }

// addDone and addBad record progress; the counts shown live are kept in step.
func (e *Engine) addDone(f *file, off, n int64) {
	f.rangeMu.Lock()
	f.doneSet.add(off, n)
	f.dirty = true
	f.rangeMu.Unlock()
}

func (e *Engine) addBad(f *file, off, n int64) {
	f.rangeMu.Lock()
	before, beforeBytes := len(f.badSet), f.badSet.total()
	f.badSet.add(off, n)
	f.dirty = true
	after, afterBytes := len(f.badSet), f.badSet.total()
	f.rangeMu.Unlock()
	if before == 0 {
		e.badFiles.Add(1)
	}
	e.badRanges.Add(int64(after - before))
	e.badBytes.Add(afterBytes - beforeBytes)
}

// salvage is called for a chunk that failed every retry. It bisects the chunk
// down to sectors, writes what reads, records what doesn't, and returns the
// bytes it wrote. A piece left unaccounted (on cancel) is simply still missing,
// so a rerun copies it.
func (e *Engine) salvage(f *file, canceled func() bool, b []byte, off int64) int {
	budget := salvageBudget
	good := 0
	var split func(o int64, l int)
	split = func(o int64, l int) {
		if f.failed.Load() {
			return
		}
		if canceled() {
			f.canceled.Store(true)
			return
		}
		if l <= sector || budget <= 0 {
			e.addBad(f, o, int64(l))
			return
		}
		half := (l / 2) &^ (sector - 1)
		if half == 0 {
			half = sector
		}
		for _, p := range [2]span{{o, int64(half)}, {o + int64(half), int64(l - half)}} {
			if p.Len <= 0 || f.failed.Load() || f.canceled.Load() {
				continue
			}
			piece := b[p.Off-off : p.end()-off]
			k, err := e.readAt(f, piece, p.Off)
			e.readBytes.Add(int64(k))
			switch {
			case k == len(piece):
				if _, werr := f.df.WriteAt(piece, p.Off); werr != nil {
					f.fail("write", werr)
					return
				}
				e.addDone(f, p.Off, p.Len)
				good += len(piece)
			case err == nil || err == io.EOF:
				f.fail(fmt.Sprintf("read at byte %d", p.Off), fmt.Errorf("file shrank while being copied (was %d bytes)", f.size))
				return
			default:
				budget--
				split(p.Off, int(p.Len))
			}
		}
	}
	split(off, len(b))
	return good
}

// prepare runs in the dispatcher before a file's chunks are cut: it claims the
// part's name and, when an earlier run left usable progress, starts from it.
func (e *Engine) prepare(f *file) {
	f.tmp, f.base = e.claimTmp(f.dst)
	if f.base == "" {
		return // a second file with this name in flight: no resuming for it
	}
	part, mp := partPath(f.base), mapPath(f.base)
	switch {
	case f.patch != nil:
		// Installed earlier with zeros in unreadable ranges: work on a clone of
		// it (instant on APFS) and read only those ranges again.
		os.Remove(part)
		os.Remove(mp)
		if err := cloneFile(f.dst, part); err != nil {
			os.Remove(part)
			f.patch = nil
			return // copy it whole instead
		}
		f.doneSet = f.patch.gaps(f.size)
		f.resuming = true
	default:
		m, err := loadMap(mp)
		if err != nil || !m.matches(f) {
			if err == nil || !os.IsNotExist(err) {
				os.Remove(mp) // a record for an older version of the source, or unreadable
			}
			return
		}
		done, ok := fromPairs(m.Done, f.size)
		fi, serr := os.Lstat(part)
		if !ok || serr != nil || !fi.Mode().IsRegular() || fi.Size() < done.last() || fi.Size() > f.size {
			os.Remove(mp)
			return
		}
		f.doneSet = done
		f.resuming = true
	}
	if n := f.doneSet.total(); n > 0 {
		f.done.Add(n)
		e.bytesDone.Add(n)
		e.bytesResumed.Add(n)
	}
}

// loadMarker returns the unreadable ranges recorded for a destination file
// that was installed with zeros (--rescue), if the record is for this source.
func (e *Engine) loadMarker(f *file) ranges {
	path := markerPath(e.tmpBase(f.dst))
	m, err := loadMap(path)
	if err != nil {
		if !os.IsNotExist(err) {
			os.Remove(path)
		}
		return nil
	}
	bad, ok := fromPairs(m.Bad, f.size)
	if !m.matches(f) || !ok || len(bad) == 0 {
		os.Remove(path) // stale: the source changed since
		return nil
	}
	return bad
}

// saveProgress records a part's progress, after making sure the disk has it.
func (e *Engine) saveProgress(f *file) error {
	f.rangeMu.Lock()
	m := &mapFile{V: 1, Name: filepath.Base(f.dst), Size: f.size, MTime: f.mtime.UnixNano(), Done: f.doneSet.pairs(), Bad: f.badSet.pairs()}
	f.dirty = false
	f.rangeMu.Unlock()
	if err := syscall.Fsync(int(f.df.Fd())); err != nil {
		return err
	}
	return saveMap(mapPath(f.base), m)
}

// flushMaps writes progress for files that have been going a while, so a
// crash or a forced quit loses at most a couple of seconds of work.
func (e *Engine) flushMaps(now time.Time) {
	e.mu.Lock()
	var fs []*file
	for f := range e.active {
		fs = append(fs, f)
	}
	e.mu.Unlock()
	for _, f := range fs {
		f.ioMu.Lock()
		f.rangeMu.Lock()
		due := f.dirty && f.base != "" && f.df != nil && !f.closed && now.Sub(f.started) >= flushEvery && now.Sub(f.flushed) >= flushEvery
		f.rangeMu.Unlock()
		if due {
			if err := e.saveProgress(f); err == nil {
				f.flushed = now
			}
		}
		f.ioMu.Unlock()
	}
}

// zeroFill writes zeros over the unreadable ranges of a part.
func zeroFill(df *os.File, bad ranges) error {
	zeros := make([]byte, 1<<20)
	for _, s := range bad {
		for o := s.Off; o < s.end(); {
			n := int64(len(zeros))
			if rem := s.end() - o; rem < n {
				n = rem
			}
			if _, err := df.WriteAt(zeros[:n], o); err != nil {
				return err
			}
			o += n
		}
	}
	return nil
}

// copyPlain copies src to dst byte for byte (when cloning isn't possible).
func copyPlain(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	_, err = io.Copy(out, in)
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	return err
}

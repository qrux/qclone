// Package copier is tclone's engine.
//
// One goroutine walks the sources and queues files (copying can start before
// the walk ends). A dispatcher cuts each file, in order, into chunks of the
// tuner's current size. A pool of workers takes chunks through a gate that
// admits the tuner's current number of readers; each worker reads its chunk
// and writes it at the same offset in a hidden temporary file beside the
// destination. So a big file is read by several workers at once and many
// small files are copied side by side, under one knob. When a file's last
// chunk lands, the temporary file gets the source's mode and times and is
// renamed into place, so a destination file is either the old one or a
// complete new one, never half-written.
package copier

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"tclone/internal/devstat"
	"tclone/internal/plan"
	"tclone/internal/rate"
	"tclone/internal/sim"
	"tclone/internal/tuner"
)

// Options configure a run.
type Options struct {
	Start        tuner.Setting
	Tuner        tuner.Config
	Fixed        bool          // never change the setting
	Cached       bool          // read and write through the OS cache
	ModifyWindow time.Duration // mtimes this close count as equal
	Retries      int           // read attempts per chunk
	Errors       *ErrorLog
	Sim          *sim.Device // pretend the source is this drive
}

// Active is one file in flight.
type Active struct {
	Show   string
	Size   int64
	Done   int64
	Rate   float64
	RateOK bool
	Age    time.Duration
}

// DeviceView is one disk as the display shows it.
type DeviceView struct {
	Role    string // "source", "dest" or "source+dest"
	Info    devstat.Info
	Rates   devstat.Rates
	OK      bool          // rates are from two good samples
	Idle    bool          // the disk did nothing tclone cares about for the whole history kept
	Window  time.Duration // what the rates are averaged over
	Errors  uint64        // read+write errors since tclone started
	Retries uint64
}

// Snapshot is everything the display needs, taken at one moment.
type Snapshot struct {
	Now, Start                                       time.Time
	Scanning                                         bool
	FilesTotal, FilesDone, FilesSkipped, FilesFailed int64
	Links, Dirs                                      int64
	BytesTotal, BytesDone, BytesSkipped, BytesFailed int64
	Rate                                             float64
	RateOK                                           bool
	Avg                                              float64
	Peak                                             float64
	Idle                                             time.Duration
	Active                                           []Active
	Tuner                                            tuner.Status
	Devices                                          []DeviceView
	Errors                                           int64
	ErrorFile                                        string
	LastError                                        string
	Retries                                          int64
	Done, Canceled                                   bool
}

type file struct {
	show, src, dst, tmp string
	size                int64
	mode                fs.FileMode
	mtime               time.Time
	started             time.Time
	meter               *rate.Meter
	done                atomic.Int64
	refs                atomic.Int64

	once    sync.Once
	sf, df  *os.File
	openErr error

	errMu    sync.Mutex
	failOp   string
	failErr  error
	failed   atomic.Bool
	canceled atomic.Bool
}

func (f *file) fail(op string, err error) {
	f.errMu.Lock()
	if f.failErr == nil {
		f.failOp, f.failErr = op, err
	}
	f.errMu.Unlock()
	f.failed.Store(true)
}

type job struct {
	f   *file
	off int64
	n   int
}

type dirAttr struct {
	dst   string
	mode  fs.FileMode
	mtime time.Time
}

type devWatch struct {
	role      string
	src       devstat.Source
	first     devstat.Stats
	prevAt    time.Time
	view      DeviceView
	haveFirst bool
	hist      []devSample // one a second, newest last, up to devHistory
}

type devSample struct {
	at time.Time
	s  devstat.Stats
}

// devHistory is how far back a disk's rate may reach to find movement.
const devHistory = 120 * time.Second

// moved reports whether the counters tclone cares about for this role changed.
func (w *devWatch) moved(a, b devstat.Stats) bool {
	switch w.role {
	case "source":
		return b.ReadBytes != a.ReadBytes
	case "dest":
		return b.WriteBytes != a.WriteBytes
	}
	return b.ReadBytes != a.ReadBytes || b.WriteBytes != a.WriteBytes
}

// Engine runs one copy.
type Engine struct {
	plan  *plan.Plan
	opt   Options
	start time.Time

	tuner  *tuner.Tuner
	gate   *gate
	chunk  atomic.Int64
	global *rate.Meter
	peak   atomic.Uint64 // float64 bits

	filesTotal, filesDone, filesSkipped, filesFailed atomic.Int64
	links, dirsSeen                                  atomic.Int64
	bytesTotal, bytesDone, bytesSkipped, bytesFailed atomic.Int64
	readBytes, retries                               atomic.Int64
	scanning, dispatchIdle, dispatchDone             atomic.Bool
	canceled, done                                   atomic.Bool

	q       fileQueue
	jobs    chan job
	opStart []atomic.Int64 // per worker: unix nanos of the op in flight, 0 when idle

	mu     sync.Mutex
	active map[*file]struct{}
	dirs   []dirAttr
	seen   map[string]struct{}
	devs   []*devWatch
	srcDev *devWatch // the one source disk, when there is exactly one

	readFault func(show string, off int64) error // tests only
}

// New prepares a run.
func New(p *plan.Plan, opt Options) *Engine {
	if opt.Retries < 1 {
		opt.Retries = 1
	}
	now := time.Now()
	e := &Engine{
		plan:   p,
		opt:    opt,
		start:  now,
		tuner:  tuner.New(opt.Tuner, opt.Start, opt.Fixed),
		gate:   newGate(opt.Start.Readers),
		global: rate.New(now),
		active: map[*file]struct{}{},
		jobs:   make(chan job, opt.Tuner.MaxReaders),
	}
	e.opStart = make([]atomic.Int64, opt.Tuner.MaxReaders)
	s := e.tuner.Setting()
	e.gate.setLimit(s.Readers)
	e.chunk.Store(int64(s.Chunk))
	e.q.init()
	if p.Overlap {
		e.seen = map[string]struct{}{}
	}
	e.scanning.Store(true)
	e.openDevices()
	return e
}

func (e *Engine) openDevices() {
	byName := map[string]*devWatch{}
	add := func(role string, src devstat.Source) {
		name := src.Info().Name
		if w, ok := byName[name]; ok {
			if w.role != role {
				w.role = "source+dest"
			}
			src.Close()
			return
		}
		w := &devWatch{role: role, src: src}
		byName[name] = w
		e.devs = append(e.devs, w)
	}
	if e.opt.Sim != nil {
		add("source", e.opt.Sim)
	} else {
		for _, r := range e.plan.Roots {
			if d, err := devstat.ForPath(r.Src); err == nil {
				add("source", d)
			}
		}
	}
	if d, err := devstat.ForPath(existingAncestor(e.plan.Dst)); err == nil {
		add("dest", d)
	}
	var srcs []*devWatch
	for _, w := range e.devs {
		if w.role != "dest" {
			srcs = append(srcs, w)
		}
	}
	if len(srcs) == 1 {
		e.srcDev = srcs[0]
	}
	for _, w := range e.devs {
		w.view = DeviceView{Role: w.role, Info: w.src.Info()}
	}
	e.pollDevices(time.Now(), true)
}

func existingAncestor(p string) string {
	for {
		if _, err := os.Stat(p); err == nil {
			return p
		}
		parent := filepath.Dir(p)
		if parent == p {
			return p
		}
		p = parent
	}
}

// Run copies everything and returns when done or canceled.
func (e *Engine) Run(ctx context.Context) error {
	defer func() {
		for _, w := range e.devs {
			w.src.Close()
		}
	}()
	if err := os.MkdirAll(e.plan.Dst, 0o755); err != nil {
		return err
	}
	parent := ctx
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		<-ctx.Done()
		e.q.close()
		e.gate.openAll()
	}()

	var wg sync.WaitGroup
	go e.scan(ctx)
	go e.dispatch(ctx)
	for i := 0; i < e.opt.Tuner.MaxReaders; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			e.worker(ctx, id)
		}(i)
	}
	stop := make(chan struct{})
	ctlDone := make(chan struct{})
	go func() {
		defer close(ctlDone)
		e.control(stop)
	}()
	wg.Wait()
	close(stop)
	<-ctlDone
	if parent.Err() != nil {
		e.canceled.Store(true)
	} else {
		e.fixDirs()
	}
	e.pollDevices(time.Now(), true)
	e.done.Store(true)
	return nil
}

func (e *Engine) scan(ctx context.Context) {
	defer func() {
		e.scanning.Store(false)
		e.q.close()
	}()
	plan.Walk(ctx, e.plan, func(en plan.Entry) {
		if en.Err != nil {
			e.logErr("scan", en.Src, en.Err)
			return
		}
		m := en.Info.Mode()
		switch {
		case m.IsDir():
			if err := os.MkdirAll(en.Dst, 0o755); err != nil {
				e.logErr("mkdir", en.Dst, err)
				return
			}
			e.dirsSeen.Add(1)
			e.mu.Lock()
			e.dirs = append(e.dirs, dirAttr{dst: en.Dst, mode: m, mtime: en.Info.ModTime()})
			e.mu.Unlock()
		case m.IsRegular():
			if e.seen != nil {
				e.mu.Lock()
				_, dup := e.seen[en.Dst]
				e.seen[en.Dst] = struct{}{}
				e.mu.Unlock()
				if dup {
					e.logErr("skip", en.Src, fmt.Errorf("an earlier source already copies to %s", en.Dst))
					return
				}
			}
			size := en.Info.Size()
			if fi, err := os.Lstat(en.Dst); err == nil && fi.Mode().IsRegular() && fi.Size() == size && near(fi.ModTime(), en.Info.ModTime(), e.opt.ModifyWindow) {
				e.filesSkipped.Add(1)
				e.bytesSkipped.Add(size)
				return
			}
			f := &file{show: en.Show, src: en.Src, dst: en.Dst, tmp: tmpName(en.Dst), size: size, mode: m, mtime: en.Info.ModTime()}
			e.filesTotal.Add(1)
			e.bytesTotal.Add(size)
			e.q.push(f)
		case m&fs.ModeSymlink != 0:
			e.copyLink(en)
		default:
			e.logErr("skip", en.Src, fmt.Errorf("not copied: %s", kindOf(m)))
		}
	})
}

func near(a, b time.Time, window time.Duration) bool {
	d := a.Sub(b)
	if d < 0 {
		d = -d
	}
	return d <= window
}

func kindOf(m fs.FileMode) string {
	switch {
	case m&fs.ModeNamedPipe != 0:
		return "named pipe"
	case m&fs.ModeSocket != 0:
		return "socket"
	case m&fs.ModeDevice != 0:
		return "device"
	}
	return "special file"
}

// tmpName is the hidden file a copy is written to before it's renamed into place.
func tmpName(dst string) string {
	h := fnv.New64a()
	io.WriteString(h, filepath.Base(dst))
	return filepath.Join(filepath.Dir(dst), fmt.Sprintf(".tclone-%012x.part", h.Sum64()&0xffffffffffff))
}

func (e *Engine) copyLink(en plan.Entry) {
	target, err := os.Readlink(en.Src)
	if err != nil {
		e.logErr("link", en.Src, err)
		return
	}
	if have, err := os.Readlink(en.Dst); err == nil && have == target {
		e.filesSkipped.Add(1)
		return
	}
	tmp := tmpName(en.Dst)
	os.Remove(tmp) // a leftover of tclone's own, from an earlier run
	if err := os.Symlink(target, tmp); err != nil {
		e.logErr("link", en.Dst, err)
		return
	}
	if err := os.Rename(tmp, en.Dst); err != nil {
		os.Remove(tmp)
		e.logErr("link", en.Dst, err)
		return
	}
	e.links.Add(1)
}

func (e *Engine) dispatch(ctx context.Context) {
	defer func() {
		e.dispatchDone.Store(true)
		close(e.jobs)
	}()
	for {
		e.dispatchIdle.Store(true)
		f, ok := e.q.pop()
		e.dispatchIdle.Store(false)
		if !ok || ctx.Err() != nil {
			return
		}
		f.started = time.Now()
		f.meter = rate.New(f.started)
		f.refs.Store(1) // the dispatcher's own reference
		e.mu.Lock()
		e.active[f] = struct{}{}
		e.mu.Unlock()
		var off int64
		for first := true; first || off < f.size; first = false {
			n := int(e.chunk.Load())
			if rem := f.size - off; int64(n) > rem {
				n = int(rem)
			}
			f.refs.Add(1)
			select {
			case e.jobs <- job{f: f, off: off, n: n}:
			case <-ctx.Done():
				f.refs.Add(-1)
				f.canceled.Store(true)
				e.release(f)
				return
			}
			off += int64(n)
			if f.failed.Load() {
				break
			}
		}
		e.release(f)
	}
}

func (e *Engine) worker(ctx context.Context, id int) {
	var buf []byte
	for {
		e.gate.acquire()
		j, ok := <-e.jobs
		if !ok {
			e.gate.release()
			return
		}
		if ctx.Err() != nil {
			e.gate.release()
			j.f.canceled.Store(true)
			e.release(j.f)
			continue
		}
		t0 := time.Now()
		e.opStart[id].Store(t0.UnixNano())
		n := e.copyChunk(ctx, j, &buf)
		t1 := time.Now()
		e.opStart[id].Store(0)
		e.gate.release()
		e.tuner.Record(tuner.Op{Start: t0.Sub(e.start), End: t1.Sub(e.start), Bytes: int64(n)})
		e.release(j.f)
	}
}

func (e *Engine) open(f *file) error {
	f.once.Do(func() {
		sf, err := os.Open(f.src)
		if err != nil {
			f.openErr = err
			return
		}
		df, err := os.OpenFile(f.tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
		if err != nil {
			sf.Close()
			f.openErr = err
			return
		}
		if !e.opt.Cached {
			noCache(sf)
			noCache(df)
		}
		f.sf, f.df = sf, df
	})
	return f.openErr
}

func (e *Engine) copyChunk(ctx context.Context, j job, buf *[]byte) int {
	f := j.f
	if f.failed.Load() {
		return 0
	}
	if err := e.open(f); err != nil {
		f.fail("open", err)
		return 0
	}
	if j.n == 0 {
		return 0
	}
	b := alignedBuf(buf, j.n)
	var n int
	var err error
	for attempt := 1; ; attempt++ {
		n, err = e.readAt(f, b, j.off)
		if n == j.n {
			err = nil
			break
		}
		if err == nil || errors.Is(err, io.EOF) {
			err = fmt.Errorf("file shrank to %d bytes while being copied (was %d)", j.off+int64(n), f.size)
			break
		}
		if attempt >= e.opt.Retries || ctx.Err() != nil {
			err = fmt.Errorf("%w (%d tries)", err, attempt)
			break
		}
		e.retries.Add(1)
		select {
		case <-time.After(time.Duration(attempt) * 250 * time.Millisecond):
		case <-ctx.Done():
		}
	}
	e.readBytes.Add(int64(n))
	if err != nil {
		f.fail(fmt.Sprintf("read at byte %d", j.off), err)
		return 0
	}
	if _, err := f.df.WriteAt(b, j.off); err != nil {
		f.fail("write", err)
		return 0
	}
	now := time.Now()
	f.meter.Add(int64(n), now)
	e.global.Add(int64(n), now)
	f.done.Add(int64(n))
	e.bytesDone.Add(int64(n))
	return n
}

func (e *Engine) readAt(f *file, b []byte, off int64) (int, error) {
	if e.readFault != nil {
		if err := e.readFault(f.show, off); err != nil {
			return 0, err
		}
	}
	if e.opt.Sim != nil {
		return e.opt.Sim.ReadAt(f.show, f.size, f.sf, b, off)
	}
	return f.sf.ReadAt(b, off)
}

// alignedBuf returns n bytes of *buf, growing it as needed, starting on a
// 16 KiB boundary so uncached I/O can go straight to the disk.
func alignedBuf(buf *[]byte, n int) []byte {
	const align = 16 << 10
	if cap(*buf) < n {
		size := 1 << 16
		for size < n {
			size <<= 1
		}
		raw := make([]byte, size+align)
		off := 0
		if r := int(uintptr(unsafePointer(raw)) % align); r != 0 {
			off = align - r
		}
		*buf = raw[off : off+size]
	}
	return (*buf)[:n]
}

func (e *Engine) release(f *file) {
	if f.refs.Add(-1) == 0 {
		e.finish(f)
	}
}

func (e *Engine) finish(f *file) {
	defer func() {
		e.mu.Lock()
		delete(e.active, f)
		e.mu.Unlock()
	}()
	if f.sf != nil {
		f.sf.Close()
	}
	failed, canceled := f.failed.Load(), f.canceled.Load()
	if !failed && !canceled && f.df != nil {
		if err := e.install(f); err != nil {
			f.fail("finish", err)
			failed = true
		}
	} else if f.df != nil {
		f.df.Close()
		os.Remove(f.tmp) // tclone's own partial file
	}
	switch {
	case failed:
		f.errMu.Lock()
		e.logErr(f.failOp, f.src, f.failErr)
		f.errMu.Unlock()
		e.filesFailed.Add(1)
		e.bytesDone.Add(-f.done.Load())
		e.bytesFailed.Add(f.size)
	case canceled:
		e.bytesDone.Add(-f.done.Load())
	default:
		e.filesDone.Add(1)
	}
}

// install gives the finished temporary file its mode and times and renames it into place.
func (e *Engine) install(f *file) error {
	err := f.df.Chmod(f.mode.Perm() | f.mode&(fs.ModeSetuid|fs.ModeSetgid|fs.ModeSticky))
	if cerr := f.df.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Chtimes(f.tmp, f.mtime, f.mtime)
	}
	if err == nil {
		err = os.Rename(f.tmp, f.dst)
	}
	if err != nil {
		os.Remove(f.tmp)
	}
	return err
}

// fixDirs gives directories their source modes and times, deepest first, once
// nothing more will be written inside them.
func (e *Engine) fixDirs() {
	e.mu.Lock()
	dirs := e.dirs
	e.mu.Unlock()
	for i := len(dirs) - 1; i >= 0; i-- {
		d := dirs[i]
		if err := os.Chmod(d.dst, d.mode.Perm()); err != nil {
			e.logErr("chmod", d.dst, err)
		}
		if err := os.Chtimes(d.dst, d.mtime, d.mtime); err != nil {
			e.logErr("times", d.dst, err)
		}
	}
}

func (e *Engine) logErr(op, path string, err error) {
	if e.opt.Errors != nil {
		e.opt.Errors.Log(op, path, err)
	}
}

// control steps the tuner ten times a second and polls the disks.
func (e *Engine) control(stop <-chan struct{}) {
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-stop:
			return
		case now := <-tick.C:
			e.pollDevices(now, false)
			p := tuner.Probe{
				Now:            now.Sub(e.start),
				OldestInflight: e.oldest(now),
				Starved:        e.starved(now),
				DevReadBytes:   -1,
				OurReadBytes:   e.readBytes.Load(),
			}
			if e.srcDev != nil && e.opt.Sim == nil && !e.opt.Cached {
				if s, err := e.srcDev.src.Stats(); err == nil {
					p.DevReadBytes = int64(s.ReadBytes)
				}
			}
			s := e.tuner.Step(p)
			e.gate.setLimit(s.Readers)
			e.chunk.Store(int64(s.Chunk))
			if r, ok := e.global.Rate(now); ok {
				for {
					old := e.peak.Load()
					if r <= float64frombits(old) || e.peak.CompareAndSwap(old, float64bits(r)) {
						break
					}
				}
			}
		}
	}
}

// starved: the readers can't be kept busy for a whole tuning round, either
// because the walk is behind the copy or because the copy is nearly done.
func (e *Engine) starved(now time.Time) bool {
	if e.dispatchIdle.Load() && !e.dispatchDone.Load() && len(e.jobs) < int(e.gate.limitNow()) {
		return true
	}
	if e.scanning.Load() {
		return false
	}
	left := e.bytesTotal.Load() - e.bytesDone.Load() - e.bytesFailed.Load()
	r, ok := e.global.Rate(now)
	return ok && float64(left) < r*20
}

func (e *Engine) oldest(now time.Time) time.Duration {
	o := now.UnixNano()
	for i := range e.opStart {
		if s := e.opStart[i].Load(); s != 0 && s < o {
			o = s
		}
	}
	return time.Unix(0, o).Sub(e.start)
}

// pollDevices refreshes each disk's rates about once a second. Like the main
// rate, a disk's rate is measured back from now over the shortest span (at
// least a second) in which its counters moved, so a disk that is slowly
// grinding through error correction shows a small rate, not zero. Only a disk
// that did nothing for two minutes shows as idle.
func (e *Engine) pollDevices(now time.Time, force bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, w := range e.devs {
		if !force && now.Sub(w.prevAt) < time.Second {
			continue
		}
		s, err := w.src.Stats()
		if err != nil {
			w.view.OK = false
			continue
		}
		w.prevAt = now
		if !w.haveFirst {
			w.first, w.haveFirst = s, true
		}
		w.hist = append(w.hist, devSample{at: now, s: s})
		for len(w.hist) > 2 && now.Sub(w.hist[1].at) > devHistory {
			w.hist = w.hist[1:]
		}
		w.view.Errors = (s.ReadErrors - w.first.ReadErrors) + (s.WriteErrors - w.first.WriteErrors)
		w.view.Retries = (s.ReadRetries - w.first.ReadRetries) + (s.WriteRetries - w.first.WriteRetries)
		if len(w.hist) < 2 {
			continue
		}
		w.view.OK = true
		w.view.Idle = true
		for k := len(w.hist) - 2; k >= 0; k-- {
			a := w.hist[k]
			if now.Sub(a.at) >= time.Second && w.moved(a.s, s) {
				w.view.Rates = devstat.Diff(a.s, s, now.Sub(a.at))
				w.view.Window = now.Sub(a.at)
				w.view.Idle = false
				break
			}
		}
	}
}

// Snapshot returns the current state for the display.
func (e *Engine) Snapshot() Snapshot {
	now := time.Now()
	s := Snapshot{
		Now: now, Start: e.start,
		Scanning:     e.scanning.Load(),
		FilesTotal:   e.filesTotal.Load(),
		FilesDone:    e.filesDone.Load(),
		FilesSkipped: e.filesSkipped.Load(),
		FilesFailed:  e.filesFailed.Load(),
		Links:        e.links.Load(),
		Dirs:         e.dirsSeen.Load(),
		BytesTotal:   e.bytesTotal.Load(),
		BytesDone:    e.bytesDone.Load(),
		BytesSkipped: e.bytesSkipped.Load(),
		BytesFailed:  e.bytesFailed.Load(),
		Retries:      e.retries.Load(),
		Tuner:        e.tuner.Status(now.Sub(e.start)),
		Canceled:     e.canceled.Load(),
		Done:         e.done.Load(),
		Peak:         float64frombits(e.peak.Load()),
	}
	s.Rate, s.RateOK = e.global.Rate(now)
	s.Avg, _ = e.global.Average(now)
	s.Idle = e.global.Idle(now)
	if e.opt.Errors != nil {
		s.Errors = e.opt.Errors.Count()
		s.ErrorFile = e.opt.Errors.Path()
		s.LastError = e.opt.Errors.Last()
	}
	e.mu.Lock()
	for f := range e.active {
		a := Active{Show: f.show, Size: f.size, Done: f.done.Load(), Age: now.Sub(f.started)}
		if f.meter != nil {
			a.Rate, a.RateOK = f.meter.Rate(now)
		}
		s.Active = append(s.Active, a)
	}
	for _, w := range e.devs {
		s.Devices = append(s.Devices, w.view)
	}
	e.mu.Unlock()
	sort.Slice(s.Active, func(i, j int) bool { return s.Active[i].Age > s.Active[j].Age })
	return s
}

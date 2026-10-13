// Package copier is qclone's engine.
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
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"qclone/internal/devstat"
	"qclone/internal/plan"
	"qclone/internal/rate"
	"qclone/internal/sim"
	"qclone/internal/tuner"
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
	Rescue       bool        // install files with unreadable ranges, zeros there
	Order        plan.Order  // the order files are copied in
}

// hashWindow is how many found files hash order chooses among.
const hashWindow = 1024

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
	Idle    bool          // the disk did nothing qclone cares about for the whole history kept
	Window  time.Duration // what the rates are averaged over; when Idle, how long it has been idle
	Errors  uint64        // read+write errors since qclone started
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
	BytesResumed                                     int64 // carried over from earlier runs' partial files
	BytesZeroed                                      int64 // zeros written for unreadable ranges (--rescue)
	BadRanges, BadBytes, BadFiles, FilesRescued      int64 // unreadable ranges found, and files installed with zeros
	Rescue                                           bool
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
	inflight            atomic.Int64 // chunks handed out and not yet finished
	key, seq            uint64       // the queue's order: path hash (or seq), then the order found

	once    sync.Once
	sf, df  *os.File
	openErr error

	errMu    sync.Mutex
	failOp   string
	failErr  error
	failed   atomic.Bool
	canceled atomic.Bool

	// Rescue (rescue.go). base is the part's name without ".part" when it can
	// be resumed: the plain claim, not a numbered one.
	base     string
	patch    ranges // set when re-reading the zero-filled ranges of a rescued file
	resuming bool   // the part already holds doneSet from an earlier run
	rangeMu  sync.Mutex
	doneSet  ranges
	badSet   ranges
	dirty    bool
	ioMu     sync.Mutex // guards sf, df and closed against the map flusher
	closed   bool
	flushed  time.Time
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

// moved reports whether the counters qclone cares about for this role changed.
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
	files  atomic.Int64  // files to keep in flight
	slots  chan struct{} // nudges the dispatcher when a chunk finishes
	seq    atomic.Uint64
	global *rate.Meter
	peak   atomic.Uint64 // float64 bits

	filesTotal, filesDone, filesSkipped, filesFailed atomic.Int64
	links, dirsSeen                                  atomic.Int64
	bytesTotal, bytesDone, bytesSkipped, bytesFailed atomic.Int64
	readBytes, retries                               atomic.Int64
	bytesResumed, bytesZeroed, filesRescued          atomic.Int64
	badRanges, badBytes, badFiles                    atomic.Int64
	scanning, dispatchIdle, dispatchDone             atomic.Bool
	canceled, done                                   atomic.Bool

	q       fileQueue
	jobs    chan job
	opStart []atomic.Int64 // per worker: unix nanos of the op in flight, 0 when idle

	mu      sync.Mutex
	active  map[*file]struct{}
	dirs    []dirAttr
	seen    map[string]struct{}
	fold    bool            // the destination ignores letter case
	tmps    map[string]bool // temporary names in use this run (folded)
	scanEnd chan struct{}
	devs    []*devWatch
	srcDev  *devWatch // the one source disk, when there is exactly one

	readFault func(show string, off int64, n int) error // tests only
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
		tmps:   map[string]bool{},
		fold:   !caseSensitive(existingAncestor(p.Dst)),
		jobs:   make(chan job), // unbuffered: each chunk is sized at hand-off, so a new chunk size applies at once
	}
	e.opStart = make([]atomic.Int64, opt.Tuner.MaxReaders)
	s := e.tuner.Setting()
	e.gate.setLimit(s.Readers)
	e.chunk.Store(int64(s.Chunk))
	e.files.Store(int64(s.Files))
	e.slots = make(chan struct{}, 1)
	window := 1
	if opt.Order == plan.OrderHash {
		window = hashWindow
	}
	e.q.init(window)
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
	e.scanEnd = make(chan struct{})
	go func() {
		defer close(e.scanEnd)
		e.scan(ctx)
	}()
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
	flushDone := make(chan struct{})
	go func() {
		defer close(flushDone)
		tick := time.NewTicker(flushEvery / 4)
		defer tick.Stop()
		for {
			select {
			case <-stop:
				return
			case now := <-tick.C:
				e.flushMaps(now)
			}
		}
	}()
	wg.Wait()
	<-e.scanEnd // nothing may log after Run returns
	close(stop)
	<-ctlDone
	<-flushDone
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
	plan.Walk(ctx, e.plan, e.opt.Order != plan.OrderDirent, func(en plan.Entry) bool {
		if en.Err != nil {
			e.logErr("scan", en.Src, en.Err)
			return false
		}
		m := en.Info.Mode()
		switch {
		case m.IsDir():
			if !e.prepareDir(en) {
				return false
			}
			e.dirsSeen.Add(1)
			e.mu.Lock()
			e.dirs = append(e.dirs, dirAttr{dst: en.Dst, mode: m, mtime: en.Info.ModTime()})
			e.mu.Unlock()
		case m.IsRegular():
			if e.seen != nil {
				key := e.key(en.Dst)
				e.mu.Lock()
				_, dup := e.seen[key]
				e.seen[key] = struct{}{}
				e.mu.Unlock()
				if dup {
					e.logErr("skip", en.Src, fmt.Errorf("an earlier source already copies to %s", en.Dst))
					return true
				}
			}
			size := en.Info.Size()
			f := &file{show: en.Show, src: en.Src, dst: en.Dst, size: size, mode: m, mtime: en.Info.ModTime()}
			if fi, err := os.Lstat(en.Dst); err == nil && fi.Mode().IsRegular() && fi.Size() == size && near(fi.ModTime(), en.Info.ModTime(), e.opt.ModifyWindow) {
				// Unchanged, unless an earlier --rescue installed it with zeros.
				if f.patch = e.loadMarker(f); f.patch == nil {
					e.filesSkipped.Add(1)
					e.bytesSkipped.Add(size)
					return true
				}
			}
			e.filesTotal.Add(1)
			e.bytesTotal.Add(size)
			f.seq = e.seq.Add(1)
			f.key = f.seq
			if e.opt.Order == plan.OrderHash {
				h := fnv.New64a()
				io.WriteString(h, en.Show)
				f.key = h.Sum64()
			}
			e.q.push(f)
		case m&fs.ModeSymlink != 0:
			e.copyLink(en)
			return true
		default:
			e.logErr("skip", en.Src, fmt.Errorf("not copied: %s", kindOf(m)))
		}
		return true
	})
}

// prepareDir makes the destination directory for en, or checks the one that's
// there. Below the destination root it must be a real directory: a symlink
// there (say, from an earlier copy of a link) would lead writes outside the
// destination, so that subtree is skipped. It is made writable for the run;
// fixDirs gives it the source's mode at the end, so a read-only source
// directory doesn't block a rerun from retrying files that failed.
func (e *Engine) prepareDir(en plan.Entry) bool {
	if en.Rel != "" {
		if fi, err := os.Lstat(en.Dst); err == nil && !fi.IsDir() {
			e.logErr("mkdir", en.Dst, fmt.Errorf("exists and is not a directory (%s); not copying into it", fi.Mode().Type()))
			return false
		}
	}
	if err := os.MkdirAll(en.Dst, 0o755); err != nil {
		e.logErr("mkdir", en.Dst, err)
		return false
	}
	if fi, err := os.Stat(en.Dst); err == nil && fi.Mode().Perm()&0o700 != 0o700 {
		if err := os.Chmod(en.Dst, fi.Mode().Perm()|0o700); err != nil {
			e.logErr("chmod", en.Dst, err)
		}
	}
	return true
}

// key is how a destination path is compared with others: folded on a disk
// that ignores letter case.
func (e *Engine) key(p string) string {
	if e.fold {
		return strings.ToLower(p)
	}
	return p
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

// tmpBase is .qclone-<hash of the name> beside the destination; the part,
// its map and a rescue marker are this plus .part, .map and .bad.
func (e *Engine) tmpBase(dst string) string {
	h := fnv.New64a()
	io.WriteString(h, filepath.Base(dst))
	return filepath.Join(filepath.Dir(dst), fmt.Sprintf(".qclone-%012x", h.Sum64()&0xffffffffffff))
}

// claimTmp picks the hidden file a copy is written to before it's renamed
// into place. The plain name (base + ".part") is the same from run to run, so
// a rerun can resume from it or replace it. Within one run each claim is
// unique: if another file in flight already holds that name (two names that
// differ only by case, on a disk that ignores case), a numbered one is used,
// and base comes back empty: that copy can't be resumed.
func (e *Engine) claimTmp(dst string) (name, base string) {
	b := e.tmpBase(dst)
	e.mu.Lock()
	defer e.mu.Unlock()
	name = partPath(b)
	if !e.tmps[strings.ToLower(name)] {
		e.tmps[strings.ToLower(name)] = true
		return name, b
	}
	for n := 2; ; n++ {
		name = fmt.Sprintf("%s-%d.part", b, n)
		if !e.tmps[strings.ToLower(name)] {
			e.tmps[strings.ToLower(name)] = true
			return name, ""
		}
	}
}

func (e *Engine) releaseTmp(name string) {
	e.mu.Lock()
	delete(e.tmps, strings.ToLower(name))
	e.mu.Unlock()
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
	tmp, _ := e.claimTmp(en.Dst)
	defer e.releaseTmp(tmp)
	os.Remove(tmp) // a leftover of qclone's own, from an earlier run
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

// openFile is a file the dispatcher is cutting into chunks.
type openFile struct {
	f    *file
	todo ranges // what's left to hand out
	zero bool   // nothing to read, but one empty job must still go, so it's opened and finished
}

func (o *openFile) next(chunk int64) job {
	if o.zero {
		o.zero = false
		return job{f: o.f}
	}
	g := &o.todo[0]
	n := chunk
	if n > g.Len {
		n = g.Len
	}
	j := job{f: o.f, off: g.Off, n: int(n)}
	g.Off += n
	g.Len -= n
	if g.Len == 0 {
		o.todo = o.todo[1:]
	}
	return j
}

func (o *openFile) exhausted() bool { return !o.zero && len(o.todo) == 0 }

func (e *Engine) startFile(f *file) *openFile {
	f.started = time.Now()
	f.meter = rate.New(f.started)
	f.refs.Store(1) // the dispatcher's own reference
	e.mu.Lock()
	e.active[f] = struct{}{}
	e.mu.Unlock()
	e.prepare(f)
	todo := ranges{{0, f.size}}
	if f.resuming {
		f.rangeMu.Lock()
		todo = f.doneSet.gaps(f.size)
		f.rangeMu.Unlock()
	}
	o := &openFile{f: f, todo: todo}
	if todo.total() == 0 {
		o.todo, o.zero = nil, true
	}
	return o
}

// dispatch keeps the tuner's number of files open and hands out their chunks
// in turn. Each open file may have at most readers/files chunks in flight, so
// a file in a slow region can tie up only its share of the readers while the
// others keep moving; when it's the only file left, it may have them all.
func (e *Engine) dispatch(ctx context.Context) {
	var open []*openFile
	defer func() {
		for _, o := range open {
			o.f.canceled.Store(true)
			e.release(o.f)
		}
		e.dispatchDone.Store(true)
		close(e.jobs)
	}()
	rr, walkDone := 0, false
	for {
		if ctx.Err() != nil {
			return
		}
		for !walkDone && len(open) < int(e.files.Load()) {
			f, done := e.q.tryPop(time.Now())
			if f == nil {
				walkDone = done
				break
			}
			open = append(open, e.startFile(f))
		}
		if len(open) == 0 {
			if walkDone {
				return
			}
			e.dispatchIdle.Store(true)
			e.wait(ctx)
			e.dispatchIdle.Store(false)
			continue
		}
		limit := (e.gate.limitNow() + len(open) - 1) / len(open)
		var o *openFile
		for i := range open {
			k := (rr + i) % len(open)
			if open[k].f.inflight.Load() < int64(limit) {
				o, rr = open[k], k+1
				break
			}
		}
		if o == nil {
			e.wait(ctx) // every open file has its share in flight
			continue
		}
		j := o.next(e.chunk.Load())
		o.f.refs.Add(1)
		o.f.inflight.Add(1)
		select {
		case e.jobs <- j:
		case <-ctx.Done():
			o.f.refs.Add(-1)
			o.f.inflight.Add(-1)
			return
		}
		if o.exhausted() || o.f.failed.Load() || o.f.canceled.Load() {
			for k := range open {
				if open[k] == o {
					open = append(open[:k], open[k+1:]...)
					break
				}
			}
			e.release(o.f)
		}
	}
}

// wait sleeps until a chunk finishes, a file arrives, or a moment passes.
func (e *Engine) wait(ctx context.Context) {
	t := time.NewTimer(windowPatience)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-e.q.kick:
	case <-e.slots:
	case <-t.C:
	}
}

// finished tells the dispatcher a chunk of f is done, freeing its share.
func (e *Engine) finished(f *file) {
	f.inflight.Add(-1)
	select {
	case e.slots <- struct{}{}:
	default:
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
			e.finished(j.f)
			e.release(j.f)
			continue
		}
		t0 := time.Now()
		// Monotonic, from the run's start (+1 so 0 means idle): the wall
		// clock can jump, and stands still neither while the Mac sleeps.
		e.opStart[id].Store(int64(t0.Sub(e.start)) + 1)
		n, clean := e.copyChunk(ctx, j, &buf)
		t1 := time.Now()
		// Record before the op stops counting as in flight, so no window
		// can be closed without it. A chunk that hit unreadable sectors says
		// nothing about the setting, so the tuner doesn't see it.
		if clean {
			e.tuner.Record(tuner.Op{Start: t0.Sub(e.start), End: t1.Sub(e.start), Bytes: int64(n)})
		}
		e.opStart[id].Store(0)
		e.gate.release()
		e.finished(j.f)
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
		var df *os.File
		if f.resuming {
			df, err = os.OpenFile(f.tmp, os.O_WRONLY, 0)
		} else {
			df, err = os.OpenFile(f.tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
			if errors.Is(err, fs.ErrExist) {
				// Not in use this run (the claim says so): qclone's own leftover.
				os.Remove(f.tmp)
				df, err = os.OpenFile(f.tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
			}
		}
		if err != nil {
			sf.Close()
			f.openErr = err
			return
		}
		if !e.opt.Cached {
			noCache(sf)
			noCache(df)
		}
		f.ioMu.Lock()
		f.sf, f.df = sf, df
		f.ioMu.Unlock()
	})
	return f.openErr
}

// copyChunk copies one chunk. clean is false when it ran into unreadable
// sectors (and was salvaged piece by piece).
func (e *Engine) copyChunk(ctx context.Context, j job, buf *[]byte) (n int, clean bool) {
	f := j.f
	if f.failed.Load() || f.canceled.Load() {
		return 0, true
	}
	if err := e.open(f); err != nil {
		f.fail("open", err)
		return 0, true
	}
	if j.n == 0 {
		return 0, true
	}
	b := alignedBuf(buf, j.n)
	var err error
	shrank := false
	for attempt := 1; ; attempt++ {
		n, err = e.readAt(f, b, j.off)
		if n == j.n {
			err = nil
			break
		}
		if err == nil || errors.Is(err, io.EOF) {
			shrank = true
			err = fmt.Errorf("file shrank to %d bytes while being copied (was %d)", j.off+int64(n), f.size)
			break
		}
		if ctx.Err() != nil {
			f.canceled.Store(true)
			return 0, true
		}
		if attempt >= e.opt.Retries {
			break
		}
		e.retries.Add(1)
		select {
		case <-time.After(time.Duration(attempt) * 250 * time.Millisecond):
		case <-ctx.Done():
			f.canceled.Store(true)
			return 0, true
		}
	}
	e.readBytes.Add(int64(n))
	if shrank {
		f.fail(fmt.Sprintf("read at byte %d", j.off), err)
		return 0, true
	}
	if err != nil {
		good := e.salvage(f, func() bool { return ctx.Err() != nil }, b, j.off)
		e.counted(f, good)
		return good, false
	}
	if _, err := f.df.WriteAt(b, j.off); err != nil {
		f.fail("write", err)
		return 0, true
	}
	e.addDone(f, j.off, int64(n))
	e.counted(f, n)
	return n, true
}

// counted adds n freshly copied bytes to the meters and totals.
func (e *Engine) counted(f *file, n int) {
	if n <= 0 {
		return
	}
	now := time.Now()
	f.meter.Add(int64(n), now)
	e.global.Add(int64(n), now)
	f.done.Add(int64(n))
	e.bytesDone.Add(int64(n))
}

func (e *Engine) readAt(f *file, b []byte, off int64) (int, error) {
	if e.readFault != nil {
		if err := e.readFault(f.show, off, len(b)); err != nil {
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
	f.ioMu.Lock()
	defer f.ioMu.Unlock()
	f.closed = true
	if f.sf != nil {
		f.sf.Close()
	}
	f.rangeMu.Lock()
	bad := f.badSet.clone()
	f.rangeMu.Unlock()
	failed, canceled := f.failed.Load(), f.canceled.Load()
	// A file re-reading the holes of an earlier rescue is installed whatever
	// it found: it can only have fewer holes than the one it replaces.
	rescue := len(bad) > 0 && (e.opt.Rescue || f.patch != nil)
	kept := false
	switch {
	case failed || f.df == nil:
		e.discard(f)
	case canceled || (len(bad) > 0 && !rescue):
		kept = e.keepPart(f)
	default:
		if err := e.install(f, bad); err != nil {
			f.fail("finish", err)
			failed = true
		}
	}
	if f.tmp != "" {
		e.releaseTmp(f.tmp)
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
	case len(bad) > 0 && !rescue:
		next := "rerun to retry them"
		if kept {
			next = "the bytes that read fine are kept for the next run, which retries only these ranges"
		}
		e.logErr("read", f.src, fmt.Errorf("unreadable: %s (%s, %d bytes); not copied: %s, or add --rescue to finish the file with zeros there", bad.describe(32), count(len(bad), "range"), bad.total(), next))
		e.filesFailed.Add(1)
		e.bytesDone.Add(-f.done.Load())
		e.bytesFailed.Add(f.size)
	case len(bad) > 0:
		e.logErr("read", f.src, fmt.Errorf("unreadable: %s (%s, %d bytes); installed with zeros there (--rescue); a later run tries these ranges again", bad.describe(32), count(len(bad), "range"), bad.total()))
		e.filesDone.Add(1)
		e.filesRescued.Add(1)
		e.bytesDone.Add(bad.total()) // the file is finished, holes and all
		e.bytesZeroed.Add(bad.total())
	default:
		e.filesDone.Add(1)
	}
}

// discard removes a part that can't be used, and its map.
func (e *Engine) discard(f *file) {
	if f.df != nil {
		f.df.Close()
	}
	if f.tmp != "" {
		os.Remove(f.tmp) // qclone's own partial file
	}
	if f.base != "" {
		os.Remove(mapPath(f.base))
	}
}

// keepPart saves a stopped file's progress for the next run, when there is
// any worth keeping and the part can be found again; otherwise it discards it.
func (e *Engine) keepPart(f *file) bool {
	f.rangeMu.Lock()
	have := f.doneSet.total()
	f.rangeMu.Unlock()
	if f.base == "" || have == 0 || f.df == nil {
		e.discard(f)
		return false
	}
	if err := e.saveProgress(f); err != nil {
		e.discard(f)
		return false
	}
	f.df.Close()
	return true
}

// install gives the finished temporary file its mode and times and renames
// it into place. With unreadable ranges (--rescue), it zero-fills them and
// leaves a .bad marker naming them, written before the rename so the file is
// never in place without it.
func (e *Engine) install(f *file, bad ranges) error {
	marker := markerPath(e.tmpBase(f.dst))
	err := zeroFill(f.df, bad)
	if err == nil {
		if fi, serr := f.df.Stat(); serr == nil && fi.Size() != f.size {
			err = f.df.Truncate(f.size)
		}
	}
	if err == nil {
		err = f.df.Chmod(f.mode.Perm() | f.mode&(fs.ModeSetuid|fs.ModeSetgid|fs.ModeSticky))
	}
	if err == nil {
		// Data reaches the disk before the name does, so a crash can't leave a
		// complete-looking file (right size and time, so skipped next run)
		// with missing data. Plain fsync, not F_FULLFSYNC: the cost stays small.
		err = syscall.Fsync(int(f.df.Fd()))
	}
	if cerr := f.df.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Chtimes(f.tmp, f.mtime, f.mtime)
	}
	if err == nil && len(bad) > 0 {
		err = saveMap(marker, &mapFile{V: 1, Name: filepath.Base(f.dst), Size: f.size, MTime: f.mtime.UnixNano(), Done: bad.gaps(f.size).pairs(), Bad: bad.pairs()})
	}
	if err == nil {
		err = os.Rename(f.tmp, f.dst)
	}
	if err != nil {
		os.Remove(f.tmp)
		if f.base != "" {
			os.Remove(mapPath(f.base))
		}
		return err
	}
	if len(bad) == 0 {
		os.Remove(marker) // whole now
	}
	if f.base != "" {
		os.Remove(mapPath(f.base))
	}
	return nil
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
			e.files.Store(int64(s.Files))
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
	return ok && float64(left) < r*10
}

func (e *Engine) oldest(now time.Time) time.Duration {
	o := int64(now.Sub(e.start))
	for i := range e.opStart {
		if s := e.opStart[i].Load(); s != 0 && s-1 < o {
			o = s - 1
		}
	}
	return time.Duration(o)
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
		w.view.Window = now.Sub(w.hist[0].at) // idle this long, unless movement is found below
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
		BytesResumed: e.bytesResumed.Load(),
		BytesZeroed:  e.bytesZeroed.Load(),
		BadRanges:    e.badRanges.Load(),
		BadBytes:     e.badBytes.Load(),
		BadFiles:     e.badFiles.Load(),
		FilesRescued: e.filesRescued.Load(),
		Rescue:       e.opt.Rescue,
		Peak:         float64frombits(e.peak.Load()),
	}
	s.Rate, s.RateOK = e.global.Rate(now)
	s.Avg, _ = e.global.Average(now)
	// The peak is sampled ten times a second; a run shorter than that, or the
	// moment now, may be higher.
	s.Peak = math.Max(s.Peak, math.Max(s.Rate, s.Avg))
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

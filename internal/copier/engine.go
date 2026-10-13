// Package copier is qclone's engine.
//
// One goroutine walks the sources and queues files (copying can start before
// the walk ends). A pool of workers goes through a gate; each worker that gets
// through asks a dispatcher for a chunk, reads it, and writes it at the same
// offset in a hidden temporary file beside the destination. So a big file is
// read by several workers at once and many small files are copied side by
// side. When a file's last chunk lands, the temporary file gets the source's
// mode and times and is renamed into place, so a destination file is either
// the old one or a complete new one, never half-written.
//
// Files are cut into regions (32 MiB), each judged on its own: unknown until a
// read of it comes back, then fresh while its reads take less than StuckAfter
// (50 ms), or slow, and quarantined, once one doesn't. Reads in flight never
// exceed the queue length Q, measured at the start (calibrate.go). Within it,
// pick hands out, in order: up to R reads of slow regions (the slow lane, R
// measured, at most Q - 4); up to W flying reads of fresh regions (W tuned);
// when there's no fresh work, tries of unknown regions, breadth first; and
// until something fresh turns up, more slow reads in every slot left. Once
// fresh work exists, slow reads back off to R as they finish.
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
	Readers      int           // flying reads at the start (W)
	Tuner        tuner.Config  // for W; Tuner.MaxReaders is an optional limit
	SlowLane     int           // R at the start; R is measured unless SlowFixed
	SlowFixed    bool          // keep R at SlowLane (0: no slow lane)
	Queue        int           // Q, the most reads in flight; 0: measured at the start, else 32
	NoLanes      bool          // read every region alike, in order, Readers at a time (rclone's way, to compare)
	StuckAfter   time.Duration // a read in flight this long is slow (0: 50 ms)
	Chunk        int           // bytes per read (0: 256 KiB)
	RegionSize   int64         // 0: 32 MiB
	Fixed        bool          // never change W or R
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

const (
	defaultStuckAfter = 50 * time.Millisecond
	defaultChunk      = 256 << 10
	defaultRegion     = 32 << 20
	defaultQueue      = 32                     // a SATA drive's command queue, when nothing better is known
	maxQueue          = 256                    // workers in the pool
	fastReserve       = 4                      // R is at most Q - 4: slots always left for fresh reads
	controlTick       = 20 * time.Millisecond  // marking stuck reads, stepping the tuners
	dispatchTick      = 20 * time.Millisecond  // the dispatcher looks again at least this often
	starvedAfter      = 100 * time.Millisecond // a lane short of work this long: nothing to measure
)

// Lanes a read can go out in.
const (
	laneFly  = iota // a fresh region's chunk: one of W
	laneSlow        // a slow region's chunk: counted as stuck from the start
	laneTry         // the first read of an unknown region
)

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
	SlowTuner                                        tuner.Status // R; Mode "off" when there's no slow lane
	Queue                                            int          // Q
	QueueHow                                         string       // "measured", "set", or "default" (no fresh data to measure)
	SlowLane                                         int          // R
	InFly, InSlow, InTry                             int          // reads in flight in each lane
	Reading                                          int          // reads in flight now
	Stuck, PeakStuck                                 int          // reads in flight over StuckAfter (or in the slow lane), now and at most
	Quarantined                                      int          // slow regions with chunks waiting
	Chunk                                            int
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
	regionsLeft         int    // regions still with chunks to hand out (the dispatcher's own count)
	key, seq            uint64 // the queue's order: path hash (or seq), then the order found

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
	f    *file
	r    *region
	off  int64
	n    int
	lane int
}

// slot is one worker's read in flight, as the control loop sees it.
type slot struct {
	mu    sync.Mutex
	r     *region
	start time.Duration // from the run's start; < 0 when not reading
	lane  int
	stuck bool // counted in Engine.stuck and r.stuck now
	was   bool // counted as stuck at some point: not a flying read
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

	tuner      *tuner.Tuner // W
	slowTuner  *tuner.Tuner // R; nil when there's no slow lane or it's fixed
	gate       *gate
	stuckAfter time.Duration
	queue      atomic.Int64 // Q
	queueHow   atomic.Value // string
	flying     atomic.Int64 // W
	slowLane   atomic.Int64 // R
	slowOn     atomic.Bool  // the slow tuner is in use (Q leaves room for a slow lane)
	inLane     [3]atomic.Int64
	pending    atomic.Int64 // tries out that have neither come back nor gone stuck: maybe fresh
	stuck      atomic.Int64 // reads in flight now that count as stuck
	reading    atomic.Int64 // reads in flight now
	peakStuck  atomic.Int64
	quarantine atomic.Int64  // slow regions with chunks waiting
	flyShort   atomic.Int64  // since when W couldn't be filled for lack of fresh work (nanos from start + 1); 0: it could
	slowShort  atomic.Int64  // since when R wasn't in force or couldn't be filled; 0: it was
	nudge      chan struct{} // tells the dispatcher something changed
	seq        atomic.Uint64
	global     *rate.Meter
	peak       atomic.Uint64 // float64 bits

	filesTotal, filesDone, filesSkipped, filesFailed atomic.Int64
	links, dirsSeen                                  atomic.Int64
	bytesTotal, bytesDone, bytesSkipped, bytesFailed atomic.Int64
	readBytes, retries                               atomic.Int64
	bytesResumed, bytesZeroed, filesRescued          atomic.Int64
	badRanges, badBytes, badFiles                    atomic.Int64
	scanning, dispatchDone                           atomic.Bool
	canceled, done                                   atomic.Bool

	q         fileQueue
	want      chan chan job // a worker through the gate asks for a chunk
	quit      chan struct{} // closed when the dispatcher has nothing more
	workers   []slot
	rr        int // the dispatcher's round-robin position
	filesOpen int // files with regions still to hand out (the dispatcher's own count)

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
	if opt.Tuner.MaxReaders < 1 {
		opt.Tuner = tuner.DefaultConfig()
	}
	if opt.Chunk <= 0 {
		opt.Chunk = defaultChunk
	}
	if opt.RegionSize <= 0 {
		opt.RegionSize = defaultRegion
	}
	if opt.Queue > maxQueue {
		opt.Queue = maxQueue
	}
	if opt.SlowLane < 0 {
		opt.SlowLane = 0
	}
	now := time.Now()
	e := &Engine{
		plan:       p,
		opt:        opt,
		start:      now,
		tuner:      tuner.New(opt.Tuner, opt.Readers, opt.Fixed),
		stuckAfter: opt.StuckAfter,
		global:     rate.New(now),
		active:     map[*file]struct{}{},
		tmps:       map[string]bool{},
		fold:       !caseSensitive(existingAncestor(p.Dst)),
		nudge:      make(chan struct{}, 1),
		want:       make(chan chan job),
		quit:       make(chan struct{}),
	}
	if e.stuckAfter <= 0 {
		e.stuckAfter = defaultStuckAfter
	}
	e.workers = make([]slot, maxQueue)
	for i := range e.workers {
		e.workers[i].start = -1
	}
	e.gate = newGate(0)
	if !opt.NoLanes && !opt.Fixed && !opt.SlowFixed {
		start := opt.SlowLane
		if start < 1 {
			start = 4
		}
		e.slowTuner = tuner.New(slowTunerConfig(maxQueue), start, false)
	}
	switch {
	case opt.NoLanes:
		e.setQueue(e.tuner.Readers(), "set")
	case opt.Queue > 0:
		e.setQueue(opt.Queue, "set")
	default:
		e.setQueue(defaultQueue, "default") // until measured
	}
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
	for i := range e.workers {
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

// region is a stretch of a file, at most RegionSize long, that the dispatcher
// hands out chunk by chunk and judges on its own: unknown until a read of it
// has finished, then fresh while its reads come back fast, or slow
// (quarantined) once one doesn't. Data written together sits together and ages
// together, so one read is a good guess for the rest of a region, while a big
// file can hold old and fresh stretches side by side.
type region struct {
	f        *file
	todo     ranges // what's left to hand out
	zero     bool   // nothing to read, but one empty job must still go, so the file is opened and finished
	rank     int    // its place in the file's breadth-first descent: 0 is its start, then its middle, its quarters, ...
	inflight atomic.Int64
	stuck    atomic.Int64 // reads of it in flight now that count as stuck
	lastSlow atomic.Bool  // its last finished read took longer than StuckAfter
	proven   atomic.Bool  // a read of it has finished
}

func (r *region) next(chunk int64) job {
	if r.zero {
		r.zero = false
		return job{f: r.f, r: r}
	}
	g := &r.todo[0]
	n := chunk
	if n > g.Len {
		n = g.Len
	}
	j := job{f: r.f, r: r, off: g.Off, n: int(n)}
	g.Off += n
	g.Len -= n
	if g.Len == 0 {
		r.todo = r.todo[1:]
	}
	return j
}

func (r *region) exhausted() bool { return !r.zero && len(r.todo) == 0 }

// slow: a read of it is stuck now, or its last one took too long.
func (r *region) slow() bool { return r.stuck.Load() > 0 || r.lastSlow.Load() }

func slowRegion(r *region) bool  { return !r.exhausted() && r.slow() }
func freshRegion(r *region) bool { return !r.exhausted() && r.proven.Load() && !r.slow() }

// untried: no read of it has come back, and none is out now.
func untried(r *region) bool {
	return !r.exhausted() && !r.proven.Load() && r.inflight.Load() == 0 && r.stuck.Load() == 0
}

// startFile opens f for the dispatcher and cuts what's left of it into regions.
func (e *Engine) startFile(f *file) []*region {
	f.started = time.Now()
	f.meter = rate.New(f.started)
	f.refs.Store(1) // the dispatcher's own reference, until its last region is handed out
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
	var rs []*region
	if todo.total() == 0 {
		rs = []*region{{f: f, zero: true}}
	} else {
		size := e.opt.RegionSize
		byIndex := map[int64]*region{}
		for _, g := range todo {
			for off, end := g.Off, g.Off+g.Len; off < end; {
				i := off / size
				stop := min((i+1)*size, end)
				r := byIndex[i]
				if r == nil {
					r = &region{f: f}
					byIndex[i] = r
					rs = append(rs, r) // in offset order: todo is sorted
				}
				r.todo = append(r.todo, span{off, stop - off})
				off = stop
			}
		}
		for k, pos := range breadthFirst(len(rs)) {
			rs[pos].rank = k
		}
	}
	f.regionsLeft = len(rs)
	e.filesOpen++
	return rs
}

// breadthFirst lists 0..n-1 in a breadth-first descent: the start, then the
// middle, then the quarters, and so on, so the first few tries sample a whole
// file before any part of it is looked at closely.
func breadthFirst(n int) []int {
	if n == 0 {
		return nil
	}
	order := []int{0}
	seen := make([]bool, n)
	seen[0] = true
	queue := [][2]int{{0, n}}
	for len(queue) > 0 {
		lo, hi := queue[0][0], queue[0][1]
		queue = queue[1:]
		if hi-lo < 2 {
			continue
		}
		mid := (lo + hi) / 2
		if !seen[mid] {
			seen[mid] = true
			order = append(order, mid)
		}
		queue = append(queue, [2]int{lo, mid}, [2]int{mid, hi})
	}
	return order
}

// dispatch hands each worker that asks the chunk pick chooses at that moment.
func (e *Engine) dispatch(ctx context.Context) {
	var open []*region
	var waiting []chan job
	walkDone := false
	defer func() {
		released := map[*file]bool{}
		for _, r := range open {
			if !released[r.f] {
				released[r.f] = true
				r.f.canceled.Store(true)
				e.release(r.f)
			}
		}
		e.dispatchDone.Store(true)
		close(e.quit)
		for _, c := range waiting {
			close(c)
		}
	}()
	if e.queueHow.Load() == "default" {
		e.measureQueue(ctx, &open, &walkDone)
	}
	t := time.NewTimer(dispatchTick)
	defer t.Stop()
	var noted time.Time
	for {
		if ctx.Err() != nil {
			return
		}
		for len(waiting) > 0 {
			j, ok := e.pick(&open, &walkDone)
			if !ok {
				break
			}
			waiting[0] <- j
			waiting = waiting[1:]
		}
		if now := time.Now(); now.Sub(noted) >= dispatchTick/2 {
			noted = now
			e.note(open)
		}
		if walkDone && len(open) == 0 {
			return
		}
		t.Reset(dispatchTick)
		select {
		case <-ctx.Done():
		case r := <-e.want:
			waiting = append(waiting, r)
		case <-e.q.kick:
		case <-e.nudge:
		case <-t.C:
		}
	}
}

// pick chooses the next chunk for a worker that's ready, within Q reads in
// flight, in this order:
//
//  1. The slow lane: while fewer than R slow reads are out, a slow region's
//     next chunk. These R slots are kept for slow regions whenever they have
//     work, however many attempts are stuck elsewhere.
//  2. Flying reads: while fewer than W are out, a fresh region's next chunk.
//     When fresh work is waiting, nothing else goes out ahead of it: that's
//     the quarantine, and slow reads back off to R as they finish.
//  3. With no fresh work: a try, the first read of a region nobody has read
//     yet, breadth first (nextTry).
//  4. With no fresh work, nothing left to try, and no try still out that
//     might turn out fresh: more slow reads, in every slot left. Until
//     something fresh turns up, a full queue costs nothing and spares the
//     drive waiting on qclone between reads.
func (e *Engine) pick(open *[]*region, walkDone *bool) (job, bool) {
	inFly, inSlow, inTry := e.inLane[laneFly].Load(), e.inLane[laneSlow].Load(), e.inLane[laneTry].Load()
	if inFly+inSlow+inTry >= e.queue.Load() {
		return job{}, false
	}
	if e.opt.NoLanes {
		return e.pickInOrder(open, walkDone)
	}
	if inSlow < e.slowLane.Load() {
		if r := e.choose(*open, slowRegion); r != nil {
			return e.take(open, r, laneSlow), true
		}
	}
	if fresh := e.choose(*open, freshRegion); fresh != nil {
		if inFly < e.flying.Load() {
			return e.take(open, fresh, laneFly), true
		}
		return job{}, false
	}
	if r := e.nextTry(open, walkDone); r != nil {
		return e.take(open, r, laneTry), true
	}
	if inFly == 0 && e.pending.Load() == 0 {
		if r := e.choose(*open, slowRegion); r != nil {
			return e.take(open, r, laneSlow), true
		}
	}
	return job{}, false
}

// nextTry picks a region nobody has read yet, breadth first across files and
// within them: every open file's first region, then a new file's, then the
// next level down in the open files (their middles, then their quarters, ...).
// A region gets one try at a time. New files open while fewer than 2Q are open.
func (e *Engine) nextTry(open *[]*region, walkDone *bool) *region {
	var best *region
	for _, r := range *open {
		if untried(r) && (best == nil || r.rank < best.rank) {
			best = r
			if r.rank == 0 {
				break
			}
		}
	}
	if (best == nil || best.rank > 0) && !*walkDone && e.filesOpen < 2*int(e.queue.Load()) {
		f, done := e.q.tryPop(time.Now())
		if f != nil {
			rs := e.startFile(f)
			*open = append(*open, rs...)
			return rs[0]
		}
		*walkDone = done
	}
	return best
}

// pickInOrder reads the way rclone does, to compare: up to W files at once,
// each read one chunk at a time from start to end, in the walk's order.
func (e *Engine) pickInOrder(open *[]*region, walkDone *bool) (job, bool) {
	busy := map[*file]bool{}
	for _, r := range *open {
		if busy[r.f] {
			continue
		}
		busy[r.f] = true
		if r.inflight.Load() == 0 && !r.exhausted() {
			return e.take(open, r, laneFly), true
		}
	}
	if !*walkDone && e.filesOpen < int(e.flying.Load()) {
		f, done := e.q.tryPop(time.Now())
		if f != nil {
			rs := e.startFile(f)
			*open = append(*open, rs...)
			return e.take(open, rs[0], laneFly), true
		}
		*walkDone = done
	}
	return job{}, false
}

// note records, for the tuners and the display, whether each lane could be
// kept full and how many slow regions wait.
func (e *Engine) note(open []*region) {
	fresh, slow := false, 0
	for _, r := range open {
		switch {
		case freshRegion(r):
			fresh = true
		case slowRegion(r):
			slow++
		}
	}
	e.quarantine.Store(int64(slow))
	now := int64(time.Since(e.start)) + 1
	set := func(a *atomic.Int64, short bool) {
		if !short {
			a.Store(0)
		} else if a.Load() == 0 {
			a.Store(now)
		}
	}
	set(&e.flyShort, !fresh && e.inLane[laneFly].Load() < e.flying.Load())
	// R is in force only while fresh work holds slow reads down to it.
	set(&e.slowShort, !(fresh || e.inLane[laneFly].Load() > 0) || slow == 0)
}

// choose returns the next open region, in turn, that ok accepts.
func (e *Engine) choose(open []*region, ok func(*region) bool) *region {
	for i := range open {
		k := (e.rr + i) % len(open)
		if ok(open[k]) {
			e.rr = k + 1
			return open[k]
		}
	}
	return nil
}

// take cuts r's next chunk and hands it out in lane.
func (e *Engine) take(open *[]*region, r *region, lane int) job {
	j := r.next(int64(e.opt.Chunk))
	j.lane = lane
	r.f.refs.Add(1)
	r.inflight.Add(1)
	e.inLane[lane].Add(1)
	switch lane {
	case laneSlow:
		e.stick(r)
	case laneTry:
		e.pending.Add(1)
	}
	if r.exhausted() || r.f.failed.Load() || r.f.canceled.Load() {
		for k := range *open {
			if (*open)[k] == r {
				*open = append((*open)[:k], (*open)[k+1:]...)
				break
			}
		}
		if r.f.regionsLeft--; r.f.regionsLeft == 0 {
			e.filesOpen--
			e.release(r.f)
		}
	}
	return j
}

// setQueue sets Q, and with it the slow lane's ceiling, Q - 4, so a few slots
// are always left for fresh reads. A queue of 4 or less has no slow lane.
func (e *Engine) setQueue(q int, how string) {
	q = max(1, min(q, maxQueue))
	e.queue.Store(int64(q))
	e.queueHow.Store(how)
	e.gate.setLimit(q)
	e.tuner.SetMax(min(e.opt.Tuner.MaxReaders, q))
	e.flying.Store(int64(e.tuner.Readers()))
	rmax := q - fastReserve
	switch {
	case e.opt.NoLanes || rmax < 1:
		e.slowOn.Store(false)
		e.slowLane.Store(0)
	case e.slowTuner == nil:
		e.slowOn.Store(false)
		e.slowLane.Store(int64(min(e.opt.SlowLane, rmax)))
	default:
		e.slowTuner.SetMax(rmax)
		e.slowOn.Store(true)
		e.slowLane.Store(int64(e.slowTuner.Readers()))
	}
	e.poke()
}

// slowTunerConfig: R is judged on slow reads, which finish in lumps seconds
// apart, so each measurement needs longer and more of them.
func slowTunerConfig(max int) tuner.Config {
	return tuner.Config{
		MinReaders: 1, MaxReaders: max, Step: 4,
		Window:    3 * time.Second,
		MaxWindow: 30 * time.Second,
		MinOps:    20,
		Settle:    500 * time.Millisecond,
		Gain:      0.05,
		Recheck:   60 * time.Second,
	}
}

// stick counts one more read of r as stuck.
func (e *Engine) stick(r *region) {
	r.stuck.Add(1)
	n := e.stuck.Add(1)
	for {
		p := e.peakStuck.Load()
		if n <= p || e.peakStuck.CompareAndSwap(p, n) {
			break
		}
	}
	e.poke()
}

// unstick counts one read of r as no longer stuck: it ended.
func (e *Engine) unstick(r *region) {
	r.stuck.Add(-1)
	e.stuck.Add(-1)
	e.poke()
}

// poke tells the dispatcher to look again.
func (e *Engine) poke() {
	select {
	case e.nudge <- struct{}{}:
	default:
	}
}

// beginRead notes that worker id started reading a chunk of r at now;
// counted means the dispatcher already counts it as stuck.
func (e *Engine) beginRead(id int, r *region, lane int, now time.Duration) {
	counted := lane == laneSlow
	s := &e.workers[id]
	s.mu.Lock()
	s.r, s.lane, s.start, s.stuck, s.was = r, lane, now, counted, counted
	s.mu.Unlock()
	e.reading.Add(1)
}

// endRead notes that worker id's read is over, and reports whether it ever
// counted as stuck. Calling it again before the next beginRead changes nothing.
func (e *Engine) endRead(id int, now time.Duration) (wasStuck bool) {
	s := &e.workers[id]
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.start < 0 {
		return s.was
	}
	s.r.lastSlow.Store(now-s.start > e.stuckAfter)
	s.r.proven.Store(true)
	if s.stuck {
		e.unstick(s.r)
	}
	if s.lane == laneTry && !s.was {
		e.pending.Add(-1) // came back fast
	}
	s.start, s.stuck = -1, false
	e.reading.Add(-1)
	return s.was
}

// markStuck counts every read that has now been in flight longer than
// StuckAfter as stuck.
func (e *Engine) markStuck(now time.Duration) {
	for i := range e.workers {
		s := &e.workers[i]
		s.mu.Lock()
		if s.start >= 0 && !s.stuck && now-s.start > e.stuckAfter {
			s.stuck, s.was = true, true
			e.stick(s.r)
			if s.lane == laneTry {
				e.pending.Add(-1) // went stuck: not fresh
			}
		}
		s.mu.Unlock()
	}
}

// finished tells the dispatcher a chunk is done.
func (e *Engine) finished(j job) {
	j.r.inflight.Add(-1)
	e.inLane[j.lane].Add(-1)
	e.poke()
}

func (e *Engine) worker(ctx context.Context, id int) {
	var buf []byte
	reply := make(chan job, 1)
	for {
		e.gate.acquire()
		select {
		case e.want <- reply:
		case <-e.quit:
			e.gate.release()
			return
		}
		j, ok := <-reply
		if !ok {
			e.gate.release()
			return
		}
		// Monotonic, from the run's start: the wall clock can jump, and
		// stands still neither while the Mac sleeps.
		t0 := time.Now()
		e.beginRead(id, j.r, j.lane, t0.Sub(e.start))
		if ctx.Err() != nil {
			e.endRead(id, t0.Sub(e.start))
			e.gate.release()
			j.f.canceled.Store(true)
			e.finished(j)
			e.release(j.f)
			continue
		}
		n, clean := e.copyChunk(ctx, j, &buf, id)
		t1 := time.Now()
		stuck := e.endRead(id, t1.Sub(e.start))
		// Each tuner sees only its own lane's reads: W the flying reads that
		// stayed fast, R the slow lane's. A chunk that hit unreadable
		// sectors tells neither anything.
		op := tuner.Op{Start: t0.Sub(e.start), End: t1.Sub(e.start), Bytes: int64(n)}
		switch {
		case !clean:
		case j.lane == laneFly && !stuck:
			e.tuner.Record(op)
		case j.lane == laneSlow && e.slowTuner != nil:
			e.slowTuner.Record(op)
		}
		e.gate.release()
		e.finished(j)
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

// copyChunk copies one chunk for worker id. clean is false when it ran into
// unreadable sectors (and was salvaged piece by piece). The read counts as
// over (endRead) before the write, so a slow destination doesn't make reads
// look stuck.
func (e *Engine) copyChunk(ctx context.Context, j job, buf *[]byte, id int) (n int, clean bool) {
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
	e.endRead(id, time.Since(e.start))
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

// control marks stuck reads and steps the tuners every controlTick, samples
// the peak rate, and polls the disks.
func (e *Engine) control(stop <-chan struct{}) {
	tick := time.NewTicker(controlTick)
	defer tick.Stop()
	for i := 0; ; i++ {
		select {
		case <-stop:
			return
		case now := <-tick.C:
			at := now.Sub(e.start)
			e.markStuck(at)
			e.pollDevices(now, false)
			end := e.nearEnd(now)
			if w := e.tuner.Step(tuner.Probe{Now: at, Starved: end || e.short(&e.flyShort, at)}); int64(w) != e.flying.Load() {
				e.flying.Store(int64(w))
				e.poke()
			}
			if e.slowOn.Load() {
				if r := e.slowTuner.Step(tuner.Probe{Now: at, Starved: end || e.short(&e.slowShort, at)}); int64(r) != e.slowLane.Load() {
					e.slowLane.Store(int64(r))
					e.poke()
				}
			}
			if i%5 != 0 {
				continue
			}
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

// short: a lane has been short of work (since, from note) for starvedAfter.
func (e *Engine) short(since *atomic.Int64, at time.Duration) bool {
	s := since.Load()
	return s != 0 && at-time.Duration(s-1) >= starvedAfter
}

// nearEnd: less than 10 s of work left, too little to measure anything by.
func (e *Engine) nearEnd(now time.Time) bool {
	if e.scanning.Load() {
		return false
	}
	left := e.bytesTotal.Load() - e.bytesDone.Load() - e.bytesFailed.Load()
	r, ok := e.global.Rate(now)
	return ok && float64(left) < r*10
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
		Queue:        int(e.queue.Load()),
		QueueHow:     e.queueHow.Load().(string),
		SlowLane:     int(e.slowLane.Load()),
		InFly:        int(e.inLane[laneFly].Load()),
		InSlow:       int(e.inLane[laneSlow].Load()),
		InTry:        int(e.inLane[laneTry].Load()),
		Reading:      int(e.reading.Load()),
		Stuck:        int(e.stuck.Load()),
		PeakStuck:    int(e.peakStuck.Load()),
		Quarantined:  int(e.quarantine.Load()),
		Chunk:        e.opt.Chunk,
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
	switch {
	case e.slowOn.Load():
		s.SlowTuner = e.slowTuner.Status(now.Sub(e.start))
	case s.SlowLane > 0:
		s.SlowTuner = tuner.Status{Mode: "fixed", Readers: s.SlowLane}
	default:
		s.SlowTuner = tuner.Status{Mode: "off"}
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

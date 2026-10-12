// Package tuner decides how many reads to keep in flight, and how big each one
// is, by experiment.
//
// The difficulty on a degraded SSD is that the drive's own speed swings 250x
// between regions (2 MB/s in old, heavily error-corrected data; 500 MB/s in
// fresh data), which drowns out the effect of any one setting. So the tuner
// never compares a setting against the past. Each round runs short arms in the
// order A C A C A (A: the current setting, C: a candidate) and scores each C
// against the mean of the A arms on either side, which cancels any steady
// drift in the drive's speed. A round whose A arms disagree badly (the copy
// crossed into a different region) is thrown away.
//
// Throughput in an arm is measured "fluidly": each finished read+write is
// spread evenly over its own start..end interval, and an arm counts only the
// part that overlaps it, so the boundaries are exact even when single reads
// take seconds. An arm is scored only after every op begun during it has
// finished. It starts measuring two generations after the setting changes:
// first every op begun under the old setting must finish, then every op begun
// while those were still running. One generation is not enough. Ops issued
// during the switch sit behind a queue that is still growing or shrinking, so
// their intervals are the wrong length. The fluid count would then credit
// bytes still waiting in the drive's queue, which makes any deeper queue (more
// readers, bigger chunks) look 15-20% better than it is.
//
// The tuner also keeps a small memory of regions ("regimes") by speed and the
// best setting found in each, so on re-entering a slow region it jumps
// straight to what worked there last time instead of climbing again.
package tuner

import (
	"math"
	"sort"
	"sync"
	"time"
)

// Setting is what the tuner controls.
type Setting struct {
	Readers int // reads in flight at once
	Chunk   int // bytes per read
}

// Dimensions of the search.
const (
	DimReaders = 0
	DimChunk   = 1
)

// Config bounds the search. Times are wall-clock durations.
type Config struct {
	MinReaders, MaxReaders int
	MinChunk, MaxChunk     int
	TuneReaders, TuneChunk bool

	Warmup     time.Duration // before the first round
	MinMeasure time.Duration // shortest arm
	MaxMeasure time.Duration // longest arm
	MaxSettle  time.Duration // give up on a round if old ops take longer than this to drain
	MinRest    time.Duration // pause between rounds once converged (doubles each time)
	MaxRest    time.Duration
}

// DefaultConfig is what tclone uses.
func DefaultConfig() Config {
	return Config{
		MinReaders: 1, MaxReaders: 32,
		MinChunk: 128 << 10, MaxChunk: 8 << 20,
		TuneReaders: true, TuneChunk: true,
		Warmup:     2 * time.Second,
		MinMeasure: 1 * time.Second,
		MaxMeasure: 20 * time.Second,
		MaxSettle:  45 * time.Second,
		MinRest:    4 * time.Second,
		MaxRest:    60 * time.Second,
	}
}

// Op is one finished read+write, timed from the start of the run.
type Op struct {
	Start, End time.Duration
	Bytes      int64
}

// Probe is the engine's view at one control step.
type Probe struct {
	Now            time.Duration
	OldestInflight time.Duration // start of the oldest op still running; Now when none are
	Starved        bool          // the queue can't keep the readers busy (scan behind, or near the end)
	DevReadBytes   int64         // cumulative bytes the source device has read; < 0 when unknown
	OurReadBytes   int64         // cumulative bytes tclone has read
}

// Decision is one finished round, kept for the display.
type Decision struct {
	At       time.Duration
	Dim      int
	From, To Setting
	Gain     float64 // C over A, as a ratio minus one; NaN when the round didn't count
	Kept     bool    // the candidate became the setting
	Note     string  // "kept", "rejected", "jumped", "noisy", ...
}

// ArmRate is one arm of the round in progress.
type ArmRate struct {
	Setting Setting
	Rate    float64 // bytes/s; 0 while still measuring
	Final   bool
}

// RegimeView is one remembered region speed and its best setting.
type RegimeView struct {
	Rate    float64 // bytes/s at the best setting
	Best    Setting
	Current bool
}

// Status is a snapshot for the display.
type Status struct {
	Mode      string // "fixed", "warming up", "probing", "holding", "waiting for work"
	Applied   Setting
	Incumbent Setting
	Candidate Setting
	Dim       int
	Arms      []ArmRate
	History   []Decision
	Regimes   []RegimeView
	RestLeft  time.Duration
	Changes   int // how many times the setting moved
}

type phase int

const (
	phWarmup phase = iota
	phRest
	phRound
	phEval
)

type window struct {
	t0, t1     time.Duration // t1 < 0 while open
	bytes      float64
	dev0, our0 int64
	dev1, our1 int64
	final      bool
}

func (w *window) add(op Op) {
	if op.End <= op.Start {
		if op.End >= w.t0 && (w.t1 < 0 || op.End < w.t1) {
			w.bytes += float64(op.Bytes)
		}
		return
	}
	lo := op.Start
	if w.t0 > lo {
		lo = w.t0
	}
	hi := op.End
	if w.t1 >= 0 && w.t1 < hi {
		hi = w.t1
	}
	if hi <= lo {
		return
	}
	w.bytes += float64(op.Bytes) * float64(hi-lo) / float64(op.End-op.Start)
}

func (w *window) rate() float64 {
	d := (w.t1 - w.t0).Seconds()
	if d <= 0 {
		return 0
	}
	return w.bytes / d
}

// noisy reports whether the source device read much more than tclone did
// during the window: someone else was using the disk, so the arm is not a
// fair comparison.
func (w *window) noisy() bool {
	if w.dev0 < 0 || w.dev1 < 0 {
		return false
	}
	ours := float64(w.our1 - w.our0)
	extra := float64(w.dev1-w.dev0) - ours
	return extra > 0.5*ours+2e6*(w.t1-w.t0).Seconds()
}

type arm struct {
	s         Setting
	changedAt time.Duration
	drained   time.Duration // when the last op begun before changedAt finished; < 0 until then
	win       *window
}

// settled runs the two-generation rule; it returns true once measuring may start.
func (a *arm) settled(p Probe) bool {
	if a.drained < 0 {
		if p.OldestInflight < a.changedAt {
			return false
		}
		a.drained = p.Now
	}
	return p.OldestInflight >= a.drained
}

type dimState struct {
	dir       int // +1 or -1
	triedUp   bool
	triedDown bool
	converged bool
}

func (d *dimState) tried(dir int) bool {
	if dir > 0 {
		return d.triedUp
	}
	return d.triedDown
}

func (d *dimState) setTried(dir int) {
	if dir > 0 {
		d.triedUp = true
	} else {
		d.triedDown = true
	}
}

func (d *dimState) reset() { d.triedUp, d.triedDown, d.converged = false, false, false }

type regime struct {
	level float64 // log bytes/s at best
	best  Setting
}

// Thresholds, as log ratios.
var (
	upGain      = math.Log(1.05) // more readers must buy 5%
	downLoss    = math.Log(1.02) // fewer readers may cost up to 2%
	downWorst   = math.Log(1.06) // ...and no single arm may be 6% worse
	chunkGain   = math.Log(1.03) // a bigger chunk must buy 3%
	chunkLoss   = math.Log(1.02) // a smaller chunk may cost up to 2% (finer progress, faster rounds)
	driftLimit  = math.Log(2.5)  // A arms this far apart: the drive changed region mid-round
	regimeShift = math.Log(2.5)  // steady speed this far from the last round: a new region
	regimeMatch = math.Log(12)   // a remembered region this close is the same one
	earlyGap    = math.Log(1.25) // A C A already shows 25% either way: decide now
	slowOp      = 0.5            // seconds per read above which the chunk is not grown
	earlyStable = math.Log(1.2)  // ...provided the two A arms agree within 20%
)

// Tuner is safe for concurrent use: Record from the workers, Step from one control loop.
type Tuner struct {
	mu    sync.Mutex
	cfg   Config
	fixed bool

	cur, out    Setting
	lastApplied time.Duration
	phase       phase

	dims      [2]dimState
	preferDim int
	dim, dir  int
	cand      Setting
	arms      []*arm
	armIdx    int
	earlyDone bool
	roundAt   time.Duration
	explore   bool // double readers per step until the first overshoot

	open []*window // windows still taking op attribution

	opsDone   int64
	latEWMA   float64 // seconds per op
	bytesEWMA float64 // bytes per op

	restUntil time.Duration
	restDur   time.Duration
	monitor   *window
	strikes   int
	baseline  float64 // log bytes/s at the incumbent, from the last round; NaN before
	streak    int     // inconclusive rounds in a row

	regimes []*regime
	curReg  *regime

	history []Decision
	changes int
}

// New returns a tuner starting at start. With fixed set, it never moves.
func New(cfg Config, start Setting, fixed bool) *Tuner {
	start = clampSetting(cfg, start)
	t := &Tuner{cfg: cfg, fixed: fixed, cur: start, out: start, baseline: math.NaN(), restDur: cfg.MinRest, explore: true}
	t.dims[DimReaders].dir = +1
	t.dims[DimChunk].dir = +1
	if !cfg.TuneReaders {
		t.dims[DimReaders].converged = true
	}
	if !cfg.TuneChunk {
		t.dims[DimChunk].converged = true
	}
	return t
}

func clampSetting(cfg Config, s Setting) Setting {
	s.Readers = clampInt(s.Readers, cfg.MinReaders, cfg.MaxReaders)
	s.Chunk = clampInt(s.Chunk, cfg.MinChunk, cfg.MaxChunk)
	return s
}

func clampInt(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// Setting returns the setting to apply now.
func (t *Tuner) Setting() Setting {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.out
}

// Record attributes one finished op. Call it for every op, in any order.
func (t *Tuner) Record(op Op) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.opsDone++
	lat := (op.End - op.Start).Seconds()
	if t.opsDone == 1 {
		t.latEWMA, t.bytesEWMA = lat, float64(op.Bytes)
	} else {
		t.latEWMA += 0.05 * (lat - t.latEWMA)
		t.bytesEWMA += 0.05 * (float64(op.Bytes) - t.bytesEWMA)
	}
	for _, w := range t.open {
		w.add(op)
	}
}

// Step advances the tuner and returns the setting to apply.
func (t *Tuner) Step(p Probe) Setting {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.finalize(p)
	if t.fixed {
		return t.out
	}
	switch t.phase {
	case phWarmup:
		if p.Now >= t.cfg.Warmup && t.opsDone >= 8 {
			t.rest(p, 0)
		}
	case phRest:
		t.watch(p)
		if p.Now >= t.restUntil && !p.Starved {
			t.startRound(p)
		}
	case phRound:
		t.runRound(p)
	case phEval:
		last := t.arms[len(t.arms)-1].win
		if last.final {
			t.evaluate(p)
		} else if p.Now-last.t1 > t.cfg.MaxSettle {
			t.abort(p, "stuck")
		}
	}
	return t.out
}

func (t *Tuner) finalize(p Probe) {
	keep := t.open[:0]
	for _, w := range t.open {
		if w.t1 >= 0 && p.OldestInflight >= w.t1 {
			w.final = true
			continue
		}
		keep = append(keep, w)
	}
	t.open = keep
}

func (t *Tuner) openWindow(p Probe) *window {
	w := &window{t0: p.Now, t1: -1, dev0: p.DevReadBytes, our0: p.OurReadBytes}
	t.open = append(t.open, w)
	return w
}

func (t *Tuner) closeWindow(w *window, p Probe) {
	w.t1 = p.Now
	w.dev1, w.our1 = p.DevReadBytes, p.OurReadBytes
	if p.DevReadBytes < 0 {
		w.dev0 = -1
	}
}

func (t *Tuner) apply(s Setting, now time.Duration) {
	if s != t.out {
		t.out = s
		t.lastApplied = now
	}
}

func (t *Tuner) measureFor() time.Duration {
	d := time.Duration(1.5 * t.latEWMA * float64(time.Second))
	if d < t.cfg.MinMeasure {
		d = t.cfg.MinMeasure
	}
	if d > t.cfg.MaxMeasure {
		d = t.cfg.MaxMeasure
	}
	return d
}

// rest holds the incumbent for d, watching for a change of region.
func (t *Tuner) rest(p Probe, d time.Duration) {
	t.phase = phRest
	t.restUntil = p.Now + d
	t.arms = nil
	t.dropMonitor(p)
	t.strikes = 0
	t.apply(t.cur, p.Now)
}

// watch measures steady throughput while resting; a big, lasting change means
// the copy has moved into a different region of the drive.
func (t *Tuner) watch(p Probe) {
	if p.Starved || math.IsNaN(t.baseline) {
		t.dropMonitor(p)
		return
	}
	if t.monitor == nil {
		if p.OldestInflight >= t.lastApplied && p.Now-t.lastApplied > t.steadyAfter() {
			t.monitor = t.openWindow(p)
		}
		return
	}
	if t.monitor.t1 < 0 {
		if p.Now-t.monitor.t0 >= t.measureFor() {
			t.closeWindow(t.monitor, p)
		}
		return
	}
	if !t.monitor.final {
		return
	}
	r := t.monitor.rate()
	t.monitor = nil
	if r <= 0 {
		return
	}
	x := math.Log(r)
	if math.Abs(x-t.baseline) > regimeShift {
		t.strikes++
	} else {
		t.strikes = 0
	}
	if t.strikes >= 2 {
		t.regimeChange(p, x)
	}
}

func (t *Tuner) candidate(dim, dir int) Setting {
	c := t.cur
	if dim == DimReaders {
		n := c.Readers
		if dir > 0 {
			step := n / 2
			if t.explore {
				step = n
			}
			if step < 1 {
				step = 1
			}
			c.Readers = clampInt(n+step, t.cfg.MinReaders, t.cfg.MaxReaders)
		} else {
			step := n / 3
			if step < 1 {
				step = 1
			}
			c.Readers = clampInt(n-step, t.cfg.MinReaders, t.cfg.MaxReaders)
		}
	} else {
		if dir > 0 {
			if t.latEWMA > slowOp {
				return c // reads already take long; bigger ones only make every reaction slower
			}
			c.Chunk = clampInt(c.Chunk*2, t.cfg.MinChunk, t.cfg.MaxChunk)
		} else {
			c.Chunk = clampInt(c.Chunk/2, t.cfg.MinChunk, t.cfg.MaxChunk)
		}
	}
	return c
}

// fileBound: most ops are whole small files, so the chunk size doesn't matter.
func (t *Tuner) fileBound() bool { return t.bytesEWMA < 0.5*float64(t.cur.Chunk) }

func (t *Tuner) pickRound() (dim, dir int, c Setting, ok bool) {
	for _, d := range [2]int{t.preferDim, 1 - t.preferDim} {
		ds := &t.dims[d]
		if ds.converged || (d == DimChunk && t.fileBound()) {
			continue
		}
		for try := 0; try < 2; try++ {
			c := t.candidate(d, ds.dir)
			if c != t.cur && !ds.tried(ds.dir) {
				return d, ds.dir, c, true
			}
			ds.setTried(ds.dir)
			if ds.triedUp && ds.triedDown {
				ds.converged = true
				break
			}
			ds.dir = -ds.dir
		}
	}
	return 0, 0, Setting{}, false
}

func (t *Tuner) startRound(p Probe) {
	dim, dir, c, ok := t.pickRound()
	if !ok {
		// Converged everywhere: hold for a while, then look again from scratch.
		for i := range t.dims {
			t.dims[i].reset()
		}
		if !t.cfg.TuneReaders {
			t.dims[DimReaders].converged = true
		}
		if !t.cfg.TuneChunk {
			t.dims[DimChunk].converged = true
		}
		d := t.restDur
		t.restDur *= 2
		if t.restDur > t.cfg.MaxRest {
			t.restDur = t.cfg.MaxRest
		}
		t.rest(p, d)
		return
	}
	t.phase = phRound
	t.dim, t.dir, t.cand = dim, dir, c
	t.roundAt = p.Now
	t.arms = []*arm{{s: t.cur}, {s: c}, {s: t.cur}, {s: c}, {s: t.cur}}
	for _, a := range t.arms {
		a.drained = -1
	}
	t.armIdx = 0
	t.earlyDone = false
	t.arms[0].changedAt = t.lastApplied
	if p.OldestInflight >= t.lastApplied && p.Now-t.lastApplied > t.steadyAfter() {
		t.arms[0].drained = t.lastApplied // long unchanged: already steady
	}
	t.dropMonitor(p)
}

func (t *Tuner) dropMonitor(p Probe) {
	if t.monitor != nil && t.monitor.t1 < 0 {
		t.closeWindow(t.monitor, p)
	}
	t.monitor = nil
}

func (t *Tuner) runRound(p Probe) {
	if p.Starved {
		t.abort(p, "starved")
		return
	}
	if !t.earlyDone && t.armIdx >= 3 && t.arms[2].win != nil && t.arms[2].win.final {
		t.earlyDone = true
		if t.early(p) {
			return
		}
	}
	a := t.arms[t.armIdx]
	if a.win == nil {
		if a.settled(p) {
			a.win = t.openWindow(p)
		} else if p.Now-a.changedAt > t.cfg.MaxSettle {
			t.abort(p, "stuck")
		}
		return
	}
	if p.Now-a.win.t0 < t.measureFor() {
		return
	}
	t.closeWindow(a.win, p)
	t.armIdx++
	if t.armIdx == len(t.arms) {
		t.phase = phEval
		return
	}
	next := t.arms[t.armIdx]
	next.changedAt = p.Now
	t.apply(next.s, p.Now)
}

// steadyAfter is how long a setting must stand before the queue is steady.
func (t *Tuner) steadyAfter() time.Duration {
	return time.Duration(3 * t.latEWMA * float64(time.Second))
}

func (t *Tuner) abort(p Probe, note string) {
	t.addHistory(Decision{At: p.Now, Dim: t.dim, From: t.cur, To: t.cand, Gain: math.NaN(), Note: note})
	t.closeOpen(p)
	t.rest(p, t.cfg.MinRest/2)
}

func (t *Tuner) evaluate(p Probe) {
	var x [5]float64
	noisy := false
	for i, a := range t.arms {
		r := a.win.rate()
		if r <= 0 {
			t.inconclusive(p, "stalled")
			return
		}
		x[i] = math.Log(r)
		noisy = noisy || a.win.noisy()
	}
	if noisy {
		t.inconclusive(p, "disk busy")
		return
	}
	a1, c1, a2, c2, a3 := x[0], x[1], x[2], x[3], x[4]
	if math.Max(math.Abs(a1-a2), math.Abs(a2-a3)) > driftLimit {
		// The drive changed speed mid-round: a new region, not a verdict.
		t.addHistory(Decision{At: p.Now, Dim: t.dim, From: t.cur, To: t.cand, Gain: math.NaN(), Note: "region changed"})
		t.regimeChange(p, a3)
		return
	}
	s1 := c1 - (a1+a2)/2
	s2 := c2 - (a2+a3)/2
	mean := (s1 + s2) / 2
	worst := math.Min(s1, s2)
	noise := math.Abs(a1-2*a2+a3) / 2

	var keep bool
	switch {
	case t.dim == DimReaders && t.dir > 0:
		keep = mean > math.Max(upGain, noise) && worst > 0
	case t.dim == DimReaders:
		keep = (mean > -downLoss && worst > -downWorst) || (mean > math.Max(upGain, noise) && worst > 0)
	case t.dir > 0:
		keep = mean > math.Max(chunkGain, noise) && worst > 0
	default:
		keep = (mean > -chunkLoss && worst > -downWorst) || (mean > math.Max(chunkGain, noise) && worst > 0)
	}
	t.conclude(p, keep, mean, (a1+a2+a3)/3, (c1+c2)/2, "")
}

// conclude applies a round's verdict. levelA and levelC are the log rates of
// the current setting and the candidate.
func (t *Tuner) conclude(p Probe, keep bool, gain, levelA, levelC float64, how string) {
	t.streak = 0
	d := Decision{At: p.Now, Dim: t.dim, From: t.cur, To: t.cand, Gain: math.Exp(gain) - 1, Kept: keep}
	ds := &t.dims[t.dim]
	if keep {
		d.Note = "kept"
		t.cur = t.cand
		t.changes++
		t.baseline = levelC
		ds.triedUp, ds.triedDown = false, false
		if t.tunable(1 - t.dim) {
			t.dims[1-t.dim].reset() // the other dimension's best may have moved
		}
		t.preferDim = t.dim // momentum: same dimension, same direction
		t.restDur = t.cfg.MinRest
	} else {
		d.Note = "rejected"
		t.baseline = levelA
		if t.dim == DimReaders && t.dir > 0 {
			t.explore = false // overshot: refine in smaller steps from here
		}
		ds.setTried(t.dir)
		if ds.triedUp && ds.triedDown {
			ds.converged = true
			t.preferDim = 1 - t.dim
		} else {
			ds.dir = -t.dir
		}
	}
	d.Note += how
	t.addHistory(d)
	t.remember()
	t.rest(p, 0)
}

// early decides a round after its first three arms (A C A) when the
// difference is too big to be noise, which halves the time a round takes
// when it matters most: climbing out of a slow region.
func (t *Tuner) early(p Probe) bool {
	var x [3]float64
	for i, a := range t.arms[:3] {
		r := a.win.rate()
		if r <= 0 || a.win.noisy() {
			return false
		}
		x[i] = math.Log(r)
	}
	a1, c1, a2 := x[0], x[1], x[2]
	if math.Abs(a1-a2) > earlyStable {
		return false
	}
	s := c1 - (a1+a2)/2
	switch {
	case s > earlyGap:
		t.closeOpen(p)
		t.conclude(p, true, s, (a1+a2)/2, c1, " early")
		return true
	case s < -earlyGap:
		t.closeOpen(p)
		t.conclude(p, false, s, (a1+a2)/2, c1, " early")
		return true
	}
	return false
}

func (t *Tuner) closeOpen(p Probe) {
	for _, a := range t.arms {
		if a.win != nil && a.win.t1 < 0 {
			t.closeWindow(a.win, p)
		}
	}
}

func (t *Tuner) tunable(dim int) bool {
	if dim == DimReaders {
		return t.cfg.TuneReaders
	}
	return t.cfg.TuneChunk
}

func (t *Tuner) inconclusive(p Probe, note string) {
	t.addHistory(Decision{At: p.Now, Dim: t.dim, From: t.cur, To: t.cand, Gain: math.NaN(), Note: note})
	t.streak++
	if t.streak >= 2 {
		t.streak = 0
		t.rest(p, t.cfg.MinRest)
		return
	}
	t.rest(p, 0)
}

// remember files the current setting under the current region.
func (t *Tuner) remember() {
	if math.IsNaN(t.baseline) {
		return
	}
	if t.curReg == nil {
		t.curReg = &regime{level: t.baseline, best: t.cur}
		t.regimes = append(t.regimes, t.curReg)
		return
	}
	t.curReg.level = 0.5*t.curReg.level + 0.5*t.baseline
	t.curReg.best = t.cur
}

// regimeChange handles a lasting jump in speed at an unchanged setting.
func (t *Tuner) regimeChange(p Probe, x float64) {
	if t.curReg != nil {
		t.curReg.best = t.cur
	}
	var match *regime
	best := regimeMatch
	for _, r := range t.regimes {
		if r == t.curReg {
			continue
		}
		if d := math.Abs(r.level - x); d < best {
			best, match = d, r
		}
	}
	slower := math.IsNaN(t.baseline) || x < t.baseline
	if match != nil {
		if match.best != t.cur {
			t.addHistory(Decision{At: p.Now, Dim: DimReaders, From: t.cur, To: match.best, Gain: math.NaN(), Kept: true, Note: "jumped"})
			t.cur = match.best
			t.changes++
		}
		t.curReg = match
		t.baseline = match.level
	} else {
		t.curReg = &regime{level: x, best: t.cur}
		t.regimes = append(t.regimes, t.curReg)
		t.baseline = x
	}
	for i := range t.dims {
		t.dims[i].reset()
		if !t.tunable(i) {
			t.dims[i].converged = true
		}
	}
	// A slower region is where more reads in flight tend to pay; try that first.
	if slower {
		t.dims[DimReaders].dir = +1
		t.explore = true
	} else {
		t.dims[DimReaders].dir = -1
	}
	t.preferDim = DimReaders
	t.restDur = t.cfg.MinRest
	t.streak = 0
	t.rest(p, 0)
}

func (t *Tuner) addHistory(d Decision) {
	t.history = append(t.history, d)
	if len(t.history) > 64 {
		t.history = append(t.history[:0], t.history[len(t.history)-32:]...)
	}
}

// Status returns a snapshot for the display.
func (t *Tuner) Status(now time.Duration) Status {
	t.mu.Lock()
	defer t.mu.Unlock()
	st := Status{Applied: t.out, Incumbent: t.cur, Candidate: t.cand, Dim: t.dim, Changes: t.changes}
	switch {
	case t.fixed:
		st.Mode = "fixed"
	case t.phase == phWarmup:
		st.Mode = "warming up"
	case t.phase == phRound || t.phase == phEval:
		st.Mode = "probing"
		for _, a := range t.arms {
			ar := ArmRate{Setting: a.s}
			if a.win != nil && a.win.final {
				ar.Rate, ar.Final = a.win.rate(), true
			}
			st.Arms = append(st.Arms, ar)
		}
	default:
		st.Mode = "holding"
		if t.restUntil > now {
			st.RestLeft = t.restUntil - now
		}
	}
	n := len(t.history)
	if n > 8 {
		n = 8
	}
	st.History = append([]Decision(nil), t.history[len(t.history)-n:]...)
	for _, r := range t.regimes {
		st.Regimes = append(st.Regimes, RegimeView{Rate: math.Exp(r.level), Best: r.best, Current: r == t.curReg})
	}
	sort.Slice(st.Regimes, func(i, j int) bool { return st.Regimes[i].Rate < st.Regimes[j].Rate })
	return st
}

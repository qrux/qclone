// Package tuner decides how many flying reads to keep in flight.
//
// The engine splits reads in flight into two kinds. A read is stuck once it
// has been in flight longer than a threshold (50 ms; a healthy SSD reads a
// chunk in about a millisecond, a degraded region takes hundreds of
// milliseconds or seconds), or when it was sent to a file already known to be
// slow. The rest are flying. The engine keeps
//
//	reads in flight = flying + min(stuck, max stuck)
//
// so stuck reads never use up the flying ones' room, and stuck reads are what's
// capped: once max stuck are stuck, nothing more goes to slow files. This
// package sets only the flying number, which has no fixed cap.
//
// It is judged on flying reads alone. Those all read fresh data at about the
// same speed, so their throughput over a second is a steady number, and a plain
// before-and-after comparison works: no read that got stuck counts, so which
// slow files happen to be open doesn't matter. The rule:
//
//   - At the start, add 4 flying reads at a time while each step makes them at
//     least 5% faster. When one doesn't, go back. Small steps stop close to
//     where more stops paying; doubling could overshoot by as many again, and
//     every flying read too many is one more read that can get stuck.
//   - Then every 30 s, try one step up (+4) and one step down (-4), each
//     against a fresh measurement of the current number, and move only to one
//     that is at least 5% faster. A step up that pays starts the climb again.
//   - A measurement lasts at least 1 s and 20 flying reads. If 5 s pass without
//     20, there's too little flying to judge, and the number holds.
//   - While the readers can't be kept busy (the walk is behind, or the copy is
//     nearly done), nothing is measured.
package tuner

import (
	"sync"
	"time"
)

// Config bounds the tuner. Times are durations since the run started.
type Config struct {
	MinReaders, MaxReaders int           // the range of flying reads
	Step                   int           // flying reads added or taken away per test
	Warmup                 time.Duration // before the first measurement
	Window                 time.Duration // shortest measurement
	MaxWindow              time.Duration // longest; too few flying reads by then and the test is off
	MinOps                 int           // flying reads a measurement needs
	Settle                 time.Duration // after a change, before measuring
	Gain                   float64       // a change must be this much faster (0.05: 5%)
	Recheck                time.Duration // between checks once settled
}

// DefaultConfig is what qclone uses.
func DefaultConfig() Config {
	// No fixed ceiling on flying reads: they're raised only while a step pays,
	// which on a SATA drive stops well short of its 32 command slots. (With
	// steps of 4 and a 5% bar, even perfect scaling stops at 80: 4 more is under
	// 5% past that.) 256 is a backstop, the most --max-readers allows.
	return Config{
		MinReaders: 1, MaxReaders: 256, Step: 4,
		Warmup:    time.Second,
		Window:    time.Second,
		MaxWindow: 5 * time.Second,
		MinOps:    20,
		Settle:    100 * time.Millisecond,
		Gain:      0.05,
		Recheck:   30 * time.Second,
	}
}

// Op is one finished flying read (and its write), timed from the run's start.
// The engine records only reads that were never stuck.
type Op struct {
	Start, End time.Duration
	Bytes      int64
}

// Probe is the engine's view at one control step.
type Probe struct {
	Now     time.Duration
	Starved bool // readers are waiting for work, or the copy is nearly done
}

// Decision is one finished test, kept for the display.
type Decision struct {
	At       time.Duration
	From, To int     // flying reads
	Gain     float64 // To over From, as a ratio minus one
	Kept     bool
	Note     string // "kept", "not kept", "too few flying reads", "starved"
}

// Status is a snapshot for the display.
type Status struct {
	Mode      string // "fixed", "warming up", "measuring", "testing", "holding", "waiting for work"
	Readers   int    // flying reads now
	Base      int    // the number being tested against, while measuring or testing
	Candidate int    // the number on trial, while testing
	BaseRate  float64
	Rate      float64 // the measurement in progress so far, bytes/s; 0 until it has some
	History   []Decision
	NextCheck time.Duration // while holding
	Changes   int
}

type phase int

const (
	phWarmup phase = iota
	phBase         // measuring the current number
	phCand         // measuring a candidate
	phHold
)

// statusHistory is how many decisions a Status carries.
const statusHistory = 64

// Tuner is safe for concurrent use.
type Tuner struct {
	mu    sync.Mutex
	cfg   Config
	fixed bool

	w         int           // flying reads now
	appliedAt time.Duration // when w last changed
	phase     phase
	climbing  bool // stepping up until a step stops paying
	base      int  // the incumbent during a check
	baseRate  float64
	plan      []int // candidates left in this check
	holdUntil time.Duration
	starved   bool

	// The measurement in progress: ops that began at or after since and ended
	// at or after from. from < 0 until the window opens.
	since, from time.Duration
	bytes       int64
	ops         int
	lastNow     time.Duration

	history []Decision
	changes int
}

// New starts a tuner at start flying reads; fixed never changes them.
func New(cfg Config, start int, fixed bool) *Tuner {
	if cfg.MinReaders < 1 {
		cfg.MinReaders = 1
	}
	if cfg.MaxReaders < cfg.MinReaders {
		cfg.MaxReaders = cfg.MinReaders
	}
	if cfg.Step < 1 {
		cfg.Step = 4
	}
	return &Tuner{cfg: cfg, fixed: fixed, w: clamp(start, cfg.MinReaders, cfg.MaxReaders), climbing: true, from: -1}
}

func clamp(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// Readers is the number of flying reads to keep in flight now.
func (t *Tuner) Readers() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.w
}

// Record notes one finished flying read.
func (t *Tuner) Record(op Op) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.from >= 0 && op.Start >= t.since && op.End >= t.from {
		t.bytes += op.Bytes
		t.ops++
	}
}

// Step advances the tuner and returns the flying reads to use.
func (t *Tuner) Step(p Probe) int {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.lastNow = p.Now
	t.starved = p.Starved
	if t.fixed {
		return t.w
	}
	if p.Starved {
		if t.phase == phBase || t.phase == phCand {
			t.decide(p.Now, t.cand(), 0, false, "starved")
			t.revert(p.Now)
			t.hold(p.Now, t.cfg.Recheck/6)
		}
		return t.w
	}
	switch t.phase {
	case phWarmup:
		if p.Now >= t.cfg.Warmup {
			t.check(p.Now)
		}
	case phHold:
		if p.Now >= t.holdUntil {
			t.check(p.Now)
		}
	case phBase, phCand:
		t.measure(p.Now)
	}
	return t.w
}

// cand is the number on trial, or while measuring the base, the one that
// would have been tried next.
func (t *Tuner) cand() int {
	if t.phase == phCand {
		return t.w
	}
	if len(t.plan) > 0 {
		return t.plan[0]
	}
	return t.base
}

// check starts a check: measure the current number, then try the candidates.
func (t *Tuner) check(now time.Duration) {
	t.base = t.w
	t.plan = t.plan[:0]
	if up := clamp(t.w+t.cfg.Step, t.cfg.MinReaders, t.cfg.MaxReaders); up != t.w {
		t.plan = append(t.plan, up)
	}
	if !t.climbing {
		if down := clamp(t.w-t.cfg.Step, t.cfg.MinReaders, t.cfg.MaxReaders); down != t.w {
			t.plan = append(t.plan, down)
		}
	}
	if len(t.plan) == 0 {
		t.climbing = false
		t.hold(now, t.cfg.Recheck)
		return
	}
	t.phase = phBase
	t.open(t.appliedAt, now)
}

// open starts a measurement of ops begun at or after since, from the later of
// now and since+settle.
func (t *Tuner) open(since, now time.Duration) {
	t.since = since
	t.from = since + t.cfg.Settle
	if t.from < now {
		t.from = now
	}
	t.bytes, t.ops = 0, 0
}

func (t *Tuner) measure(now time.Duration) {
	if now < t.from {
		return
	}
	d := now - t.from
	switch {
	case d >= t.cfg.Window && t.ops >= t.cfg.MinOps:
	case d >= t.cfg.MaxWindow:
		t.decide(now, t.cand(), 0, false, "too few flying reads")
		t.revert(now)
		t.hold(now, t.cfg.Recheck/3)
		return
	default:
		return
	}
	rate := float64(t.bytes) / d.Seconds()
	if t.phase == phBase {
		t.baseRate = rate
		t.next(now)
		return
	}
	gain := rate/t.baseRate - 1
	cand := t.w
	if gain >= t.cfg.Gain {
		t.decide(now, cand, gain, true, "kept")
		t.changes++
		if cand > t.base {
			// Up paid: keep climbing from here, against what was just measured.
			t.climbing = true
			t.base, t.baseRate = cand, rate
			t.plan = t.plan[:0]
			if up := clamp(cand+t.cfg.Step, t.cfg.MinReaders, t.cfg.MaxReaders); up != cand {
				t.plan = append(t.plan, up)
			}
			t.next(now)
			return
		}
		t.climbing = false
		t.plan = t.plan[:0]
		t.hold(now, t.cfg.Recheck)
		return
	}
	t.decide(now, cand, gain, false, "not kept")
	t.revert(now)
	if cand > t.base {
		t.climbing = false
	}
	t.next(now)
}

// next tries the next candidate in the plan, or holds when none are left.
func (t *Tuner) next(now time.Duration) {
	if len(t.plan) == 0 {
		t.hold(now, t.cfg.Recheck)
		return
	}
	c := t.plan[0]
	t.plan = t.plan[1:]
	t.apply(c, now)
	t.phase = phCand
	t.open(now, now)
}

func (t *Tuner) apply(w int, now time.Duration) {
	if w != t.w {
		t.w = w
		t.appliedAt = now
	}
}

// revert goes back to the incumbent after a candidate.
func (t *Tuner) revert(now time.Duration) {
	if t.phase == phCand {
		t.apply(t.base, now)
	}
}

func (t *Tuner) hold(now, d time.Duration) {
	t.phase = phHold
	t.holdUntil = now + d
	t.from = -1
	t.plan = t.plan[:0]
}

func (t *Tuner) decide(now time.Duration, to int, gain float64, kept bool, note string) {
	t.history = append(t.history, Decision{At: now, From: t.base, To: to, Gain: gain, Kept: kept, Note: note})
	if len(t.history) > 4*statusHistory {
		t.history = append(t.history[:0], t.history[len(t.history)-statusHistory:]...)
	}
}

// Status returns a snapshot for the display.
func (t *Tuner) Status(now time.Duration) Status {
	t.mu.Lock()
	defer t.mu.Unlock()
	st := Status{Readers: t.w, Changes: t.changes}
	switch {
	case t.fixed:
		st.Mode = "fixed"
	case t.starved:
		st.Mode = "waiting for work"
	case t.phase == phWarmup:
		st.Mode = "warming up"
	case t.phase == phBase || t.phase == phCand:
		st.Mode = "measuring"
		st.Base = t.base
		if t.phase == phCand {
			st.Mode = "testing"
			st.Candidate = t.w
			st.BaseRate = t.baseRate
		}
		if d := t.lastNow - t.from; t.from >= 0 && d > 0 && t.ops > 0 {
			st.Rate = float64(t.bytes) / d.Seconds()
		}
	default:
		st.Mode = "holding"
		if t.holdUntil > now {
			st.NextCheck = t.holdUntil - now
		}
	}
	n := len(t.history)
	if n > statusHistory {
		n = statusHistory
	}
	st.History = append([]Decision(nil), t.history[len(t.history)-n:]...)
	return st
}

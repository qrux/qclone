package tuner

import (
	"math/rand"
	"testing"
	"time"
)

// lane drives a Tuner in virtual time. The flying reads' throughput depends
// only on the time and on how many flying reads there are.
type lane struct {
	t       *Tuner
	rate    func(now time.Duration, w int) float64 // bytes/s
	noise   float64                                // relative spread of each tick's bytes
	starved func(now time.Duration) bool
	rng     *rand.Rand
	now     time.Duration
	w       int
	carry   float64
}

const (
	tick  = 20 * time.Millisecond
	chunk = 256 << 10
)

func newLane(start int, fixed bool, rate func(time.Duration, int) float64) *lane {
	t := New(DefaultConfig(), start, fixed)
	return &lane{t: t, rate: rate, rng: rand.New(rand.NewSource(1)), w: t.Readers()}
}

func (l *lane) run(until time.Duration) {
	for l.now < until {
		l.now += tick
		r := l.rate(l.now, l.w) * (1 + l.noise*l.rng.NormFloat64())
		if r > 0 {
			l.carry += r * tick.Seconds()
		}
		for l.carry >= chunk {
			l.carry -= chunk
			l.t.Record(Op{Start: l.now - tick/2, End: l.now, Bytes: chunk})
		}
		l.w = l.t.Step(Probe{Now: l.now, Starved: l.starved != nil && l.starved(l.now)})
	}
}

func (l *lane) history() []Decision { return l.t.Status(l.now).History }

// scales: each flying read adds 20 MB/s, up to 16 of them.
func scales(_ time.Duration, w int) float64 {
	if w > 16 {
		w = 16
	}
	return float64(w) * 20e6
}

func TestDoublesWhileItPays(t *testing.T) {
	l := newLane(4, false, scales)
	l.run(15 * time.Second)
	if l.w != 16 {
		t.Fatalf("flying reads %d, want 16; history %+v", l.w, l.history())
	}
	h := l.history()
	want := []struct {
		from, to int
		kept     bool
	}{{4, 8, true}, {8, 16, true}, {16, 32, false}}
	if len(h) != len(want) {
		t.Fatalf("history %+v", h)
	}
	for i, w := range want {
		if h[i].From != w.from || h[i].To != w.to || h[i].Kept != w.kept {
			t.Errorf("decision %d: %d -> %d kept %v, want %d -> %d kept %v", i, h[i].From, h[i].To, h[i].Kept, w.from, w.to, w.kept)
		}
	}
	if g := h[0].Gain; g < 0.9 || g > 1.1 {
		t.Errorf("4 -> 8 measured %+.0f%%, want about +100%%", g*100)
	}
}

func TestStaysWhenMoreDoesNotHelp(t *testing.T) {
	l := newLane(4, false, func(time.Duration, int) float64 { return 400e6 })
	l.run(10 * time.Second)
	h := l.history()
	if l.w != 4 || len(h) != 1 || h[0].To != 8 || h[0].Kept {
		t.Fatalf("flying reads %d; history %+v", l.w, h)
	}
}

// After the drive changes so that more than 8 flying reads hurt, the next
// check tries up (worse), then down (better), and moves down.
func TestRechecksBothWays(t *testing.T) {
	l := newLane(4, false, func(now time.Duration, w int) float64 {
		if now < 20*time.Second {
			return scales(now, w)
		}
		if w > 8 {
			return 300e6 * 8 / float64(w)
		}
		return 300e6
	})
	l.run(15 * time.Second)
	if l.w != 16 {
		t.Fatalf("climbed to %d, want 16", l.w)
	}
	l.run(100 * time.Second)
	if l.w != 8 {
		t.Fatalf("flying reads %d, want 8; history %+v", l.w, l.history())
	}
	var down bool
	for _, d := range l.history() {
		if d.From == 16 && d.To == 8 && d.Kept {
			down = true
		}
		if d.At > 20*time.Second && d.From == 8 && d.To == 16 && d.Kept {
			t.Errorf("went back up: %+v", d)
		}
	}
	if !down {
		t.Errorf("never moved 16 -> 8: %+v", l.history())
	}
}

func TestFixedNeverMoves(t *testing.T) {
	l := newLane(4, true, scales)
	l.run(60 * time.Second)
	if st := l.t.Status(l.now); l.w != 4 || st.Mode != "fixed" || len(st.History) != 0 {
		t.Fatalf("flying reads %d, mode %q, history %+v", l.w, st.Mode, st.History)
	}
}

func TestStarvedHolds(t *testing.T) {
	l := newLane(4, false, scales)
	l.starved = func(time.Duration) bool { return true }
	l.run(60 * time.Second)
	if st := l.t.Status(l.now); l.w != 4 || st.Mode != "waiting for work" || len(st.History) != 0 {
		t.Fatalf("flying reads %d, mode %q, history %+v", l.w, st.Mode, st.History)
	}
}

// Starved in the middle of a test: the test is off and the number goes back.
func TestStarvedMidTestGoesBack(t *testing.T) {
	l := newLane(4, false, scales)
	l.starved = func(now time.Duration) bool { return now > 2*time.Second && now < 3*time.Second }
	l.run(2500 * time.Millisecond)
	if l.w != 4 {
		t.Fatalf("flying reads %d during starvation, want 4 back", l.w)
	}
	if h := l.history(); len(h) != 1 || h[0].Note != "starved" {
		t.Fatalf("history %+v", h)
	}
	l.run(30 * time.Second)
	if l.w != 16 {
		t.Fatalf("flying reads %d after starvation, want 16", l.w)
	}
}

// Nothing flying (every read stuck): nothing to judge by, so it holds.
func TestTooFewFlyingReads(t *testing.T) {
	l := newLane(4, false, func(time.Duration, int) float64 { return 0 })
	l.run(20 * time.Second)
	h := l.history()
	if l.w != 4 || len(h) == 0 || h[0].Note != "too few flying reads" || h[0].From != 4 || h[0].To != 8 {
		t.Fatalf("flying reads %d; history %+v", l.w, h)
	}
}

// Ordinary jitter in the flying reads doesn't make it wander.
func TestSteadyUnderJitter(t *testing.T) {
	l := newLane(4, false, func(time.Duration, int) float64 { return 400e6 })
	l.noise = 0.1
	l.run(180 * time.Second)
	if st := l.t.Status(l.now); st.Changes > 1 {
		t.Fatalf("%d changes on a flat drive; history %+v", st.Changes, st.History)
	}
}

// Ops that began before a change don't count toward the measurement after it.
func TestOldOpsDoNotCount(t *testing.T) {
	tu := New(DefaultConfig(), 4, false)
	tu.Step(Probe{Now: 2 * time.Second}) // past warmup: measuring 4
	tu.mu.Lock()
	tu.from = 2 * time.Second
	tu.since = 2 * time.Second
	tu.mu.Unlock()
	tu.Record(Op{Start: time.Second, End: 2100 * time.Millisecond, Bytes: 1 << 30})
	tu.Record(Op{Start: 2050 * time.Millisecond, End: 2100 * time.Millisecond, Bytes: 100})
	tu.mu.Lock()
	defer tu.mu.Unlock()
	if tu.bytes != 100 || tu.ops != 1 {
		t.Fatalf("counted %d bytes in %d ops; only the op begun after the window's start counts", tu.bytes, tu.ops)
	}
}

package tuner

import (
	"container/heap"
	"math"
	"math/rand"
	"testing"
	"time"

	"qclone/internal/sim"
)

// vsim drives a Tuner against the simulated drive in virtual time: readers
// issue chunk-sized reads one after another along a single stream, and the
// region each read lands in depends on its position in that stream.
type vsim struct {
	t      *Tuner
	model  sim.Model
	region func(pos int64) sim.Region
	rng    *rand.Rand
	sigma  float64 // log-normal noise on each op's duration

	now      time.Duration
	pos      int64
	inflight opHeap
	ourBytes int64
	setting  Setting

	// per-second samples
	secBytes []int64
	secRead  []int
}

type vop struct {
	start, end time.Duration
	bytes      int64
}

type opHeap []vop

func (h opHeap) Len() int            { return len(h) }
func (h opHeap) Less(i, j int) bool  { return h[i].end < h[j].end }
func (h opHeap) Swap(i, j int)       { h[i], h[j] = h[j], h[i] }
func (h *opHeap) Push(x interface{}) { *h = append(*h, x.(vop)) }
func (h *opHeap) Pop() interface{} {
	old := *h
	x := old[len(old)-1]
	*h = old[:len(old)-1]
	return x
}

func newVsim(cfg Config, start Setting, region func(int64) sim.Region) *vsim {
	return &vsim{t: New(cfg, start, false), region: region, rng: rand.New(rand.NewSource(1)), setting: start}
}

func (v *vsim) fill() {
	for v.inflight.Len() < v.setting.Readers {
		n := v.setting.Chunk
		end := v.model.Schedule(v.now, v.region(v.pos), n)
		if v.sigma > 0 {
			f := math.Exp(v.rng.NormFloat64() * v.sigma)
			end = v.now + time.Duration(float64(end-v.now)*f)
		}
		heap.Push(&v.inflight, vop{start: v.now, end: end, bytes: int64(n)})
		v.pos += int64(n)
	}
}

func (v *vsim) oldest() time.Duration {
	o := v.now
	for _, op := range v.inflight {
		if op.start < o {
			o = op.start
		}
	}
	return o
}

func (v *vsim) run(until time.Duration) {
	const tick = 100 * time.Millisecond
	next := v.now
	v.fill()
	for v.now < until {
		if v.inflight.Len() > 0 && v.inflight[0].end <= next {
			op := heap.Pop(&v.inflight).(vop)
			v.now = op.end
			v.ourBytes += op.bytes
			v.t.Record(Op{Start: op.start, End: op.end, Bytes: op.bytes})
			sec := int(v.now / time.Second)
			for len(v.secBytes) <= sec {
				v.secBytes = append(v.secBytes, 0)
				v.secRead = append(v.secRead, v.setting.Readers)
			}
			v.secBytes[sec] += op.bytes
			v.fill()
			continue
		}
		v.now = next
		next += tick
		v.setting = v.t.Step(Probe{Now: v.now, OldestInflight: v.oldest(), DevReadBytes: -1, OurReadBytes: v.ourBytes})
		v.fill()
	}
}

// rate is mean bytes/s over [from, to) seconds.
func (v *vsim) rate(from, to int) float64 {
	var b int64
	for s := from; s < to && s < len(v.secBytes); s++ {
		b += v.secBytes[s]
	}
	return float64(b) / float64(to-from)
}

func (v *vsim) meanReaders(from, to int) float64 {
	var n, c int
	for s := from; s < to && s < len(v.secRead); s++ {
		n += v.secRead[s]
		c++
	}
	return float64(n) / float64(c)
}

func slowScalable() sim.Region {
	return sim.Region{OpRate: 0.6e6, Bandwidth: 8e6, Overhead: 2 * time.Millisecond}
}

func fast() sim.Region {
	return sim.Region{OpRate: 300e6, Bandwidth: 500e6, Overhead: 100 * time.Microsecond}
}

func start() Setting { return Setting{Readers: 4, Chunk: 1 << 20, Files: 1} }

func TestClimbsWhenSlowRegionScales(t *testing.T) {
	v := newVsim(DefaultConfig(), start(), func(int64) sim.Region { return slowScalable() })
	v.run(900 * time.Second)
	got := v.t.Setting()
	r := v.rate(600, 900)
	t.Logf("setting %+v, rate %.2f MB/s (bandwidth 8), at 4 readers it would be 2.4", got, r/1e6)
	if got.Readers < 12 {
		t.Errorf("readers = %d, want >= 12 (the drive saturates at about 14)", got.Readers)
	}
	if r < 0.85*8e6 {
		t.Errorf("rate %.2f MB/s, want >= 6.8", r/1e6)
	}
}

func TestStaysLowWhenSlowRegionDoesNotScale(t *testing.T) {
	flat := sim.Region{OpRate: 2e6, Bandwidth: 2e6, Overhead: 2 * time.Millisecond}
	v := newVsim(DefaultConfig(), start(), func(int64) sim.Region { return flat })
	v.run(900 * time.Second)
	got := v.t.Setting()
	r := v.rate(300, 900)
	t.Logf("setting %+v, rate %.2f MB/s", got, r/1e6)
	if got.Readers > 4 {
		t.Errorf("readers = %d, want <= 4: more reads in flight buy nothing here", got.Readers)
	}
	if r < 0.9*2e6 {
		t.Errorf("rate %.2f MB/s, want >= 1.8", r/1e6)
	}
}

func TestFastRegionKeepsFullSpeed(t *testing.T) {
	v := newVsim(DefaultConfig(), start(), func(int64) sim.Region { return fast() })
	v.run(120 * time.Second)
	got := v.t.Setting()
	r := v.rate(30, 120)
	t.Logf("setting %+v, rate %.0f MB/s", got, r/1e6)
	if r < 0.95*500e6 {
		t.Errorf("rate %.0f MB/s, want >= 475", r/1e6)
	}
	if got.Readers < 2 {
		t.Errorf("readers = %d: one read in flight can't saturate this drive", got.Readers)
	}
}

// Q's drive: stretches of old data between stretches of fresh data. The
// second visit to a slow stretch should start from what the first one learned.
func TestRemembersSlowRegions(t *testing.T) {
	const slowLen, fastLen = 1200e6, 15e9
	period := int64(slowLen + fastLen)
	v := newVsim(DefaultConfig(), start(), func(pos int64) sim.Region {
		if pos%period < slowLen {
			return slowScalable()
		}
		return fast()
	})
	// Find when the stream enters the second slow stretch.
	var secondSlow time.Duration = -1
	for v.now < 1200*time.Second {
		v.run(v.now + time.Second)
		if secondSlow < 0 && v.pos > period && v.pos%period < slowLen {
			secondSlow = v.now
		}
		if secondSlow >= 0 && v.now > secondSlow+60*time.Second {
			break
		}
	}
	if secondSlow < 0 {
		t.Fatal("never reached the second slow stretch")
	}
	s := int(secondSlow / time.Second)
	readers := v.meanReaders(s+15, s+60)
	r := v.rate(s+15, s+60)
	t.Logf("second slow stretch at %ds: mean readers %.1f, rate %.2f MB/s; regimes %+v", s, readers, r/1e6, v.t.Status(v.now).Regimes)
	if readers < 10 {
		t.Errorf("mean readers %.1f in the second slow stretch, want >= 10 (remembered)", readers)
	}
	if r < 0.75*8e6 {
		t.Errorf("rate %.2f MB/s in the second slow stretch, want >= 6", r/1e6)
	}
}

func TestChunkGrowsWhenReadsHaveOverhead(t *testing.T) {
	cfg := DefaultConfig()
	cfg.MaxReaders = 4
	costly := sim.Region{OpRate: 300e6, Bandwidth: 2000e6, Overhead: 5 * time.Millisecond}
	v := newVsim(cfg, start(), func(int64) sim.Region { return costly })
	v.run(180 * time.Second)
	got := v.t.Setting()
	t.Logf("setting %+v, rate %.0f MB/s", got, v.rate(120, 180)/1e6)
	if got.Chunk < 4<<20 {
		t.Errorf("chunk = %d, want >= 4 MiB when each read costs 5 ms", got.Chunk)
	}
}

func TestConvergesThroughNoise(t *testing.T) {
	v := newVsim(DefaultConfig(), start(), func(int64) sim.Region { return slowScalable() })
	v.sigma = 0.25
	v.run(1500 * time.Second)
	got := v.t.Setting()
	r := v.rate(900, 1500)
	t.Logf("setting %+v, rate %.2f MB/s", got, r/1e6)
	if got.Readers < 9 {
		t.Errorf("readers = %d under noise, want >= 9", got.Readers)
	}
	if r < 0.75*8e6 {
		t.Errorf("rate %.2f MB/s under noise, want >= 6", r/1e6)
	}
}

func TestFixedNeverMoves(t *testing.T) {
	tu := New(DefaultConfig(), start(), true)
	for i := 0; i < 1000; i++ {
		now := time.Duration(i) * 100 * time.Millisecond
		tu.Record(Op{Start: now, End: now + 50*time.Millisecond, Bytes: 1 << 20})
		if s := tu.Step(Probe{Now: now, OldestInflight: now}); s != start() {
			t.Fatalf("fixed tuner moved to %+v", s)
		}
	}
}

func TestStarvedHolds(t *testing.T) {
	tu := New(DefaultConfig(), start(), false)
	for i := 0; i < 3000; i++ {
		now := time.Duration(i) * 100 * time.Millisecond
		tu.Record(Op{Start: now, End: now + 50*time.Millisecond, Bytes: 1 << 20})
		if s := tu.Step(Probe{Now: now, OldestInflight: now, Starved: true}); s != start() {
			t.Fatalf("starved tuner moved to %+v", s)
		}
	}
}

func TestFluidWindow(t *testing.T) {
	w := &window{t0: 10 * time.Second, t1: 12 * time.Second}
	w.add(Op{Start: 9 * time.Second, End: 11 * time.Second, Bytes: 100})  // half inside
	w.add(Op{Start: 11 * time.Second, End: 15 * time.Second, Bytes: 400}) // a quarter inside
	w.add(Op{Start: 5 * time.Second, End: 9 * time.Second, Bytes: 999})   // outside
	if w.bytes != 150 {
		t.Fatalf("fluid bytes = %v, want 150", w.bytes)
	}
	if r := w.rate(); r != 75 {
		t.Fatalf("rate = %v, want 75", r)
	}
}

func TestNoisyWindow(t *testing.T) {
	w := &window{t0: 0, t1: time.Second, dev0: 0, dev1: 50e6, our0: 0, our1: 10e6}
	if !w.noisy() {
		t.Fatal("device read 5x what we did; should be noisy")
	}
	w.dev1 = 11e6
	if w.noisy() {
		t.Fatal("device read about what we did; should not be noisy")
	}
	w.dev0 = -1
	w.dev1 = 1e12
	if w.noisy() {
		t.Fatal("unknown device counters must not count as noisy")
	}
}

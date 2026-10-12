package rate

import (
	"math"
	"testing"
	"time"
)

var t0 = time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)

func at(s float64) time.Time { return t0.Add(time.Duration(s * float64(time.Second))) }

func TestNotOkBeforeFirstByte(t *testing.T) {
	m := New(t0)
	if _, ok := m.Rate(at(10)); ok {
		t.Fatal("rate reported before any byte")
	}
}

func TestSteadyRate(t *testing.T) {
	m := New(t0)
	// 100 MB/s in 1 MB pieces.
	for i := 1; i <= 2000; i++ {
		m.Add(1e6, at(float64(i)*0.01))
	}
	r, ok := m.Rate(at(20))
	if !ok || math.Abs(r-100e6)/100e6 > 0.02 {
		t.Fatalf("steady rate = %.0f, want about 100e6", r)
	}
}

// The case rclone gets wrong: a read takes far longer than any fixed window.
func TestNeverZeroDuringStall(t *testing.T) {
	m := New(t0)
	for i := 1; i <= 10; i++ {
		m.Add(1<<20, at(float64(i)*0.5)) // 2 MiB/s
	}
	prev := math.Inf(1)
	for s := 5.0; s <= 600; s += 0.25 { // a ten-minute stall
		r, ok := m.Rate(at(s))
		if !ok || r <= 0 {
			t.Fatalf("rate %.3f (ok=%v) at %.2fs, want positive", r, ok, s)
		}
		if r > prev+1e-9 {
			t.Fatalf("rate rose during a stall at %.2fs: %.1f > %.1f", s, r, prev)
		}
		prev = r
	}
}

// After a stall, the rate comes back up as soon as data flows again.
func TestRecoversAfterStall(t *testing.T) {
	m := New(t0)
	m.Add(1<<20, at(1))
	for i := 0; i < 400; i++ {
		m.Add(1<<20, at(60+float64(i)*0.01)) // 100 MiB/s burst
	}
	r, _ := m.Rate(at(64.1))
	if r < 80*(1<<20) {
		t.Fatalf("rate after recovery = %.0f, want near 100 MiB/s", r)
	}
}

func TestSparseSlowDevice(t *testing.T) {
	m := New(t0)
	// One 256 KiB completion every 3 s (about 87 KB/s): slower than the window.
	for i := 1; i <= 40; i++ {
		m.Add(256<<10, at(float64(i)*3))
		for s := float64(i) * 3; s < float64(i+1)*3; s += 0.5 {
			if r, ok := m.Rate(at(s)); !ok || r <= 0 {
				t.Fatalf("zero rate at %.1fs", s)
			}
		}
	}
	r, _ := m.Rate(at(121))
	want := float64(256<<10) / 3
	if math.Abs(r-want)/want > 0.35 {
		t.Fatalf("sparse rate = %.0f, want about %.0f", r, want)
	}
}

func TestPruningKeepsMemoryBounded(t *testing.T) {
	m := New(t0)
	for i := 1; i <= 200000; i++ {
		m.Add(4096, at(float64(i)*0.001))
	}
	if n := len(m.ev) - m.head; n > 6000 {
		t.Fatalf("meter kept %d events for a 4 s window at 1000/s", n)
	}
	if r, _ := m.Rate(at(200)); math.Abs(r-4096e3)/4096e3 > 0.02 {
		t.Fatalf("rate after pruning = %.0f", r)
	}
}

func TestAverageAndIdle(t *testing.T) {
	m := New(t0)
	m.Add(1000, at(5))
	if a, _ := m.Average(at(10)); math.Abs(a-100) > 1e-9 {
		t.Fatalf("average = %v", a)
	}
	if d := m.Idle(at(8)); d != 3*time.Second {
		t.Fatalf("idle = %v", d)
	}
}

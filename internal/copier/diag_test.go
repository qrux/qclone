package copier

// A diagnostic, not a test: it copies the ~1 GB demo tree through the simulated
// degraded drive and prints how long each copy took, the queue it measured,
// what was in flight in each lane, and every decision both tuners made: in
// hash order, in dfs order, and the way rclone copies (4 files at once, one
// 1 MiB read each, no lanes, never tuned). It skips unless QCLONE_DIAG_SRC
// names the demo tree; run it with `make tuner-diag` (a few minutes).

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"qclone/internal/plan"
	"qclone/internal/sim"
	"qclone/internal/tuner"
)

func TestDiagRuns(t *testing.T) {
	src := os.Getenv("QCLONE_DIAG_SRC")
	if src == "" {
		t.Skip()
	}
	cases := []struct {
		name string
		set  func(*Options)
	}{
		{"hash", func(o *Options) { o.Order = plan.OrderHash }},
		{"dfs", func(o *Options) { o.Order = plan.OrderDFS }},
		{"rclone-shape", func(o *Options) { o.Order, o.Fixed, o.NoLanes, o.Chunk = plan.OrderDFS, true, true, 1<<20 }},
	}
	for _, c := range cases {
		dst := t.TempDir()
		o := opts(dst + "/e.txt")
		o.Tuner = tuner.DefaultConfig()
		o.Queue = 0
		o.Sim = sim.NewDevice(sim.Degraded())
		c.set(&o)
		p, err := plan.Resolve([]string{src + "/", dst + "/d"})
		if err != nil {
			t.Fatal(err)
		}
		e := New(p, o)
		done := make(chan struct{})
		var samples, fly, slow, try, stuck int
		go func() {
			tick := time.NewTicker(100 * time.Millisecond)
			defer tick.Stop()
			for {
				select {
				case <-done:
					return
				case <-tick.C:
					s := e.Snapshot()
					samples++
					fly += s.InFly
					slow += s.InSlow
					try += s.InTry
					stuck += s.Stuck
				}
			}
		}()
		t0 := time.Now()
		e.Run(context.Background())
		el := time.Since(t0)
		close(done)
		s := e.Snapshot()
		n := float64(max(samples, 1))
		fmt.Printf("== %s: %.0fs, %.1f MB/s, %d files, %d errors\n", c.name, el.Seconds(), float64(s.BytesDone)/el.Seconds()/1e6, s.FilesDone, s.Errors)
		fmt.Printf("   queue %d (%s); on average %.1f flying, %.1f slow, %.1f trying, %.1f stuck; at most %d stuck; at the end W %d, R %d\n",
			s.Queue, s.QueueHow, float64(fly)/n, float64(slow)/n, float64(try)/n, float64(stuck)/n, s.PeakStuck, s.Tuner.Readers, s.SlowLane)
		for _, d := range s.Tuner.History {
			fmt.Printf("   %6.1fs W %2d -> %2d: %+5.0f%% %s\n", d.At.Seconds(), d.From, d.To, d.Gain*100, d.Note)
		}
		for _, d := range s.SlowTuner.History {
			fmt.Printf("   %6.1fs R %2d -> %2d: %+5.0f%% %s\n", d.At.Seconds(), d.From, d.To, d.Gain*100, d.Note)
		}
	}
}

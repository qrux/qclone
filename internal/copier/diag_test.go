package copier

// A diagnostic, not a test: it copies the ~1 GB demo tree through the simulated
// degraded drive and prints how long each copy took, what was in flight, and
// every decision the tuner made: in hash order, in dfs order, and the way
// rclone copies (4 reads in flight, 1 MiB each, no room for stuck reads, never
// tuned). It skips unless QCLONE_DIAG_SRC names the demo tree; run it with
// `make tuner-diag` (a few minutes).

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
		{"rclone-shape", func(o *Options) { o.Order, o.Fixed, o.MaxStuck, o.Chunk = plan.OrderDFS, true, 0, 1<<20 }},
	}
	for _, c := range cases {
		dst := t.TempDir()
		o := opts(dst + "/e.txt")
		o.Tuner = tuner.DefaultConfig()
		o.Sim = sim.NewDevice(sim.Degraded())
		c.set(&o)
		p, err := plan.Resolve([]string{src + "/", dst + "/d"})
		if err != nil {
			t.Fatal(err)
		}
		e := New(p, o)
		done := make(chan struct{})
		var samples, limit, stuck, reading int
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
					limit += s.Readers
					stuck += s.Stuck
					for i := range e.workers {
						w := &e.workers[i]
						w.mu.Lock()
						if w.start >= 0 {
							reading++
						}
						w.mu.Unlock()
					}
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
		fmt.Printf("   on average: %.1f reads allowed, %.1f reading, %.1f counted stuck; at most %d stuck; flying reads at the end %d\n",
			float64(limit)/n, float64(reading)/n, float64(stuck)/n, s.PeakStuck, s.Tuner.Readers)
		for _, d := range s.Tuner.History {
			fmt.Printf("   %6.1fs %2d -> %2d flying: %+5.0f%% %s\n", d.At.Seconds(), d.From, d.To, d.Gain*100, d.Note)
		}
	}
}

package copier

// Diagnostics, not tests: they copy the ~1 GB demo tree through the simulated
// degraded drive and print what the tuner decided (TestDiagTunerDecisions, hash
// and dfs order) and what pinned settings achieve (TestDiagPinnedSettings).
// They skip unless QCLONE_DIAG_SRC names the demo tree; run them with
// `make tuner-diag` (several minutes).

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

func TestDiagTunerDecisions(t *testing.T) {
	src := os.Getenv("QCLONE_DIAG_SRC")
	if src == "" {
		t.Skip()
	}
	for _, order := range []plan.Order{plan.OrderHash, plan.OrderDFS} {
		dst := t.TempDir()
		o := opts(dst + "/e.txt")
		o.Tuner = tuner.DefaultConfig()
		o.Start = tuner.Setting{Readers: 4, Chunk: 256 << 10, Files: 4}
		o.Sim = sim.NewDevice(sim.Degraded())
		o.Order = order
		p, _ := plan.Resolve([]string{src + "/", dst + "/d"})
		e := New(p, o)
		t0 := time.Now()
		e.Run(context.Background())
		fmt.Printf("== %s: %.0fs\n", order, time.Since(t0).Seconds())
		for _, d := range e.tuner.Status(0).History {
			fmt.Printf("  %6.1fs dim=%d %v -> %v gain=%+.2f %s\n", d.At.Seconds(), d.Dim, d.From, d.To, d.Gain, d.Note)
		}
	}
}

func TestDiagPinnedSettings(t *testing.T) {
	src := os.Getenv("QCLONE_DIAG_SRC")
	if src == "" {
		t.Skip()
	}
	for _, order := range []plan.Order{plan.OrderHash, plan.OrderDFS} {
		for _, r := range []int{4, 8, 16} {
			for _, files := range []int{1, 4} {
				dst := t.TempDir()
				o := opts(dst + "/e.txt")
				o.Tuner = tuner.DefaultConfig()
				o.Fixed = true
				o.Start = tuner.Setting{Readers: r, Chunk: 256 << 10, Files: files}
				o.Sim = sim.NewDevice(sim.Degraded())
				o.Order = order
				p, _ := plan.Resolve([]string{src + "/", dst + "/d"})
				e := New(p, o)
				ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
				e.Run(ctx)
				cancel()
				s := e.Snapshot()
				fmt.Printf("%-6s readers %2d files %d: %6.1f MB in 20 s (%5.2f MB/s), %d files done\n", order, r, files, float64(e.readBytes.Load())/1e6, float64(e.readBytes.Load())/20e6, s.FilesDone)
			}
		}
	}
}

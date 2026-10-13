package copier

import (
	"context"
	"sync"
	"sync/atomic"
	"time"
)

// The queue length Q is measured, not assumed: a drive reports its command
// queue, but a bridge in an enclosure can take fewer commands than the drive
// does, or pass them along one at a time. Small reads of fresh data show it:
// a SATA SSD's small reads per second keep climbing with reads in flight, all
// the way to its queue (spec sheets: about 10k 4 KiB reads a second with 1 in
// flight, 90-100k with 32), and past the real limit only the wait grows.

var (
	calibrateBudget = 5 * time.Second        // how long to look for fresh data before settling for 32
	calibrateStep   = 150 * time.Millisecond // per count of reads in flight
	calibrateSettle = 30 * time.Millisecond  // of each step, not counted
	calibrateCounts = []int{1, 2, 4, 8, 12, 16, 24, 32, 48, 64}
)

const calibrateRead = 4 << 10

// measureQueue sets Q before copying starts. It takes files from the walk,
// opening them for the copy as usual, and reads the first 4 KiB of each until
// one comes back fast; then it measures on that file (calibrate). Nothing else
// is in flight meanwhile. With no fresh data within calibrateBudget, Q stays
// at 32.
func (e *Engine) measureQueue(ctx context.Context, open *[]*region, walkDone *bool) {
	deadline := time.Now().Add(calibrateBudget)
	var b []byte
	buf := alignedBuf(&b, calibrateRead)
	for time.Now().Before(deadline) && ctx.Err() == nil {
		f, done := e.q.tryPop(time.Now())
		if f == nil {
			if done {
				*walkDone = true
				return
			}
			select {
			case <-ctx.Done():
				return
			case <-e.q.kick:
			case <-time.After(dispatchTick):
			}
			continue
		}
		*open = append(*open, e.startFile(f)...)
		if f.size < calibrateRead || e.open(f) != nil {
			continue
		}
		t0 := time.Now()
		if _, err := e.readAt(f, buf, 0); err != nil || time.Since(t0) > e.stuckAfter {
			continue
		}
		if q, ok := e.calibrate(ctx, f); ok {
			e.setQueue(q, "measured")
		}
		return
	}
}

// calibrate reads 4 KiB pieces of f, scattered over its first 64 MiB, at each
// count of reads in flight in calibrateCounts, and returns the smallest count
// that gets 95% of the best rate seen. ok is false if any read took longer
// than StuckAfter (not fresh data after all) or failed.
func (e *Engine) calibrate(ctx context.Context, f *file) (q int, ok bool) {
	blocks := uint64(min(f.size, 64<<20) / calibrateRead)
	if blocks == 0 {
		return 0, false
	}
	rates := make([]float64, len(calibrateCounts))
	best := 0.0
	var next atomic.Uint64
	for i, k := range calibrateCounts {
		start := time.Now()
		from, to := start.Add(calibrateSettle), start.Add(calibrateStep)
		var counted atomic.Int64
		var bad atomic.Bool
		stop := make(chan struct{})
		var wg sync.WaitGroup
		for g := 0; g < k; g++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				var b []byte
				buf := alignedBuf(&b, calibrateRead)
				for {
					select {
					case <-stop:
						return
					default:
					}
					off := int64(next.Add(1)*2654435761%blocks) * calibrateRead
					t0 := time.Now()
					if _, err := e.readAt(f, buf, off); err != nil {
						bad.Store(true)
						return
					}
					t1 := time.Now()
					if t1.Sub(t0) > e.stuckAfter {
						bad.Store(true)
					}
					if !t1.Before(from) && t1.Before(to) {
						counted.Add(1)
					}
				}
			}()
		}
		select {
		case <-ctx.Done():
		case <-time.After(time.Until(to)):
		}
		close(stop)
		wg.Wait()
		if ctx.Err() != nil || bad.Load() {
			return 0, false
		}
		rates[i] = float64(counted.Load()) / (calibrateStep - calibrateSettle).Seconds()
		best = max(best, rates[i])
	}
	for i, k := range calibrateCounts {
		if rates[i] >= 0.95*best {
			return k, true
		}
	}
	return calibrateCounts[len(calibrateCounts)-1], true
}

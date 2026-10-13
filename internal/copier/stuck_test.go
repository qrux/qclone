package copier

import (
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"qclone/internal/plan"
)

// concurrency counts reads in flight through a test's readFault: all of them,
// and those of files the test calls slow, which take slowFor each.
type concurrency struct {
	mu                         sync.Mutex
	all, slow, allMax, slowMax int
	slowDone, fastDone         int
	fastAt                     map[int]int // fast reads done when slowDone reached the key
}

func (c *concurrency) fault(isSlow func(string) bool, slowFor, fastFor time.Duration) func(string, int64, int) error {
	return func(show string, off int64, n int) error {
		slow := isSlow(show)
		c.mu.Lock()
		c.all++
		c.allMax = max(c.allMax, c.all)
		if slow {
			c.slow++
			c.slowMax = max(c.slowMax, c.slow)
		}
		c.mu.Unlock()
		if slow {
			time.Sleep(slowFor)
		} else {
			time.Sleep(fastFor)
		}
		c.mu.Lock()
		c.all--
		if slow {
			c.slow--
			c.slowDone++
			if c.fastAt != nil {
				if _, ok := c.fastAt[c.slowDone]; ok {
					c.fastAt[c.slowDone] = c.fastDone
				}
			}
		} else {
			c.fastDone++
		}
		c.mu.Unlock()
		return nil
	}
}

// One big file sits in an old region: each read takes 200 ms, and the drive
// can serve many at once. Sixteen fresh files read in a millisecond. With room
// for 16 stuck reads, the slow file fills that room while the 4 flying reads
// finish the fresh files. With none, the fresh files still go first (the slow
// file is found out after one read), but then the slow file gets only the 4
// flying reads.
func TestSlowFileGetsTheStuckRoom(t *testing.T) {
	base := t.TempDir()
	src := filepath.Join(base, "s")
	specs := []spec{{"a-slow.bin", 48 << 16, 0}} // 48 chunks of 64 KiB
	for i := 0; i < 16; i++ {
		specs = append(specs, spec{fmt.Sprintf("b-%02d.bin", i), 4 << 16, 0}) // 4 chunks each
	}
	makeTree(t, src, specs)
	try := func(maxStuck int) *concurrency {
		c := &concurrency{fastAt: map[int]int{24: -1}}
		o := fixed(filepath.Join(base, "e.txt"), 4, maxStuck, plan.OrderDFS)
		o.Chunk = 64 << 10
		dst := filepath.Join(base, fmt.Sprintf("d%d", maxStuck))
		e := runWith(t, []string{src + "/", dst}, o, c.fault(func(s string) bool { return s == "a-slow.bin" }, 200*time.Millisecond, time.Millisecond))
		if s := e.Snapshot(); s.FilesDone != 17 || s.Errors != 0 {
			t.Fatalf("max stuck %d: done %d errors %d (%s)", maxStuck, s.FilesDone, s.Errors, s.LastError)
		}
		sameTree(t, src, dst)
		return c
	}
	room, none := try(16), try(0)
	t.Logf("room for 16 stuck: slow file up to %d reads at once, %d in all, %d of 64 fresh chunks done when the slow file was half done", room.slowMax, room.allMax, room.fastAt[24])
	t.Logf("no room: slow file up to %d reads at once, %d in all", none.slowMax, none.allMax)
	if room.slowMax < 12 {
		t.Errorf("with room for 16 stuck, the slow file had at most %d reads in flight", room.slowMax)
	}
	if room.allMax > 20 {
		t.Errorf("%d reads in flight; 4 flying + 16 stuck is the most", room.allMax)
	}
	if room.fastAt[24] < 64 {
		t.Errorf("only %d of 64 fresh chunks went by during the slow file's first half", room.fastAt[24])
	}
	if none.slowMax > 4 || none.allMax > 4 {
		t.Errorf("with no room for stuck reads: slow file %d at once, %d in all; 4 is the most", none.slowMax, none.allMax)
	}
}

// When every read is slow, reads in flight grow past the 4 flying ones to make
// room for 16 stuck ones, and no further.
func TestEverythingStuckStaysWithinTheRoom(t *testing.T) {
	base := t.TempDir()
	src := filepath.Join(base, "s")
	var specs []spec
	for i := 0; i < 6; i++ {
		specs = append(specs, spec{fmt.Sprintf("f%d.bin", i), 8 << 16, 0}) // 8 chunks of 64 KiB
	}
	makeTree(t, src, specs)
	c := &concurrency{}
	o := fixed(filepath.Join(base, "e.txt"), 4, 16, plan.OrderDFS)
	o.Chunk = 64 << 10
	dst := filepath.Join(base, "d")
	e := runWith(t, []string{src + "/", dst}, o, c.fault(func(string) bool { return true }, 100*time.Millisecond, 0))
	s := e.Snapshot()
	if s.FilesDone != 6 || s.Errors != 0 {
		t.Fatalf("done %d errors %d (%s)", s.FilesDone, s.Errors, s.LastError)
	}
	sameTree(t, src, dst)
	t.Logf("%d reads in flight at most; %d counted stuck at once", c.allMax, s.PeakStuck)
	if c.allMax < 17 || c.allMax > 20 {
		t.Errorf("%d reads in flight at most; want more than the 4 flying and no more than 4 + 16", c.allMax)
	}
	if s.Stuck != 0 {
		t.Errorf("%d reads still counted stuck after the run", s.Stuck)
	}
}

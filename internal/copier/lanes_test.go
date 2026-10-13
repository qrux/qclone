package copier

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"qclone/internal/plan"
)

// drive stands in for the source in readFault: slow reads take slowFor and the
// rest fastFor, and it counts reads in flight, in all and of each file.
type drive struct {
	slow             func(show string, off int64) bool
	slowFor, fastFor time.Duration

	mu                 sync.Mutex
	all, allMax        int
	per, perMax        map[string]int
	slowDone           int
	fastDone           int
	fastAtSlow         map[int]int // fast reads done when slowDone reached the key
	lastFast           time.Time
	firstFast          time.Time
	slowBeforeFastDone int // the most reads of slow files at once while fast reads were still to come
	fastLeft           int
}

func newDrive(slow func(string, int64) bool, slowFor, fastFor time.Duration, fastTotal int) *drive {
	return &drive{slow: slow, slowFor: slowFor, fastFor: fastFor, per: map[string]int{}, perMax: map[string]int{}, fastAtSlow: map[int]int{}, fastLeft: fastTotal}
}

func (d *drive) read(show string, off int64, n int) error {
	slow := d.slow(show, off)
	d.mu.Lock()
	d.all++
	d.allMax = max(d.allMax, d.all)
	d.per[show]++
	d.perMax[show] = max(d.perMax[show], d.per[show])
	if slow && d.fastLeft > 0 {
		inSlow := 0
		for name, k := range d.per {
			if strings.HasPrefix(name, "a-") {
				inSlow += k
			}
		}
		d.slowBeforeFastDone = max(d.slowBeforeFastDone, inSlow)
	}
	d.mu.Unlock()
	if slow {
		time.Sleep(d.slowFor)
	} else {
		time.Sleep(d.fastFor)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.all--
	d.per[show]--
	if slow {
		d.slowDone++
		if _, ok := d.fastAtSlow[d.slowDone]; ok {
			d.fastAtSlow[d.slowDone] = d.fastDone
		}
	} else {
		d.fastDone++
		d.fastLeft--
		d.lastFast = time.Now()
		if d.firstFast.IsZero() {
			d.firstFast = d.lastFast
		}
	}
	return nil
}

// runPolled runs a copy and calls poll with a snapshot every few milliseconds.
func runPolled(t *testing.T, args []string, o Options, fault func(string, int64, int) error, poll func(time.Time, Snapshot)) *Engine {
	t.Helper()
	p, err := plan.Resolve(args)
	if err != nil {
		t.Fatal(err)
	}
	e := New(p, o)
	e.readFault = fault
	done := make(chan struct{})
	go func() {
		defer close(done)
		tick := time.NewTicker(5 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-tick.C:
				poll(time.Now(), e.Snapshot())
			case <-e.quit:
				return
			}
		}
	}()
	if err := e.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	<-done
	o.Errors.Close()
	return e
}

func checkRun(t *testing.T, e *Engine, files int64, src, dst string) {
	t.Helper()
	if s := e.Snapshot(); s.FilesDone != files || s.Errors != 0 || s.Stuck != 0 {
		t.Fatalf("done %d of %d, errors %d (%s), %d still counted stuck", s.FilesDone, files, s.Errors, s.LastError, s.Stuck)
	}
	sameTree(t, src, dst)
}

// One big file sits in an old region (200 ms a read, served in parallel);
// sixteen fresh files take 10 ms a read. While fresh work is waiting, the slow
// file keeps exactly its slow lane (4) and the fresh files go by in the flying
// slots; once they're done, slow reads fill the queue.
func TestFreshFilesGoByTheSlowLane(t *testing.T) {
	base := t.TempDir()
	src := filepath.Join(base, "s")
	specs := []spec{{"a-slow.bin", 48 << 16, 0}} // 48 chunks of 64 KiB
	for i := 0; i < 16; i++ {
		specs = append(specs, spec{fmt.Sprintf("b-%02d.bin", i), 4 << 16, 0})
	}
	makeTree(t, src, specs)
	d := newDrive(func(s string, _ int64) bool { return strings.HasPrefix(s, "a-") }, 200*time.Millisecond, 10*time.Millisecond, 64)
	d.fastAtSlow[24] = -1
	dst := filepath.Join(base, "d")
	e := runWith(t, []string{src + "/", dst}, lanes(filepath.Join(base, "e.txt"), 4, 4, 16, plan.OrderDFS), d.read)
	checkRun(t, e, 17, src, dst)
	t.Logf("slow file: at most %d reads at once while fresh work remained, %d overall; %d of 64 fresh chunks done when it was half done",
		d.slowBeforeFastDone, d.perMax["a-slow.bin"], d.fastAtSlow[24])
	if d.fastAtSlow[24] < 64 {
		t.Errorf("only %d of 64 fresh chunks went by during the slow file's first half", d.fastAtSlow[24])
	}
	if d.slowBeforeFastDone < 4 || d.slowBeforeFastDone > 4+1 { // the slow lane, plus the try that found it slow
		t.Errorf("the slow file had up to %d reads at once while fresh work remained; want its slow lane of 4", d.slowBeforeFastDone)
	}
	if d.perMax["a-slow.bin"] < 12 || d.allMax > 16 {
		t.Errorf("slow file up to %d at once, %d in all; with nothing fresh left it should fill the queue of 16, and no more", d.perMax["a-slow.bin"], d.allMax)
	}
}

// Old files fill every slot while nothing fresh is known. Once a fresh file
// turns up, slow reads back off to the slow lane (4) as they finish, and stay
// there while fresh work lasts.
func TestSlowBacksOffWhenFreshTurnsUp(t *testing.T) {
	base := t.TempDir()
	src := filepath.Join(base, "s")
	var specs []spec
	for i := 0; i < 20; i++ {
		specs = append(specs, spec{fmt.Sprintf("a-%02d.bin", i), 4 << 16, 0})
	}
	for i := 0; i < 16; i++ {
		specs = append(specs, spec{fmt.Sprintf("b-%02d.bin", i), 8 << 16, 0})
	}
	makeTree(t, src, specs)
	d := newDrive(func(s string, _ int64) bool { return strings.HasPrefix(s, "a-") }, 100*time.Millisecond, 20*time.Millisecond, 128)
	type sample struct {
		at     time.Time
		inSlow int
	}
	var mu sync.Mutex
	var samples []sample
	dst := filepath.Join(base, "d")
	e := runPolled(t, []string{src + "/", dst}, lanes(filepath.Join(base, "e.txt"), 4, 4, 8, plan.OrderDFS), d.read, func(at time.Time, s Snapshot) {
		mu.Lock()
		samples = append(samples, sample{at, s.InSlow})
		mu.Unlock()
	})
	checkRun(t, e, 36, src, dst)
	from, to := d.firstFast.Add(250*time.Millisecond), d.lastFast
	most := 0
	for _, s := range samples {
		if s.at.After(from) && s.at.Before(to) {
			most = max(most, s.inSlow)
		}
	}
	t.Logf("before anything fresh: up to %d reads at once; once fresh work turned up: at most %d slow reads (over %v)", d.allMax, most, to.Sub(from).Round(time.Millisecond))
	if d.allMax != 8 {
		t.Errorf("at most %d reads at once; old data alone should fill the queue of 8, and no more", d.allMax)
	}
	if to.Sub(from) < 200*time.Millisecond {
		t.Fatalf("fresh phase too short to judge (%v)", to.Sub(from))
	}
	if most > 4 {
		t.Errorf("%d slow reads in flight while fresh work remained; they should have backed off to the slow lane of 4", most)
	}
}

// A big file with old regions (0 and 4 of 8) and fresh ones: the fresh regions
// are found by breadth-first tries and read in the flying slots while the old
// ones grind in the slow lane.
func TestRegionsAreJudgedApart(t *testing.T) {
	base := t.TempDir()
	src := filepath.Join(base, "s")
	makeTree(t, src, []spec{{"big.bin", 8 << 18, 0}}) // 8 regions of 256 KiB, 4 chunks each
	slowAt := func(_ string, off int64) bool { r := off / (256 << 10); return r == 0 || r == 4 }
	d := newDrive(slowAt, 200*time.Millisecond, time.Millisecond, 24)
	d.fastAtSlow[4] = -1
	o := lanes(filepath.Join(base, "e.txt"), 4, 2, 12, plan.OrderDFS)
	o.RegionSize = 256 << 10
	dst := filepath.Join(base, "d")
	e := runWith(t, []string{src + "/", dst}, o, d.read)
	checkRun(t, e, 1, src, dst)
	t.Logf("%d of 24 fresh chunks done when the old regions were half done", d.fastAtSlow[4])
	if d.fastAtSlow[4] < 24 {
		t.Errorf("only %d of 24 fresh chunks went by while the old regions' first half was read", d.fastAtSlow[4])
	}
}

// --no-lanes reads like rclone: files in order, readers at a time, one read
// each, slow or not.
func TestNoLanesReadsLikeRclone(t *testing.T) {
	base := t.TempDir()
	src := filepath.Join(base, "s")
	specs := []spec{{"a-slow.bin", 8 << 16, 0}}
	for i := 0; i < 3; i++ {
		specs = append(specs, spec{fmt.Sprintf("b-%d.bin", i), 4 << 16, 0})
	}
	makeTree(t, src, specs)
	d := newDrive(func(s string, _ int64) bool { return strings.HasPrefix(s, "a-") }, 100*time.Millisecond, time.Millisecond, 12)
	o := inOrder(filepath.Join(base, "e.txt"), 4, plan.OrderDFS)
	o.Chunk = 64 << 10
	dst := filepath.Join(base, "d")
	e := runWith(t, []string{src + "/", dst}, o, d.read)
	checkRun(t, e, 4, src, dst)
	if d.perMax["a-slow.bin"] != 1 || d.allMax > 4 {
		t.Errorf("slow file %d reads at once, %d in all; want 1 per file, 4 in all", d.perMax["a-slow.bin"], d.allMax)
	}
}

func TestBreadthFirst(t *testing.T) {
	if got := fmt.Sprint(breadthFirst(8)); got != "[0 4 2 6 1 3 5 7]" {
		t.Errorf("breadthFirst(8) = %s", got)
	}
	if got := fmt.Sprint(breadthFirst(5)); got != "[0 2 1 3 4]" {
		t.Errorf("breadthFirst(5) = %s", got)
	}
	for n := 0; n < 200; n++ {
		seen := map[int]bool{}
		for _, i := range breadthFirst(n) {
			if i < 0 || i >= n || seen[i] {
				t.Fatalf("breadthFirst(%d) has %d out of range or twice", n, i)
			}
			seen[i] = true
		}
		if len(seen) != n {
			t.Fatalf("breadthFirst(%d) lists %d", n, len(seen))
		}
	}
}

// A drive that works on 8 reads at once (2 ms each): the queue measured at the
// start comes out at 8, give or take one step of the counts tried.
func TestQueueMeasured(t *testing.T) {
	base := t.TempDir()
	src := filepath.Join(base, "s")
	makeTree(t, src, []spec{{"f.bin", 1 << 20, 0}})
	slots := make(chan struct{}, 8)
	fault := func(string, int64, int) error {
		slots <- struct{}{}
		time.Sleep(2 * time.Millisecond)
		<-slots
		return nil
	}
	o := opts(filepath.Join(base, "e.txt"))
	o.Queue = 0
	dst := filepath.Join(base, "d")
	e := runWith(t, []string{src + "/", dst}, o, fault)
	checkRun(t, e, 1, src, dst)
	s := e.Snapshot()
	t.Logf("queue %d, %s", s.Queue, s.QueueHow)
	if s.QueueHow != "measured" || s.Queue < 8 || s.Queue > 12 {
		t.Errorf("queue %d (%s); want 8 measured", s.Queue, s.QueueHow)
	}
}

// With no fresh data to measure on, the queue stays at 32.
func TestQueueNotMeasuredWithoutFreshData(t *testing.T) {
	defer func(b time.Duration) { calibrateBudget = b }(calibrateBudget)
	calibrateBudget = 300 * time.Millisecond
	base := t.TempDir()
	src := filepath.Join(base, "s")
	makeTree(t, src, []spec{{"a.bin", 64 << 10, 0}, {"b.bin", 64 << 10, 0}, {"c.bin", 64 << 10, 0}, {"d.bin", 64 << 10, 0}})
	o := opts(filepath.Join(base, "e.txt"))
	o.Queue = 0
	dst := filepath.Join(base, "d")
	e := runWith(t, []string{src + "/", dst}, o, func(string, int64, int) error { time.Sleep(100 * time.Millisecond); return nil })
	checkRun(t, e, 4, src, dst)
	if s := e.Snapshot(); s.Queue != 32 || s.QueueHow != "default" {
		t.Errorf("queue %d (%s); want 32, not measured", s.Queue, s.QueueHow)
	}
}

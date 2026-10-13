package copier

import (
	"fmt"
	"hash/fnv"
	"io"
	"path/filepath"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"qclone/internal/plan"
	"qclone/internal/tuner"
)

func fixed(errPath string, readers, files int, order plan.Order) Options {
	o := opts(errPath)
	o.Fixed = true
	o.Start = tuner.Setting{Readers: readers, Chunk: 128 << 10, Files: files}
	o.Tuner.MinChunk, o.Tuner.MaxChunk = 128<<10, 128<<10
	o.Order = order
	return o
}

// One file sits in an old region whose reads the drive serves one at a time;
// many fresh files queue behind it. With one file in flight every reader
// waits on the slow file; with several, the fast files go by meanwhile.
func TestSlowFileDoesNotHoldEveryReader(t *testing.T) {
	base := t.TempDir()
	src := filepath.Join(base, "s")
	specs := []spec{{"a-slow.bin", 32 << 17, 0}} // 32 chunks
	for i := 0; i < 16; i++ {
		specs = append(specs, spec{fmt.Sprintf("b-%02d.bin", i), 4 << 17, 0}) // 4 chunks each
	}
	makeTree(t, src, specs)
	fastDuringFirstHalf := func(files int) int64 {
		var slowMu sync.Mutex
		var slowDone, fast, atHalf atomic.Int64
		atHalf.Store(-1)
		fault := func(show string, off int64, n int) error {
			if show == "a-slow.bin" {
				slowMu.Lock() // the drive grinds through these one at a time
				time.Sleep(20 * time.Millisecond)
				if slowDone.Add(1) == 16 {
					atHalf.Store(fast.Load())
				}
				slowMu.Unlock()
				return nil
			}
			fast.Add(1)
			time.Sleep(2 * time.Millisecond)
			return nil
		}
		dst := filepath.Join(base, fmt.Sprintf("d%d", files))
		e := runWith(t, []string{src + "/", dst}, fixed(filepath.Join(base, "e.txt"), 8, files, plan.OrderDFS), fault)
		if s := e.Snapshot(); s.FilesDone != 17 || s.Errors != 0 {
			t.Fatalf("files=%d: done %d errors %d (%s)", files, s.FilesDone, s.Errors, s.LastError)
		}
		sameTree(t, src, dst)
		return atHalf.Load()
	}
	one, four := fastDuringFirstHalf(1), fastDuringFirstHalf(4)
	t.Logf("fast chunks read during the slow file's first half: 1 file in flight %d, 4 files in flight %d (of 64)", one, four)
	if one != 0 {
		t.Errorf("with 1 file in flight, %d fast chunks were read while the slow file still had chunks to hand out", one)
	}
	if four < 48 {
		t.Errorf("with 4 files in flight, only %d of 64 fast chunks went by during the slow file's first half", four)
	}
}

func TestOrders(t *testing.T) {
	base := t.TempDir()
	src := filepath.Join(base, "s")
	var specs []spec
	var names []string
	for i := 0; i < 40; i++ {
		n := filepath.Join(fmt.Sprintf("d%d", i%4), fmt.Sprintf("f%02d.txt", i))
		specs = append(specs, spec{n, 100, 0})
		names = append(names, n)
	}
	makeTree(t, src, specs)
	seen := func(order plan.Order) []string {
		var mu sync.Mutex
		var got []string
		fault := func(show string, off int64, n int) error {
			mu.Lock()
			got = append(got, show)
			mu.Unlock()
			return nil
		}
		runWith(t, []string{src + "/", filepath.Join(base, "d-"+order.String())}, fixed(filepath.Join(base, "e.txt"), 1, 1, order), fault)
		return got
	}
	dfs := append([]string(nil), names...)
	sort.Strings(dfs)
	if got := seen(plan.OrderDFS); fmt.Sprint(got) != fmt.Sprint(dfs) {
		t.Errorf("dfs order:\n got %v\nwant %v", got, dfs)
	}
	byHash := append([]string(nil), names...)
	key := func(s string) uint64 { h := fnv.New64a(); io.WriteString(h, s); return h.Sum64() }
	sort.Slice(byHash, func(i, j int) bool { return key(byHash[i]) < key(byHash[j]) })
	if got := seen(plan.OrderHash); fmt.Sprint(got) != fmt.Sprint(byHash) {
		t.Errorf("hash order:\n got %v\nwant %v", got, byHash)
	}
	if got := seen(plan.OrderDirent); len(got) != 40 {
		t.Errorf("dirent order copied %d files, want 40", len(got))
	}
}

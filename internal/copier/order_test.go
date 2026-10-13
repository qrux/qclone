package copier

import (
	"fmt"
	"hash/fnv"
	"io"
	"path/filepath"
	"sort"
	"sync"
	"testing"

	"qclone/internal/plan"
)

// lanes: fixed numbers of flying (w) and slow-lane (r) reads, in a queue of q.
func lanes(errPath string, w, r, q int, order plan.Order) Options {
	o := opts(errPath)
	o.Fixed, o.SlowFixed = true, true
	o.Readers, o.SlowLane, o.Queue = w, r, q
	o.Chunk = 64 << 10
	o.Order = order
	return o
}

// inOrder: rclone's way, readers files at a time, one read each.
func inOrder(errPath string, readers int, order plan.Order) Options {
	o := opts(errPath)
	o.Fixed, o.NoLanes = true, true
	o.Readers = readers
	o.Chunk = 128 << 10
	o.Order = order
	return o
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
		runWith(t, []string{src + "/", filepath.Join(base, "d-"+order.String())}, inOrder(filepath.Join(base, "e.txt"), 1, order), fault)
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

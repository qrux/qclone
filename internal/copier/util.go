package copier

import (
	"container/heap"
	"math"
	"sync"
	"time"
	"unsafe"
)

// gate admits up to limit workers at once; the limit can change at any time.
// Lowering it doesn't interrupt anyone: the extra workers finish their chunk
// and then wait.
type gate struct {
	mu       sync.Mutex
	cond     *sync.Cond
	limit    int
	inflight int
	open     bool
}

func newGate(limit int) *gate {
	g := &gate{limit: limit}
	g.cond = sync.NewCond(&g.mu)
	return g
}

func (g *gate) acquire() {
	g.mu.Lock()
	for !g.open && g.inflight >= g.limit {
		g.cond.Wait()
	}
	g.inflight++
	g.mu.Unlock()
}

func (g *gate) release() {
	g.mu.Lock()
	g.inflight--
	g.mu.Unlock()
	g.cond.Signal()
}

func (g *gate) setLimit(n int) {
	g.mu.Lock()
	changed := n != g.limit
	g.limit = n
	g.mu.Unlock()
	if changed {
		g.cond.Broadcast()
	}
}

func (g *gate) limitNow() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.limit
}

// openAll lets everyone through, so workers can drain the queue on cancel.
func (g *gate) openAll() {
	g.mu.Lock()
	g.open = true
	g.mu.Unlock()
	g.cond.Broadcast()
}

// windowPatience is how long the queue waits for the walk to fill its window
// before handing out a file from a partial one.
const windowPatience = 500 * time.Millisecond

// fileQueue sits between the walk and the dispatcher. It hands out, from the
// next `window` files the walk has found, the one with the lowest key: in hash
// order the key is a hash of the path, so files come out scattered across
// folders; otherwise it's the order found, and the window is 1. The walk can
// run ahead without limit, so the totals are known early.
type fileQueue struct {
	mu       sync.Mutex
	fifo     []*file
	head     int
	win      fileHeap
	window   int
	closed   bool
	lastPush time.Time
	kick     chan struct{} // nudges the dispatcher: a file arrived, or the walk ended
}

func (q *fileQueue) init(window int) {
	if window < 1 {
		window = 1
	}
	q.window = window
	q.kick = make(chan struct{}, 1)
}

func (q *fileQueue) nudge() {
	select {
	case q.kick <- struct{}{}:
	default:
	}
}

func (q *fileQueue) push(f *file) {
	q.mu.Lock()
	q.fifo = append(q.fifo, f)
	q.lastPush = time.Now()
	q.mu.Unlock()
	q.nudge()
}

func (q *fileQueue) close() {
	q.mu.Lock()
	q.closed = true
	q.mu.Unlock()
	q.nudge()
}

// tryPop returns the next file if one may go now. done means the walk has
// ended and nothing is left. While the walk is still finding files it holds
// out for a full window, unless nothing new has turned up for a moment.
func (q *fileQueue) tryPop(now time.Time) (f *file, done bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for len(q.win) < q.window && q.head < len(q.fifo) {
		heap.Push(&q.win, q.fifo[q.head])
		q.fifo[q.head] = nil
		q.head++
	}
	if q.head > 4096 && q.head*2 > len(q.fifo) {
		q.fifo = append(q.fifo[:0], q.fifo[q.head:]...)
		q.head = 0
	}
	if len(q.win) == 0 {
		return nil, q.closed && q.head == len(q.fifo)
	}
	if len(q.win) < q.window && !q.closed && now.Sub(q.lastPush) < windowPatience {
		return nil, false
	}
	return heap.Pop(&q.win).(*file), false
}

type fileHeap []*file

func (h fileHeap) Len() int { return len(h) }
func (h fileHeap) Less(i, j int) bool {
	if h[i].key != h[j].key {
		return h[i].key < h[j].key
	}
	return h[i].seq < h[j].seq
}
func (h fileHeap) Swap(i, j int)       { h[i], h[j] = h[j], h[i] }
func (h *fileHeap) Push(x interface{}) { *h = append(*h, x.(*file)) }
func (h *fileHeap) Pop() interface{} {
	old := *h
	f := old[len(old)-1]
	old[len(old)-1] = nil
	*h = old[:len(old)-1]
	return f
}

func unsafePointer(b []byte) unsafe.Pointer { return unsafe.Pointer(&b[0]) }

func float64bits(f float64) uint64     { return math.Float64bits(f) }
func float64frombits(b uint64) float64 { return math.Float64frombits(b) }

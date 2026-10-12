package copier

import (
	"math"
	"sync"
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

// fileQueue is an unbounded FIFO between the walk and the dispatcher, so the
// walk can run ahead and the totals are known early.
type fileQueue struct {
	mu     sync.Mutex
	cond   *sync.Cond
	items  []*file
	head   int
	closed bool
}

func (q *fileQueue) init() { q.cond = sync.NewCond(&q.mu) }

func (q *fileQueue) push(f *file) {
	q.mu.Lock()
	q.items = append(q.items, f)
	q.mu.Unlock()
	q.cond.Signal()
}

// pop waits for the next file; false means the queue is closed and empty.
func (q *fileQueue) pop() (*file, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for q.head == len(q.items) && !q.closed {
		q.cond.Wait()
	}
	if q.head == len(q.items) {
		return nil, false
	}
	f := q.items[q.head]
	q.items[q.head] = nil
	q.head++
	if q.head > 4096 && q.head*2 > len(q.items) {
		q.items = append(q.items[:0], q.items[q.head:]...)
		q.head = 0
	}
	return f, true
}

func (q *fileQueue) close() {
	q.mu.Lock()
	q.closed = true
	q.mu.Unlock()
	q.cond.Broadcast()
}

func unsafePointer(b []byte) unsafe.Pointer { return unsafe.Pointer(&b[0]) }

func float64bits(f float64) uint64     { return math.Float64bits(f) }
func float64frombits(b uint64) float64 { return math.Float64frombits(b) }

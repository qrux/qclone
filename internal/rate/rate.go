// Package rate estimates a transfer rate that does not read zero once bytes
// have moved.
//
// A plain "bytes in the last second" meter drops to zero whenever a read takes
// longer than the window, and on a degraded SSD single reads can take seconds.
// Meter instead measures from a baseline event: the window reaches back at
// least MinWindow, and further if needed until it holds MinEvents completed
// writes. While a read stalls, the window keeps growing, so the rate decays
// toward the true average rather than snapping to zero.
package rate

import (
	"sort"
	"sync"
	"time"
)

const (
	// DefaultMinWindow is the shortest span the rate is measured over.
	DefaultMinWindow = 4 * time.Second
	// DefaultMinEvents is how many completions the window must contain.
	DefaultMinEvents = 4
)

type event struct {
	t   time.Time
	cum int64 // total bytes after this event
}

// Meter is safe for concurrent use.
type Meter struct {
	mu        sync.Mutex
	start     time.Time
	first     time.Time
	ev        []event
	head      int
	floor     event // the newest event pruned from ev; zero-valued before any pruning
	total     int64
	minWindow time.Duration
	minEvents int
}

// New returns a meter whose clock starts at start.
func New(start time.Time) *Meter {
	return NewWith(start, DefaultMinWindow, DefaultMinEvents)
}

// NewWith returns a meter with a custom window and event floor.
func NewWith(start time.Time, minWindow time.Duration, minEvents int) *Meter {
	if minEvents < 1 {
		minEvents = 1
	}
	return &Meter{start: start, floor: event{t: start}, minWindow: minWindow, minEvents: minEvents}
}

// Add records n bytes completed at now.
func (m *Meter) Add(n int64, now time.Time) {
	if n <= 0 {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.total == 0 {
		m.first = now
	}
	m.total += n
	m.ev = append(m.ev, event{t: now, cum: m.total})
	m.prune(now)
}

// prune drops events that can no longer be a baseline. The baseline is the
// newest event at or before now-minWindow, but never one of the last
// minEvents, so ev[head] can go once ev[head+1] is old enough to replace it.
func (m *Meter) prune(now time.Time) {
	cut := now.Add(-m.minWindow)
	for len(m.ev)-m.head > m.minEvents+1 && !m.ev[m.head+1].t.After(cut) {
		m.floor = m.ev[m.head]
		m.head++
	}
	if m.head > 1024 && m.head*2 > len(m.ev) {
		m.ev = append(m.ev[:0], m.ev[m.head:]...)
		m.head = 0
	}
}

// Rate returns bytes per second at now. ok is false until the first byte.
// Once ok is true the rate is always positive.
func (m *Meter) Rate(now time.Time) (bps float64, ok bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.total == 0 {
		return 0, false
	}
	ev := m.ev[m.head:]
	cut := now.Add(-m.minWindow)
	// i: newest event at or before cut (-1 means the floor).
	i := sort.Search(len(ev), func(k int) bool { return ev[k].t.After(cut) }) - 1
	// Keep at least minEvents completions after the baseline.
	if j := len(ev) - 1 - m.minEvents; j < i {
		i = j
	}
	base := m.floor
	if i >= 0 {
		base = ev[i]
	}
	span := now.Sub(base.t).Seconds()
	bytes := float64(m.total - base.cum)
	if bytes <= 0 {
		// Every recorded event is the baseline itself (only possible when the
		// floor is the last event); fall back to the whole run.
		bytes = float64(m.total)
		span = now.Sub(m.start).Seconds()
	}
	if span < 1e-3 {
		span = 1e-3
	}
	return bytes / span, true
}

// Average is total bytes over time since start; ok is false until the first byte.
func (m *Meter) Average(now time.Time) (bps float64, ok bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.total == 0 {
		return 0, false
	}
	span := now.Sub(m.start).Seconds()
	if span < 1e-3 {
		span = 1e-3
	}
	return float64(m.total) / span, true
}

// Total returns the bytes recorded so far.
func (m *Meter) Total() int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.total
}

// Idle returns how long it has been since the last completion (zero before the first).
func (m *Meter) Idle(now time.Time) time.Duration {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.ev) == m.head {
		if m.total == 0 {
			return 0
		}
		return now.Sub(m.floor.t)
	}
	return now.Sub(m.ev[len(m.ev)-1].t)
}

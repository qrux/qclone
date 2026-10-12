package ui

import "time"

// History keeps one sample per second of the run: the rate shown and the
// readers in use, for the chart.
type History struct {
	start   time.Time
	secs    []sample
	pending sample
	n       int
}

type sample struct {
	rate    float64 // mean of the positive rates shown during that second
	readers int     // most readers in use during that second
}

// NewHistory starts the clock.
func NewHistory(start time.Time) *History { return &History{start: start} }

// Record notes what one frame showed.
func (h *History) Record(now time.Time, rate float64, ok bool, readers int) {
	sec := int(now.Sub(h.start) / time.Second)
	for len(h.secs) < sec {
		h.flush()
	}
	if ok && rate > 0 {
		h.pending.rate += rate
		h.n++
	}
	if readers > h.pending.readers {
		h.pending.readers = readers
	}
}

func (h *History) flush() {
	s := h.pending
	if h.n > 0 {
		s.rate /= float64(h.n)
	}
	if s.readers == 0 && len(h.secs) > 0 {
		s.readers = h.secs[len(h.secs)-1].readers
	}
	h.secs = append(h.secs, s)
	h.pending, h.n = sample{}, 0
}

// Seconds is how many whole seconds are recorded.
func (h *History) Seconds() int { return len(h.secs) }

// column is one chart column's worth of seconds.
type column struct {
	rate    float64 // 0 when there was no rate yet
	readers int
}

// columns returns n columns of spc seconds each, newest last, right-aligned;
// columns before the run started are empty.
func (h *History) columns(n, spc int) []column {
	out := make([]column, n)
	secs := h.secs
	if h.n > 0 {
		// Include the second in progress, so the newest data shows at once.
		cur := h.pending
		cur.rate /= float64(h.n)
		secs = append(secs[:len(secs):len(secs)], cur)
	}
	end := len(secs)
	for i := n - 1; i >= 0; i-- {
		hi := end - (n-1-i)*spc
		lo := hi - spc
		if hi <= 0 {
			break
		}
		if lo < 0 {
			lo = 0
		}
		var sum float64
		var k, r int
		for _, s := range secs[lo:hi] {
			if s.rate > 0 {
				sum += s.rate
				k++
			}
			if s.readers > r {
				r = s.readers
			}
		}
		if k > 0 {
			out[i].rate = sum / float64(k)
		}
		out[i].readers = r
	}
	return out
}

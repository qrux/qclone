package ui

import (
	"fmt"
	"math"
	"strings"
	"time"
	"unicode/utf8"

	"qclone/internal/copier"
	"qclone/internal/tuner"
)

// Frame is the fixed context of the display.
type Frame struct {
	W, H    int
	Color   bool
	Sources []string // as typed
	Dst     string
}

type renderer struct {
	f Frame
	s copier.Snapshot
	h *History
	W int
}

func (r *renderer) nl() *line { return &line{color: r.f.Color} }

const labelW = 8 // " SOURCE " and the chart's "  512M |" prefix

// Render draws one frame as lines, each at most f.W columns wide.
func Render(f Frame, s copier.Snapshot, h *History) []string {
	W := f.W
	if W > 160 {
		W = 160
	}
	if W < 20 {
		W = 20
	}
	r := &renderer{f: f, s: s, h: h, W: W}

	top := append(r.header(), r.total()...)
	tun := r.tuner()
	disks := r.disks()
	errs := r.errors()
	nActive := len(s.Active)
	if nActive > 4 {
		nActive = 4
	}

	// Height: give the chart what's left after everything else and a few file lines.
	H := f.H
	fixed := len(top) + 1 + 1 + 1 + len(tun) + 1 + len(disks) + 1 + 1 + nActive + 1 + len(errs) + 1
	readerRows := 3
	chartRows := H - fixed - readerRows - 1
	if chartRows < 4 {
		readerRows = 0
		chartRows = H - fixed - 1
	}
	if chartRows > 12 {
		chartRows = 12
	}
	var out []*line
	add := func(ls ...*line) { out = append(out, ls...) }
	blank := func() { add(r.nl()) }

	add(top...)
	blank()
	add(r.speedHeader())
	if chartRows >= 2 {
		add(r.chart(chartRows, readerRows)...)
	}
	blank()
	add(tun...)
	blank()
	add(disks...)
	blank()
	room := H - len(out) - 1 - len(errs) - 1
	add(r.files(room)...)
	blank()
	add(errs...)
	if len(out) < H {
		add(r.footer())
	}
	if len(out) > H && H > 0 {
		out = out[:H]
	}
	lines := make([]string, len(out))
	for i, l := range out {
		lines[i] = l.String()
		if l.w > W {
			lines[i] = clip(lines[i], W)
		}
	}
	return lines
}

// clip cuts a line with color codes to w visible characters.
func clip(s string, w int) string {
	var b strings.Builder
	n, esc := 0, false
	for i := 0; i < len(s); {
		if s[i] == 0x1b {
			j := i + 1
			for j < len(s) && !(s[j] >= 'a' && s[j] <= 'z' || s[j] >= 'A' && s[j] <= 'Z') {
				j++
			}
			if j < len(s) {
				j++
			}
			b.WriteString(s[i:j])
			esc = true
			i = j
			continue
		}
		if n == w {
			break
		}
		_, size := utf8.DecodeRuneInString(s[i:])
		b.WriteString(s[i : i+size])
		n++
		i += size
	}
	if esc {
		b.WriteString("\x1b[0m")
	}
	return b.String()
}

func (r *renderer) header() []*line {
	s := r.s
	l := r.nl()
	l.add(inverse, " qclone ")
	if s.Rescue {
		l.add(bred, " rescue")
	}
	l.add(plain, "  ")
	src := ""
	if len(r.f.Sources) == 1 {
		src = r.f.Sources[0]
	} else {
		src = fmt.Sprintf("%d sources", len(r.f.Sources))
	}
	state := "elapsed " + Clock(s.Now.Sub(s.Start))
	st := plain
	switch {
	case s.Canceled:
		state, st = "CANCELED after "+Clock(s.Now.Sub(s.Start)), byellow
	case s.Done:
		state, st = "DONE in "+Clock(s.Now.Sub(s.Start)), bgreen
	}
	room := r.W - l.w - len(state) - 2 - len(" -> ")
	a, b := Clean(src), Clean(r.f.Dst)
	if len(a)+len(b) > room {
		half := room / 2
		switch {
		case len(a) <= half:
			b = TruncLeft(b, room-len(a))
		case len(b) <= half:
			a = TruncLeft(a, room-len(b))
		default:
			a, b = TruncLeft(a, half), TruncLeft(b, room-half)
		}
	}
	l.add(bold, a).add(gray, " -> ").add(bold, b)
	l.right(st, state, r.W)
	rule := r.nl().add(gray, strings.Repeat("=", r.W))
	return []*line{l, rule}
}

func (r *renderer) total() []*line {
	s := r.s
	frac := 0.0
	switch {
	case s.BytesTotal > 0:
		frac = float64(s.BytesDone+s.BytesFailed) / float64(s.BytesTotal)
	case s.Done && !s.Canceled:
		frac = 1
	}
	eta := ""
	switch {
	case s.Done:
	case s.Scanning:
		eta = "ETA after scan"
	case s.Avg > 0:
		rem := s.BytesTotal - s.BytesDone - s.BytesFailed
		eta = "ETA " + Clock(time.Duration(float64(rem)/s.Avg*float64(time.Second)))
	default:
		eta = "ETA --:--"
	}
	l := r.nl()
	l.add(bcyan, " TOTAL  ")
	tail := 2 + 6 + 2 + len(eta)
	fill := bgreen
	if s.FilesFailed > 0 {
		fill = byellow
	}
	bar(l, r.W-labelW-tail, frac, fill)
	l.add(bold, fmt.Sprintf("  %6s", Percent(frac)))
	if eta != "" {
		l.add(plain, "  ").add(plain, eta)
	}

	st := r.nl().pad(labelW)
	st.add(bold, Bytes(s.BytesDone)).add(plain, " of ").add(plain, Bytes(s.BytesTotal))
	st.add(gray, "   ").add(bold, Count(s.FilesDone)).add(plain, " of ").add(plain, Count(s.FilesTotal)+" files")
	if s.FilesSkipped > 0 {
		st.add(gray, "   ").add(plain, Count(s.FilesSkipped)+" unchanged")
		if s.BytesSkipped > 0 {
			st.add(gray, " ("+Bytes(s.BytesSkipped)+")")
		}
	}
	if s.FilesFailed > 0 {
		st.add(gray, "   ").add(bred, Count(s.FilesFailed)+" failed")
	}
	if s.Links > 0 {
		st.add(gray, "   ").add(plain, Count(s.Links)+" links")
	}

	if s.Scanning {
		st.add(gray, "   ").add(yellow, "scanning...")
	}
	out := []*line{l, st}
	// Rescue news gets its own line so it is never clipped off the end.
	if s.BytesResumed > 0 || s.BadRanges > 0 {
		rl := r.nl().pad(labelW)
		if s.BytesResumed > 0 {
			rl.add(plain, Bytes(s.BytesResumed)+" resumed from an earlier run")
		}
		if s.BadRanges > 0 {
			if s.BytesResumed > 0 {
				rl.add(gray, "   ")
			}
			rl.add(bred, fmt.Sprintf("%s unreadable (%s in %s)", Bytes(s.BadBytes), plural(int(s.BadRanges), "spot"), plural(int(s.BadFiles), "file")))
			if s.Rescue {
				rl.add(red, ", zero-filled")
			} else {
				rl.add(red, ", those files kept for a rerun")
			}
		}
		out = append(out, rl)
	}
	return out
}

// spc picks seconds per chart column so the chart covers the whole run, up to
// its last 30 minutes.
func spc(cols, secs int) int {
	want := secs
	if want > 1800 {
		want = 1800
	}
	for _, c := range []int{1, 2, 3, 5, 10, 15, 20, 30, 60, 120, 300, 600, 900, 1800, 3600} {
		if cols*c >= want {
			return c
		}
	}
	return 3600
}

func (r *renderer) chartCols() int { return r.W - labelW - 1 }

func (r *renderer) speedHeader() *line {
	s := r.s
	l := r.nl()
	l.add(bcyan, " SPEED  ")
	switch {
	case !s.RateOK && s.Done && s.BadRanges > 0:
		l.add(byellow, "nothing new could be read")
	case !s.RateOK && s.Done:
		l.add(plain, "nothing to copy")
	case s.Done:
		l.add(bold, Rate(s.Avg)).add(plain, " avg")
		if s.Peak > 0 {
			l.add(gray, "   ").add(plain, Rate(s.Peak)+" peak")
		}
	case !s.RateOK:
		l.add(byellow, "starting...")
	default:
		l.add(bgreen, Rate(s.Rate)).add(plain, " now")
		if s.Idle > 5*time.Second && !s.Done {
			l.add(yellow, " (no read finished for "+Ago(s.Idle)+")")
		}
		l.add(gray, "   ").add(bold, Rate(s.Avg)).add(plain, " avg")
		if s.Peak > 0 {
			l.add(gray, "   ").add(plain, Rate(s.Peak)+" peak")
		}
	}
	cols := r.chartCols()
	secs := r.h.Seconds()
	c := spc(cols, secs)
	span := secs
	if span > cols*c {
		span = cols * c
	}
	per := fmt.Sprintf("%ds", c)
	if c >= 60 {
		per = fmt.Sprintf("%dm", c/60)
	}
	l.right(gray, fmt.Sprintf("log scale, last %s, %s per column", Span(time.Duration(span)*time.Second), per), r.W)
	return l
}

// glyph is a cell filled to fraction f: ' ' . : #
func glyph(f float64) byte {
	switch {
	case f <= 0:
		return ' '
	case f < 1.0/3:
		return '.'
	case f < 2.0/3:
		return ':'
	}
	return '#'
}

type cell struct {
	st style
	ch byte
}

func emit(l *line, cells []cell) {
	for i := 0; i < len(cells); {
		j := i
		var b []byte
		for j < len(cells) && cells[j].st == cells[i].st {
			b = append(b, cells[j].ch)
			j++
		}
		l.add(cells[i].st, string(b))
		i = j
	}
}

func (r *renderer) chart(rows, readerRows int) []*line {
	cols := r.chartCols()
	cs := r.h.columns(cols, spc(cols, r.h.Seconds()))
	lo, hi := math.Inf(1), 0.0
	maxR := r.s.Reading
	// The scale only ever widens: a range once shown stays, so the chart
	// doesn't jump about as fast and slow stretches scroll in and out.
	if r.h.yHi > 0 {
		lo, hi = r.h.yLo, r.h.yHi
	}
	if r.h.rMax > maxR {
		maxR = r.h.rMax
	}
	for _, c := range cs {
		if c.rate > 0 {
			lo = math.Min(lo, c.rate)
			hi = math.Max(hi, c.rate)
		}
		if c.readers > maxR {
			maxR = c.readers
		}
	}
	if hi > 0 {
		r.h.yLo, r.h.yHi = lo, hi
	}
	r.h.rMax = maxR
	var out []*line
	bottom := ""
	if hi == 0 {
		for i := 0; i < rows; i++ {
			l := r.nl().add(gray, fmt.Sprintf("%6s |", ""))
			if i == rows/2 {
				if r.s.Done {
					l.add(gray, "  no data was read")
				} else {
					l.add(gray, "  waiting for the first chunk to land")
				}
			}
			out = append(out, l)
		}
	} else {
		b := ladder(lo/1.2, hi*1.02, rows)
		bottom = Short(b[0])
		for row := len(b) - 2; row >= 0; row-- {
			rlo, rhi := math.Log(b[row]), math.Log(b[row+1])
			l := r.nl().add(gray, fmt.Sprintf("%6s |", Short(b[row+1])))
			cells := make([]cell, cols)
			for i, c := range cs {
				cells[i] = cell{st: plain, ch: ' '}
				if c.rate <= 0 {
					continue
				}
				f := (math.Log(c.rate) - rlo) / (rhi - rlo)
				if f > 1 {
					f = 1
				}
				st := bgreen
				switch ratio := c.rate / hi; {
				case ratio < 0.02:
					st = bred
				case ratio < 0.25:
					st = byellow
				}
				cells[i] = cell{st: st, ch: glyph(f)}
			}
			emit(l, cells)
			out = append(out, l)
		}
	}
	axis := r.nl().add(gray, fmt.Sprintf("%6s +", bottom)+strings.Repeat("-", cols))
	if readerRows > 0 {
		axis = r.nl().add(gray, fmt.Sprintf("%6s +", bottom)+strings.Repeat("-", cols-10)+" readers -")
	}
	out = append(out, axis)
	for row := readerRows - 1; row >= 0; row-- {
		label := ""
		switch row {
		case readerRows - 1:
			label = fmt.Sprintf("%d", maxR)
		case 0:
			label = "1"
		}
		l := r.nl().add(gray, fmt.Sprintf("%6s |", label))
		per := float64(maxR) / float64(readerRows)
		cells := make([]cell, cols)
		for i, c := range cs {
			cells[i] = cell{st: plain, ch: ' '}
			if c.readers > 0 {
				cells[i] = cell{st: cyan, ch: glyph(math.Min(1, (float64(c.readers)-float64(row)*per)/per))}
			}
		}
		emit(l, cells)
		out = append(out, l)
	}
	return out
}

// ladder returns round-numbered row boundaries for a log-scale axis from lo to
// hi in at most rows rows (and at least 4, extending downward): the finest of
// 1-1.5-2-3-5-7, 1-2-5, 1-3 and 1 per decade that fits.
func ladder(lo, hi float64, rows int) []float64 {
	minRows := 4
	if rows < minRows {
		minRows = rows
	}
	var b []float64
	for _, seq := range [][]float64{{1, 1.5, 2, 3, 5, 7}, {1, 2, 5}, {1, 3}, {1}} {
		var all []float64
		for e := math.Floor(math.Log10(lo)) - 3; e <= math.Ceil(math.Log10(hi))+1; e++ {
			for _, m := range seq {
				all = append(all, m*math.Pow(10, e))
			}
		}
		start, end := 0, len(all)-1
		for i, v := range all {
			if v <= lo*(1+1e-9) {
				start = i
			}
		}
		for i := len(all) - 1; i >= 0; i-- {
			if all[i] >= hi*(1-1e-9) {
				end = i
			}
		}
		for end-start < minRows && start > 0 {
			start--
		}
		b = all[start : end+1]
		if len(b)-1 <= rows {
			return b
		}
	}
	return b[len(b)-1-rows:] // even decades don't fit: keep the top
}

// readersText is what's in flight right now, by lane, as in
// "4 flying + 12 slow + 3 trying x 256K".
func readersText(s copier.Snapshot) string {
	var parts []string
	for _, p := range []struct {
		n    int
		what string
	}{{s.InFly, "flying"}, {s.InSlow, "slow"}, {s.InTry, "trying"}} {
		if p.n > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", p.n, p.what))
		}
	}
	if len(parts) == 0 {
		return "no reads in flight"
	}
	return strings.Join(parts, " + ") + " x " + Chunk(s.Chunk)
}

// queueText is Q and where it came from.
func queueText(s copier.Snapshot) string {
	switch s.QueueHow {
	case "measured":
		return fmt.Sprintf("queue %d, measured", s.Queue)
	case "default":
		return fmt.Sprintf("queue %d, not measured", s.Queue)
	}
	return fmt.Sprintf("queue %d", s.Queue)
}

func trialText(from, to int, noun string) string {
	dir := "more"
	if to < from {
		dir = "fewer"
	}
	return fmt.Sprintf("%s %s %d -> %d", dir, noun, from, to)
}

func decisionText(d tuner.Decision, noun string) (string, style) {
	what := trialText(d.From, d.To, noun)
	switch d.Note {
	case "kept":
		return fmt.Sprintf("%s: %s, kept", what, Gain(d.Gain)), green
	case "not kept":
		if d.Gain >= 0 {
			return fmt.Sprintf("%s: %s, not worth it", what, Gain(d.Gain)), plain
		}
		return fmt.Sprintf("%s: %s, slower", what, Gain(d.Gain)), plain
	case "too few flying reads":
		return "too few reads to test " + what, gray
	case "starved":
		return "not enough work to test " + what, gray
	}
	return what + ": " + d.Note, plain
}

// tuner is three lines: what's in flight and the queue; the flying reads'
// tuner (W); the slow lane's (R).
func (r *renderer) tuner() []*line {
	s := r.s
	head := r.nl().add(bcyan, " READS  ").add(bold, readersText(s))
	if s.Stuck > 0 {
		head.add(plain, fmt.Sprintf(", %d stuck", s.Stuck))
	}
	right := queueText(s)
	if s.Quarantined > 0 {
		right = fmt.Sprintf("%s quarantined, %s", plural(s.Quarantined, "region"), right)
	}
	if head.w+3+len(right) <= r.W {
		head.right(gray, right, r.W)
	}
	fly := r.tunerLine(" FLYING ", s.Tuner, "flying reads", "")
	slow := r.tunerLine(" SLOW   ", s.SlowTuner, "slow reads", "no slow lane: the queue is too short to keep one")
	return []*line{head, fly, slow}
}

// tunerLine shows one tuner: its number and state, the test in progress with
// each arm's rate, or else its last verdict.
func (r *renderer) tunerLine(label string, st tuner.Status, noun, off string) *line {
	l := r.nl().add(bcyan, label)
	mode := st.Mode
	if r.s.Done && mode != "fixed" && mode != "off" {
		mode = "done"
	}
	arm := func(n int, rate float64, final bool) {
		lab := fmt.Sprintf(" [%d: ", n)
		switch {
		case final:
			l.add(plain, lab+Rate(rate)+"]")
		case rate > 0:
			l.add(byellow, lab+"measuring, "+Rate(rate)+"]")
		default:
			l.add(byellow, lab+"measuring]")
		}
	}
	switch mode {
	case "off":
		l.add(gray, off)
		return l
	case "fixed":
		l.add(plain, fmt.Sprintf("%d, fixed", st.Readers))
		return l
	case "warming up":
		l.add(plain, fmt.Sprintf("%d, warming up", st.Readers))
		return l
	case "measuring":
		l.add(plain, fmt.Sprintf("%d, measuring", st.Base))
		arm(st.Base, st.Rate, false)
		return l
	case "testing":
		l.add(plain, "testing ").add(byellow, trialText(st.Base, st.Candidate, noun))
		arm(st.Base, st.BaseRate, true)
		arm(st.Candidate, st.Rate, false)
		return l
	case "waiting for work":
		l.add(plain, fmt.Sprintf("%d", st.Readers)).add(gray, ", waiting for work")
	case "done":
		l.add(plain, fmt.Sprintf("%d, after %s", st.Readers, plural(st.Changes, "change")))
	default:
		l.add(plain, fmt.Sprintf("%d, holding", st.Readers))
		if st.NextCheck > time.Second {
			l.add(gray, " (next check in "+Ago(st.NextCheck)+")")
		}
	}
	if n := len(st.History); n > 0 {
		d := st.History[n-1]
		txt, sty := decisionText(d, noun)
		l.add(gray, "   last: ").add(sty, txt).add(gray, ", "+Ago(r.s.Now.Sub(r.s.Start)-d.At)+" ago")
	}
	return l
}

func medium(m string) string {
	switch m {
	case "Solid State":
		return "SSD"
	case "Rotational":
		return "HDD"
	}
	return m
}

func (r *renderer) disks() []*line {
	var single []*line
	fits := true
	for _, d := range r.s.Devices {
		one := r.nl()
		r.diskName(one, d)
		one.add(plain, "   ")
		r.diskStats(one, d)
		fits = fits && one.w <= r.W
		single = append(single, one)
	}
	if fits {
		return single
	}
	var out []*line
	for _, d := range r.s.Devices {
		l, m := r.nl(), r.nl().pad(labelW)
		r.diskName(l, d)
		r.diskStats(m, d)
		out = append(out, l, m)
	}
	return out
}

func (r *renderer) diskName(l *line, d copier.DeviceView) {
	role := map[string]string{"source": " SOURCE ", "dest": " DEST   ", "source+dest": " DISK   "}[d.Role]
	l.add(bcyan, role).add(bold, d.Info.Name)
	var desc []string
	for _, p := range []string{d.Info.Model, d.Info.Interconnect, medium(d.Info.Medium)} {
		if p != "" {
			desc = append(desc, p)
		}
	}
	l.add(plain, "  "+strings.Join(desc, ", "))
}

func (r *renderer) diskStats(m *line, d copier.DeviceView) {
	if !d.OK {
		m.add(gray, "measuring...")
		return
	}
	if d.Idle {
		m.add(gray, "idle for "+Ago(d.Window))
		if d.Errors > 0 || d.Retries > 0 {
			m.add(bred, fmt.Sprintf("  errors %d  retries %d", d.Errors, d.Retries))
		}
		return
	}
	rt := d.Rates
	if d.Role != "dest" {
		m.add(plain, "read ").add(bold, Rate(rt.ReadBps)).add(plain, fmt.Sprintf(", %.0f ops/s", rt.ReadIOPS))
		if rt.ReadLatency > 0 {
			m.add(plain, "  "+latency(rt.ReadLatency)+" each")
		}
	}
	if d.Role != "source" {
		if d.Role != "dest" {
			m.add(gray, "   ")
		}
		m.add(plain, "write ").add(bold, Rate(rt.WriteBps)).add(plain, fmt.Sprintf(", %.0f ops/s", rt.WriteIOPS))
	}
	m.add(plain, fmt.Sprintf("  queue %.1f", rt.QueueDepth))
	es := gray
	if d.Errors > 0 || d.Retries > 0 {
		es = bred
	}
	m.add(es, fmt.Sprintf("  errors %d  retries %d", d.Errors, d.Retries))
}

func plural(n int, word string) string {
	if n == 1 {
		return "1 " + word
	}
	return fmt.Sprintf("%d %ss", n, word)
}

func latency(d time.Duration) string {
	switch {
	case d < time.Millisecond:
		return fmt.Sprintf("%.0f us", float64(d)/1e3)
	case d < 10*time.Millisecond:
		return fmt.Sprintf("%.1f ms", float64(d)/1e6)
	case d < time.Second:
		return fmt.Sprintf("%.0f ms", float64(d)/1e6)
	}
	return fmt.Sprintf("%.1f s", d.Seconds())
}

func (r *renderer) files(room int) []*line {
	s := r.s
	head := r.nl().add(bcyan, " FILES  ")
	switch {
	case len(s.Active) > 0:
		head.add(plain, fmt.Sprintf("%s on %s", plural(s.Reading, "read"), plural(len(s.Active), "file")))
	case s.Done:
		head.add(plain, "finished")
	case s.Scanning:
		head.add(yellow, "scanning...")
	default:
		head.add(gray, "idle")
	}
	out := []*line{head}
	if room < 2 {
		return out
	}
	n := len(s.Active)
	show := n
	if show > room-1 {
		show = room - 2
		if show < 1 {
			show = 1
		}
	}
	barW := 22
	if r.W < 90 {
		barW = 12
	}
	for _, a := range s.Active[:show] {
		l := r.nl().add(plain, "  ")
		frac := 0.0
		if a.Size > 0 {
			frac = float64(a.Done) / float64(a.Size)
		}
		bar(l, barW, frac, green)
		l.add(plain, fmt.Sprintf(" %6s  ", Percent(frac)))
		switch {
		case a.RateOK:
			l.add(bold, fmt.Sprintf("%11s", Rate(a.Rate)))
		case a.Age < 1500*time.Millisecond:
			l.add(gray, fmt.Sprintf("%11s", "starting"))
		default:
			l.add(yellow, fmt.Sprintf("%11s", "wait "+Ago(a.Age)))
		}
		sizes := fmt.Sprintf("  %9s of %-9s  ", Bytes(a.Done), Bytes(a.Size))
		l.add(gray, sizes)
		l.add(plain, TruncLeft(Clean(a.Show), r.W-l.w))
		out = append(out, l)
	}
	if show < n {
		out = append(out, r.nl().add(gray, fmt.Sprintf("  ... and %d more", n-show)))
	}
	return out
}

func (r *renderer) errors() []*line {
	s := r.s
	l := r.nl().add(bcyan, " ERRORS ")
	if s.Errors == 0 {
		l.add(gray, "none")
		return []*line{l}
	}
	l.add(bred, Count(s.Errors)).add(plain, " -> ").add(bold, TruncLeft(Clean(s.ErrorFile), r.W-l.w-4))
	m := r.nl().pad(labelW).add(gray, "last: ")
	m.add(red, TruncRight(Clean(s.LastError), r.W-m.w))
	return []*line{l, m}
}

func (r *renderer) footer() *line {
	l := r.nl()
	if r.s.Done {
		return l
	}
	return l.add(gray, TruncRight(" Ctrl-C stops cleanly (reads in flight finish, partial files are removed); twice quits now", r.W))
}

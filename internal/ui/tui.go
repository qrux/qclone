package ui

import (
	"bytes"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"os"

	"tclone/internal/copier"
)

// TUI redraws the whole screen in place, on the terminal's alternate screen,
// and leaves the last frame in the normal scrollback when it stops.
type TUI struct {
	mu      sync.Mutex
	out     *os.File
	frame   Frame
	hist    *History
	started bool
}

// NewTUI draws on out, which must be a terminal.
func NewTUI(out *os.File, frame Frame, hist *History) *TUI {
	return &TUI{out: out, frame: frame, hist: hist}
}

func (t *TUI) size() (int, int) {
	w, h := Size(t.out)
	if w <= 0 || h <= 0 {
		return 100, 40
	}
	return w, h
}

// Start switches to the alternate screen and hides the cursor.
func (t *TUI) Start() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.out.WriteString("\x1b[?1049h\x1b[?25l\x1b[H\x1b[2J")
	t.started = true
}

// Draw paints one frame. Every line is rewritten and cleared to its end, and
// the frame is sent as one write inside a synchronized update, so there is no
// flicker on terminals that support it and no harm on those that don't.
func (t *TUI) Draw(s copier.Snapshot) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.started {
		return
	}
	f := t.frame
	f.W, f.H = t.size()
	lines := Render(f, s, t.hist)
	var b bytes.Buffer
	b.WriteString("\x1b[?2026h\x1b[H")
	for i, l := range lines {
		b.WriteString(l)
		b.WriteString("\x1b[K")
		if i < len(lines)-1 {
			b.WriteString("\r\n")
		}
	}
	b.WriteString("\x1b[J\x1b[?2026l")
	t.out.Write(b.Bytes())
}

// Restore puts the terminal back as it was. Safe to call more than once.
func (t *TUI) Restore() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.started {
		t.out.WriteString("\x1b[?25h\x1b[?1049l")
		t.started = false
	}
}

// Stop restores the terminal and prints the final frame where it will stay.
func (t *TUI) Stop(final copier.Snapshot) {
	t.Restore()
	f := t.frame
	f.W, f.H = t.size()
	if f.H > 48 {
		f.H = 48
	}
	lines := Render(f, final, t.hist)
	for len(lines) > 0 && strings.TrimSpace(stripANSI(lines[len(lines)-1])) == "" {
		lines = lines[:len(lines)-1]
	}
	t.out.WriteString(strings.Join(lines, "\n") + "\n")
}

func stripANSI(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == 0x1b {
			for i < len(s) && !(s[i] >= 'a' && s[i] <= 'z' || s[i] >= 'A' && s[i] <= 'Z') {
				i++
			}
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// Plain prints a one-line status every so often, for when the output is a
// pipe or a log. It never prints a file or path name.
type Plain struct {
	w     io.Writer
	every time.Duration
	last  time.Time
}

// NewPlain prints to w at most once per every.
func NewPlain(w io.Writer, every time.Duration) *Plain { return &Plain{w: w, every: every} }

// Maybe prints a status line if it's time.
func (p *Plain) Maybe(s copier.Snapshot) {
	if s.Now.Sub(p.last) < p.every {
		return
	}
	p.last = s.Now
	fmt.Fprintln(p.w, StatusLine(s))
}

// StatusLine is one line of counts and rates, with no paths.
func StatusLine(s copier.Snapshot) string {
	frac := 0.0
	if s.BytesTotal > 0 {
		frac = float64(s.BytesDone+s.BytesFailed) / float64(s.BytesTotal)
	}
	speed := "starting"
	if s.RateOK {
		speed = Rate(s.Rate) + " now  " + Rate(s.Avg) + " avg"
	}
	parts := []string{
		"tclone " + Clock(s.Now.Sub(s.Start)),
		Percent(frac),
		Bytes(s.BytesDone) + " of " + Bytes(s.BytesTotal),
		Count(s.FilesDone) + " of " + Count(s.FilesTotal) + " files",
		speed,
		settingText(s.Tuner.Applied),
		Count(s.Errors) + " errors",
	}
	if s.Scanning {
		parts = append(parts, "scanning")
	} else if s.Avg > 0 && !s.Done {
		rem := s.BytesTotal - s.BytesDone - s.BytesFailed
		parts = append(parts, "ETA "+Clock(time.Duration(float64(rem)/s.Avg*float64(time.Second))))
	}
	return strings.Join(parts, "  ")
}

// Summary is what's printed when tclone ends: counts, rates and what the
// tuner learned. No paths, except the error file's name.
func Summary(s copier.Snapshot) []string {
	el := Clock(s.Now.Sub(s.Start))
	head := "tclone: done in " + el
	if s.Canceled {
		head = "tclone: canceled after " + el
	}
	files := Count(s.FilesDone) + " files"
	if s.FilesDone == 1 {
		files = "1 file"
	}
	l1 := fmt.Sprintf("%s. Copied %s in %s", head, Bytes(s.BytesDone), files)
	if s.Avg > 0 {
		l1 += fmt.Sprintf(" (avg %s, peak %s)", Rate(s.Avg), Rate(s.Peak))
	}
	if s.FilesSkipped > 0 {
		l1 += fmt.Sprintf("; %s unchanged", Count(s.FilesSkipped))
	}
	if s.FilesFailed > 0 {
		l1 += fmt.Sprintf("; %s failed", Count(s.FilesFailed))
	}
	l1 += "."
	out := []string{l1}
	st := s.Tuner
	l2 := fmt.Sprintf("tclone: ended at %s", settingText(st.Applied))
	if st.Mode == "fixed" {
		l2 += " (fixed)"
	} else {
		l2 += " after " + plural(st.Changes, "change")
	}
	if len(st.Regimes) > 1 {
		var parts []string
		for _, g := range st.Regimes {
			parts = append(parts, fmt.Sprintf("%s regions -> %s", Rate(g.Rate), settingText(g.Best)))
		}
		l2 += "; learned " + strings.Join(parts, ", ")
	}
	out = append(out, l2+".")
	switch {
	case s.Errors > 0:
		out = append(out, fmt.Sprintf("tclone: %s errors, written to %s", Count(s.Errors), filepath.Base(s.ErrorFile)))
	default:
		out = append(out, "tclone: no errors.")
	}
	return out
}

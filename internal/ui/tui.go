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

	"qclone/internal/copier"
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
		"qclone " + Clock(s.Now.Sub(s.Start)),
		Percent(frac),
		Bytes(s.BytesDone) + " of " + Bytes(s.BytesTotal),
		Count(s.FilesDone) + " of " + Count(s.FilesTotal) + " files",
		speed,
		readersText(s),
		Count(s.Errors) + " errors",
	}
	if s.BytesResumed > 0 {
		parts = append(parts, Bytes(s.BytesResumed)+" resumed")
	}
	if s.BadRanges > 0 {
		parts = append(parts, Bytes(s.BadBytes)+" unreadable in "+plural(int(s.BadRanges), "spot"))
	}
	if s.Scanning {
		parts = append(parts, "scanning")
	} else if s.Avg > 0 && !s.Done {
		rem := s.BytesTotal - s.BytesDone - s.BytesFailed
		parts = append(parts, "ETA "+Clock(time.Duration(float64(rem)/s.Avg*float64(time.Second))))
	}
	return strings.Join(parts, "  ")
}

// Summary is what's printed when qclone ends: counts, rates and where the
// tuner ended. No paths, except the error file's name.
func Summary(s copier.Snapshot) []string {
	el := Clock(s.Now.Sub(s.Start))
	head := "qclone: done in " + el
	if s.Canceled {
		head = "qclone: canceled after " + el
	}
	files := Count(s.FilesDone) + " files"
	if s.FilesDone == 1 {
		files = "1 file"
	}
	// Bytes carried over from an earlier run, and zeros standing in for
	// unreadable ones, weren't copied by this run.
	copied := s.BytesDone - s.BytesResumed - s.BytesZeroed
	l1 := fmt.Sprintf("%s. Copied %s in %s", head, Bytes(copied), files)
	if copied == 0 && s.FilesDone > 0 {
		l1 = fmt.Sprintf("%s. Finished %s; no new bytes could be read", head, files)
	}
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
	l2 := fmt.Sprintf("qclone: ended at %s x %s", plural(st.Readers, "flying read"), Chunk(s.Chunk))
	if st.Mode == "fixed" {
		l2 += " (fixed)"
	} else {
		l2 += " after " + plural(st.Changes, "change")
	}
	switch {
	case s.PeakStuck > s.MaxStuck:
		l2 += fmt.Sprintf("; up to %d reads stuck at once (%d in their own room, the rest in the flying reads')", s.PeakStuck, s.MaxStuck)
	case s.PeakStuck > 0:
		l2 += fmt.Sprintf("; up to %d reads stuck at once", s.PeakStuck)
	}
	out = append(out, l2+".")
	if s.BytesResumed > 0 {
		out = append(out, fmt.Sprintf("qclone: %s carried over from an earlier run (partial or zero-filled copies), not read again.", Bytes(s.BytesResumed)))
	}
	if s.BadRanges > 0 {
		l := fmt.Sprintf("qclone: %s unreadable in %s, in %s", Bytes(s.BadBytes), plural(int(s.BadRanges), "spot"), plural(int(s.BadFiles), "file"))
		if s.FilesRescued > 0 {
			l += fmt.Sprintf("; %s installed with zeros there (a later run tries those spots again)", plural(int(s.FilesRescued), "file"))
		}
		if n := s.BadFiles - s.FilesRescued; n > 0 {
			l += fmt.Sprintf("; %s not installed: rerun to retry just those spots, or add --rescue", plural(int(n), "file"))
		}
		out = append(out, l+".")
	}
	switch {
	case s.Errors > 0:
		out = append(out, fmt.Sprintf("qclone: %s errors, written to %s", Count(s.Errors), filepath.Base(s.ErrorFile)))
	default:
		out = append(out, "qclone: no errors.")
	}
	return out
}

package ui

import (
	"context"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"qclone/internal/copier"
	"qclone/internal/devstat"
	"qclone/internal/plan"
	"qclone/internal/sim"
	"qclone/internal/tuner"
)

func TestFormatNeverPrintsZeroForPositive(t *testing.T) {
	for _, v := range []float64{1e-9, 0.4, 1, 999.4, 999.96, 1e3, 2e6, 499.99e6, 1e12} {
		s := Rate(v)
		if strings.HasPrefix(s, "0 ") || strings.HasPrefix(s, "0.00") {
			t.Errorf("Rate(%v) = %q", v, s)
		}
	}
	cases := map[int64]string{0: "0 B", 999: "999 B", 1000: "1.00 kB", 1_234_567: "1.23 MB", 22_700_000_000: "22.7 GB", 999_960: "1.00 MB"}
	for n, want := range cases {
		if got := Bytes(n); got != want {
			t.Errorf("Bytes(%d) = %q, want %q", n, got, want)
		}
	}
	if got := Count(1234567); got != "1,234,567" {
		t.Errorf("Count = %q", got)
	}
	if got := Clock(3*time.Hour + 4*time.Minute + 5*time.Second); got != "3:04:05" {
		t.Errorf("Clock = %q", got)
	}
	if got := Percent(0.99999); got != "99.9%" {
		t.Errorf("Percent(0.99999) = %q; must not round up to 100%% before the end", got)
	}
	if got := Clean("a\x1b[2Jb\nc"); got != "a?[2Jb?c" {
		t.Errorf("Clean = %q", got)
	}
}

func fakeSnapshot(now time.Time) copier.Snapshot {
	start := now.Add(-95 * time.Second)
	return copier.Snapshot{
		Now: now, Start: start,
		FilesTotal: 5678, FilesDone: 1234, FilesSkipped: 312, FilesFailed: 2,
		BytesTotal: 22_700_000_000, BytesDone: 10_200_000_000, BytesSkipped: 4e9, BytesFailed: 3e6,
		Rate: 7.9e6, RateOK: true, Avg: 13.8e6, Peak: 498e6, Idle: 7 * time.Second,
		Active: []copier.Active{
			{Show: "video/2009/an-old-tape-rip-with-a-long-name.mov", Size: 3.4e9, Done: 1.2e9, Rate: 7.1e6, RateOK: true, Age: 80 * time.Second},
			{Show: "photos/2011/IMG_4400.CR2", Size: 25e6, Done: 1e6, Age: 4 * time.Second},
			{Show: "photos/2011/IMG_4401.CR2", Size: 25e6, Age: 500 * time.Millisecond},
			{Show: "photos/2011/IMG_4402.CR2", Size: 25e6, Age: 200 * time.Millisecond},
			{Show: "photos/2011/IMG_4403.CR2", Size: 25e6, Age: 100 * time.Millisecond},
		},
		Tuner: tuner.Status{
			Mode: "testing", Readers: 16, Base: 8, Candidate: 16, BaseRate: 412e6, Rate: 398e6,
			History: []tuner.Decision{{At: 40 * time.Second, From: 4, To: 8, Gain: 0.38, Kept: true, Note: "kept"}},
			Changes: 2,
		},
		Readers: 32, Reading: 31, Stuck: 16, PeakStuck: 20, MaxStuck: 16, Chunk: 256 << 10,
		Devices: []copier.DeviceView{
			{Role: "source", OK: true, Info: devInfo("disk4", "Samsung SSD 840 EVO 1TB", "USB", "Solid State"), Rates: rates(7.9e6, 0, 31, 412*time.Millisecond, 12.9)},
			{Role: "dest", OK: true, Info: devInfo("disk0", "APPLE SSD AP8192Z", "Apple Fabric", "Solid State"), Rates: rates(0, 7.9e6, 31, 0, 0.3)},
		},
		Errors: 2, ErrorFile: "20261012-141530-qclone-errors.txt", LastError: "read at byte 4194304 /Volumes/Old/photos/x.jpg: input/output error (3 tries)",
	}
}

func checkFrame(t *testing.T, lines []string, w, h int, what string) {
	t.Helper()
	if len(lines) > h {
		t.Errorf("%s: %d lines for a %d-row terminal", what, len(lines), h)
	}
	for i, l := range lines {
		vis := stripANSI(l)
		if n := utf8.RuneCountInString(vis); n > w {
			t.Errorf("%s: line %d is %d wide on a %d-column terminal: %q", what, i, n, w, vis)
		}
		for _, r := range vis {
			if r > 0x7e || r < 0x20 {
				t.Errorf("%s: line %d has non-ASCII %q: %q", what, i, r, vis)
				break
			}
		}
	}
}

func TestFramesFitEveryTerminal(t *testing.T) {
	now := time.Now()
	s := fakeSnapshot(now)
	h := NewHistory(s.Start)
	for i := 0; i <= 95; i++ {
		r := 498e6
		if (i/20)%2 == 1 {
			r = 2.1e6 + float64(i)*5e4
		}
		h.Record(s.Start.Add(time.Duration(i)*time.Second), r, true, 2+(i/20%2)*11)
	}
	sizes := [][2]int{{80, 24}, {100, 40}, {132, 50}, {200, 60}, {300, 100}}
	for w := 20; w < 80; w++ { // every narrow width: no panics, nothing too wide
		sizes = append(sizes, [2]int{w, 10 + w/3})
	}
	for _, size := range sizes {
		for _, color := range []bool{false, true} {
			for _, done := range []bool{false, true} {
				s.Done, s.Canceled = done, done
				f := Frame{W: size[0], H: size[1], Color: color, Sources: []string{"/Volumes/Old/photos", "/Volumes/Old/video/"}, Dst: "/Users/q/rescue"}
				checkFrame(t, Render(f, s, h), min(size[0], 160), size[1], fmt.Sprintf("%dx%d color=%v done=%v", size[0], size[1], color, done))
			}
		}
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// Runs a real copy against the simulated degraded drive and checks every
// frame. With QCLONE_FRAMES=FILE set, also writes the frames there as text.
func TestFramesDuringSimulatedCopy(t *testing.T) {
	base := t.TempDir()
	src := filepath.Join(base, "src")
	for _, f := range []struct {
		p string
		n int
	}{{"old-photos/a.jpg", 3 << 20}, {"old-photos/b.jpg", 3 << 20}, {"video/clip.mov", 80 << 20}, {"recent/n.txt", 100000}} {
		os.MkdirAll(filepath.Dir(filepath.Join(src, f.p)), 0o755)
		os.WriteFile(filepath.Join(src, f.p), make([]byte, f.n), 0o644)
	}
	p, err := plan.Resolve([]string{src + "/", filepath.Join(base, "dst")})
	if err != nil {
		t.Fatal(err)
	}
	errs := copier.NewErrorLog(filepath.Join(base, "e.txt"), nil, time.Now())
	eng := copier.New(p, copier.Options{Readers: 4, Tuner: tuner.DefaultConfig(), MaxStuck: 16, Chunk: 1 << 20, ModifyWindow: time.Second, Retries: 3, Errors: errs, Sim: sim.NewDevice(sim.Degraded())})
	hist := NewHistory(eng.Snapshot().Start)
	done := make(chan struct{})
	go func() { eng.Run(context.Background()); close(done) }()
	var dump *os.File
	if out := os.Getenv("QCLONE_FRAMES"); out != "" {
		dump, _ = os.Create(out)
		defer dump.Close()
	}
	f := Frame{W: 110, H: 44, Sources: []string{src + "/"}, Dst: filepath.Join(base, "dst")}
	tick := time.NewTicker(250 * time.Millisecond)
	defer tick.Stop()
	frames := 0
	for running := true; running; {
		select {
		case <-done:
			running = false
		case <-tick.C:
		}
		s := eng.Snapshot()
		hist.Record(s.Now, s.Rate, s.RateOK, s.Readers)
		lines := Render(f, s, hist)
		checkFrame(t, lines, f.W, f.H, fmt.Sprintf("frame %d", frames))
		if s.BytesDone > 0 && !s.Done {
			joined := strings.Join(lines, "\n")
			if strings.Contains(joined, " 0 B/s") || !s.RateOK || s.Rate <= 0 || math.IsNaN(s.Rate) {
				t.Errorf("frame %d shows no positive rate", frames)
			}
		}
		if dump != nil && (frames%8 == 0 || !running) {
			fmt.Fprintf(dump, "----- frame %d (%.1fs)\n%s\n", frames, s.Now.Sub(s.Start).Seconds(), strings.Join(lines, "\n"))
		}
		frames++
	}
}

func devInfo(name, model, link, medium string) devstat.Info {
	return devstat.Info{Name: name, Model: model, Interconnect: link, Medium: medium}
}

func rates(r, w, iops float64, lat time.Duration, q float64) devstat.Rates {
	return devstat.Rates{ReadBps: r, WriteBps: w, ReadIOPS: iops, WriteIOPS: iops, ReadLatency: lat, QueueDepth: q}
}

func TestLadder(t *testing.T) {
	cases := []struct {
		lo, hi float64
		rows   int
		want   string
	}{
		{1.4e6, 525e6, 12, "1M 2M 5M 10M 20M 50M 100M 200M 500M 1G"},
		{1.4e6, 525e6, 6, "1M 3M 10M 30M 100M 300M 1G"},
		{1.4e6, 525e6, 4, "100k 1M 10M 100M 1G"},
		{400e6, 520e6, 12, "150M 200M 300M 500M 700M"},
		{2e6, 8e6, 12, "2M 3M 5M 7M 10M"},
	}
	for _, c := range cases {
		var got []string
		for _, v := range ladder(c.lo, c.hi, c.rows) {
			got = append(got, Short(v))
		}
		if g := strings.Join(got, " "); g != c.want {
			t.Errorf("ladder(%g, %g, %d) = %s, want %s", c.lo, c.hi, c.rows, g, c.want)
		}
	}
}

func TestRescueCountsShown(t *testing.T) {
	s := fakeSnapshot(time.Now())
	s.BytesResumed, s.BadRanges, s.BadBytes, s.BadFiles, s.FilesRescued, s.Rescue = 1_200_000_000, 3, 20480, 2, 1, true
	h := NewHistory(s.Start)
	f := Frame{W: 120, H: 50, Sources: []string{"/Volumes/Old/photos"}, Dst: "/Users/q/rescue"}
	frame := stripANSI(strings.Join(Render(f, s, h), "\n"))
	for _, want := range []string{"1.20 GB resumed from an earlier run", "20.5 kB unreadable (3 spots in 2 files), zero-filled", " rescue"} {
		if !strings.Contains(frame, want) {
			t.Errorf("frame lacks %q", want)
		}
	}
	sum := strings.Join(Summary(s), "\n")
	for _, want := range []string{"1.20 GB carried over from an earlier run", "20.5 kB unreadable in 3 spots, in 2 files", "1 file installed with zeros", "1 file not installed"} {
		if !strings.Contains(sum, want) {
			t.Errorf("summary lacks %q:\n%s", want, sum)
		}
	}
	line := StatusLine(s)
	if !strings.Contains(line, "3 spots") || strings.Contains(line, "/") && strings.Contains(line, "Volumes") {
		t.Errorf("status line %q", line)
	}
	for _, l := range append(Summary(s), line) {
		if strings.Contains(l, "/Volumes") || strings.Contains(l, "/Users") {
			t.Errorf("a path reached stdout text: %q", l)
		}
	}
}

// Once the chart has shown a range, later frames never narrow it, even when
// the extremes have been averaged into wider columns.
func TestChartScaleOnlyWidens(t *testing.T) {
	now := time.Now()
	s := fakeSnapshot(now)
	start := now.Add(-3000 * time.Second)
	s.Start = start
	h := NewHistory(start)
	axis := func() string {
		var labels []string
		for _, l := range Render(Frame{W: 110, H: 44}, s, h) {
			v := stripANSI(l)
			if len(v) > 8 && (v[7] == '|' || v[7] == '+') {
				labels = append(labels, strings.TrimSpace(v[:7]))
			}
		}
		return strings.Join(labels, " ")
	}
	for i := 0; i < 100; i++ { // wild: 2 MB/s and 500 MB/s, second by second
		r := 2e6
		if i%2 == 0 {
			r = 500e6
		}
		h.Record(start.Add(time.Duration(i)*time.Second), r, true, 4)
	}
	s.Now = start.Add(100 * time.Second)
	first := axis()
	for i := 100; i < 3000; i++ { // then a long, steady 50 MB/s
		h.Record(start.Add(time.Duration(i)*time.Second), 50e6, true, 4)
	}
	s.Now = start.Add(3000 * time.Second)
	if later := axis(); later != first {
		t.Fatalf("scale changed:\n first %s\n later %s", first, later)
	}
	fresh := NewHistory(start)
	for i := 100; i < 3000; i++ {
		fresh.Record(start.Add(time.Duration(i)*time.Second), 50e6, true, 4)
	}
	h = fresh
	if narrow := axis(); narrow == first {
		t.Fatal("test is not testing anything: a steady run alone shows the same scale")
	}
}

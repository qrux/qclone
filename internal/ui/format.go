package ui

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

// Sizes and rates are decimal (kB, MB, GB), as Finder and drive makers use.

// Bytes formats a byte count: 999 B, 1.2 kB, 34.5 MB, 1.23 GB.
func Bytes(n int64) string {
	if n < 1000 {
		return fmt.Sprintf("%d B", n)
	}
	return scaled(float64(n), "B")
}

// Rate formats bytes/s. A positive rate never prints as zero.
func Rate(bps float64) string {
	switch {
	case bps <= 0:
		return "0 B/s"
	case bps < 1:
		return "<1 B/s"
	case bps < 1000:
		return fmt.Sprintf("%.0f B/s", bps)
	}
	return scaled(bps, "B/s")
}

func scaled(v float64, unit string) string {
	prefixes := []string{"k", "M", "G", "T", "P"}
	i := -1
	for v >= 999.95 && i < len(prefixes)-1 {
		v /= 1000
		i++
	}
	p := prefixes[i]
	switch {
	case v < 9.995:
		return fmt.Sprintf("%.2f %s%s", v, p, unit)
	case v < 99.95:
		return fmt.Sprintf("%.1f %s%s", v, p, unit)
	}
	return fmt.Sprintf("%.0f %s%s", v, p, unit)
}

// Short formats bytes/s for the chart axis: 500M, 1.5M, 2G, 64k.
func Short(v float64) string {
	prefixes := []string{"", "k", "M", "G", "T"}
	i := 0
	for v >= 999.5 && i < len(prefixes)-1 {
		v /= 1000
		i++
	}
	t := fmt.Sprintf("%.1f", v)
	if v >= 9.95 || strings.HasSuffix(t, ".0") {
		t = fmt.Sprintf("%.0f", v)
	}
	return t + prefixes[i]
}

// Chunk formats a chunk size in binary units, since chunks are powers of two: 256K, 4M.
func Chunk(n int) string {
	switch {
	case n >= 1<<20 && n%(1<<20) == 0:
		return fmt.Sprintf("%dM", n>>20)
	case n >= 1<<10 && n%(1<<10) == 0:
		return fmt.Sprintf("%dK", n>>10)
	}
	return strconv.Itoa(n)
}

// Count formats an integer with thousands separators.
func Count(n int64) string {
	s := strconv.FormatInt(n, 10)
	neg := strings.HasPrefix(s, "-")
	if neg {
		s = s[1:]
	}
	var b strings.Builder
	for i, c := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(c)
	}
	if neg {
		return "-" + b.String()
	}
	return b.String()
}

// Clock formats a duration as H:MM:SS.
func Clock(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	s := int64(d.Round(time.Second) / time.Second)
	return fmt.Sprintf("%d:%02d:%02d", s/3600, s/60%60, s%60)
}

// Ago formats a short past duration: 4s, 3m, 2h.
func Ago(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
	return fmt.Sprintf("%dh", int(d.Hours()))
}

// Span formats a chart's time span: 45s, 4m30s, 1h05m.
func Span(d time.Duration) string {
	s := int(d.Round(time.Second).Seconds())
	switch {
	case s < 60:
		return fmt.Sprintf("%ds", s)
	case s < 3600:
		if s%60 == 0 {
			return fmt.Sprintf("%dm", s/60)
		}
		return fmt.Sprintf("%dm%02ds", s/60, s%60)
	}
	return fmt.Sprintf("%dh%02dm", s/3600, s/60%60)
}

// Percent formats a fraction: 44.9%, 100%.
func Percent(f float64) string {
	if math.IsNaN(f) || f < 0 {
		f = 0
	}
	if f >= 1 {
		return "100%"
	}
	return fmt.Sprintf("%.1f%%", math.Floor(f*1000)/10)
}

// Gain formats a ratio minus one: +41%, -3%.
func Gain(g float64) string {
	return fmt.Sprintf("%+.0f%%", g*100)
}

// TruncLeft keeps the end of s within n characters: "...hotos/IMG_4400.CR2".
func TruncLeft(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	if n <= 3 {
		return string(r[len(r)-n:])
	}
	return "..." + string(r[len(r)-(n-3):])
}

// TruncRight keeps the start of s within n characters.
func TruncRight(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	if n <= 3 {
		return string(r[:n])
	}
	return string(r[:n-3]) + "..."
}

// Clean replaces control characters with '?', so a file name can't carry
// escape sequences or line breaks into the display. Other characters stay.
func Clean(s string) string {
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0) {
			return '?'
		}
		return r
	}, s)
}

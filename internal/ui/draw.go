package ui

import (
	"strings"
	"unicode/utf8"
)

type style string

// SGR codes.
const (
	plain   style = ""
	bold    style = "1"
	dim     style = "2"
	red     style = "31"
	green   style = "32"
	yellow  style = "33"
	blue    style = "34"
	magenta style = "35"
	cyan    style = "36"
	gray    style = "90"
	bgreen  style = "1;32"
	bcyan   style = "1;36"
	byellow style = "1;33"
	bred    style = "1;31"
	inverse style = "1;7"
)

// line builds one row of the display, tracking its visible width.
type line struct {
	b     strings.Builder
	w     int
	color bool
}

func (l *line) add(st style, s string) *line {
	if s == "" {
		return l
	}
	if l.color && st != plain {
		l.b.WriteString("\x1b[" + string(st) + "m")
		l.b.WriteString(s)
		l.b.WriteString("\x1b[0m")
	} else {
		l.b.WriteString(s)
	}
	l.w += utf8.RuneCountInString(s)
	return l
}

func (l *line) pad(to int) *line {
	if l.w < to {
		l.add(plain, strings.Repeat(" ", to-l.w))
	}
	return l
}

// right adds s so that it ends at column to (if there is room).
func (l *line) right(st style, s string, to int) *line {
	n := utf8.RuneCountInString(s)
	if l.w+1+n > to {
		return l
	}
	l.pad(to - n)
	return l.add(st, s)
}

func (l *line) String() string { return l.b.String() }

// bar draws an ASCII progress bar: [##########>.........]
func bar(l *line, width int, frac float64, fill style) {
	if width < 3 {
		return
	}
	inner := width - 2
	if frac < 0 {
		frac = 0
	}
	if frac > 1 {
		frac = 1
	}
	n := int(frac * float64(inner))
	l.add(gray, "[")
	if frac >= 1 {
		l.add(fill, strings.Repeat("#", inner))
	} else {
		l.add(fill, strings.Repeat("#", n))
		l.add(fill, ">")
		if rest := inner - n - 1; rest > 0 {
			l.add(gray, strings.Repeat(".", rest))
		}
	}
	l.add(gray, "]")
}

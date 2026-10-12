package copier

import (
	"fmt"
	"os"
	"strings"
	"sync"
	"time"
)

// ErrorLog collects errors in a file instead of on stderr. The file is only
// created when the first error arrives, so a clean run leaves nothing behind.
type ErrorLog struct {
	mu      sync.Mutex
	path    string
	header  string
	f       *os.File
	count   int64
	last    string
	openErr error
}

// DefaultErrorPath is <timestamp>-tclone-errors.txt in the current directory.
func DefaultErrorPath(now time.Time) string {
	return now.Format("20060102-150405") + "-tclone-errors.txt"
}

// NewErrorLog prepares (but does not create) the error file.
func NewErrorLog(path string, args []string, started time.Time) *ErrorLog {
	return &ErrorLog{
		path:   path,
		header: fmt.Sprintf("# tclone errors\n# started %s\n# command: tclone %s\n", started.Format(time.RFC3339), strings.Join(args, " ")),
	}
}

// Log records one error: what tclone was doing, on which path, and why.
func (l *ErrorLog) Log(op, path string, err error) {
	now := time.Now()
	line := fmt.Sprintf("%s  %-6s %s: %v\n", now.Format(time.RFC3339), op, path, err)
	l.mu.Lock()
	defer l.mu.Unlock()
	l.count++
	l.last = fmt.Sprintf("%s %s: %v", op, path, err)
	if l.f == nil && l.openErr == nil {
		l.f, l.openErr = os.OpenFile(l.path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o644)
		if l.openErr == nil {
			l.f.WriteString(l.header)
		}
	}
	if l.f != nil {
		l.f.WriteString(line)
	}
}

// Count is how many errors were logged.
func (l *ErrorLog) Count() int64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.count
}

// Last is the most recent error, in one line.
func (l *ErrorLog) Last() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.last
}

// Path is where errors go.
func (l *ErrorLog) Path() string { return l.path }

// WriteErr is why the error file couldn't be written, if it couldn't.
func (l *ErrorLog) WriteErr() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.openErr
}

// Close finishes the file with the stop time.
func (l *ErrorLog) Close() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f != nil {
		fmt.Fprintf(l.f, "# stopped %s, %d errors\n", time.Now().Format(time.RFC3339), l.count)
		l.f.Close()
		l.f = nil
	}
}

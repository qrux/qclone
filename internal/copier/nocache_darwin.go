//go:build darwin

package copier

import (
	"os"
	"syscall"
)

// noCache turns off the unified buffer cache for f (F_NOCACHE), so reads come
// from the disk when asked for and writes reach it when made. Two reasons:
// the rate shown is the disk's, not the cache's (no fast-then-stall from
// write-back), and the reads in flight are exactly the tuner's setting, with
// no kernel read-ahead adding its own.
func noCache(f *os.File) {
	fd := f.Fd()
	syscall.Syscall(syscall.SYS_FCNTL, fd, syscall.F_NOCACHE, 1)
	syscall.Syscall(syscall.SYS_FCNTL, fd, syscall.F_RDAHEAD, 0)
}

//go:build darwin || linux

package ui

import (
	"os"
	"syscall"
	"unsafe"
)

// IsTerminal reports whether f is a terminal.
func IsTerminal(f *os.File) bool {
	var t syscall.Termios
	_, _, err := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), uintptr(ioctlGetTermios), uintptr(unsafe.Pointer(&t)))
	return err == 0
}

// Size returns the terminal's columns and rows, or 0, 0.
func Size(f *os.File) (int, int) {
	var ws struct{ Row, Col, X, Y uint16 }
	_, _, err := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), uintptr(syscall.TIOCGWINSZ), uintptr(unsafe.Pointer(&ws)))
	if err != 0 {
		return 0, 0
	}
	return int(ws.Col), int(ws.Row)
}

//go:build !darwin

package copier

import "os"

func noCache(f *os.File) {}

//go:build !darwin

package devstat

import "errors"

// ForPath is only implemented on macOS; elsewhere qclone runs without disk counters.
func ForPath(path string) (Source, error) {
	return nil, errors.New("disk counters are only read on macOS")
}

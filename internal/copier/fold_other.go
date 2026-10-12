//go:build !darwin

package copier

func caseSensitive(path string) bool { return true }

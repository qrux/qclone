//go:build !darwin

package copier

func cloneFile(src, dst string) error { return copyPlain(src, dst) }

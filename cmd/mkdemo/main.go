// mkdemo writes the demo tree that `make demo` copies with
// `qclone --simulate degraded`: old photos (whose names put them in the
// simulated drive's slow, heavily error-corrected regions), two big videos
// with slow stretches inside them, and many small recent files.
//
// usage: mkdemo DIR
//
// Files that already exist with the right size are left alone.
package main

import (
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
)

type set struct {
	dir   string
	name  string
	count int
	size  int64
}

var demo = []set{
	{"old-photos-2014", "IMG_%04d.JPG", 60, 1_500_000},
	{"video", "clip-%d.mov", 2, 384 << 20},
	{"recent", "note-%03d.txt", 300, 256_000},
}

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: mkdemo DIR")
		os.Exit(2)
	}
	root := os.Args[1]
	var made, kept int
	var bytes int64
	for _, s := range demo {
		dir := filepath.Join(root, s.dir)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			fmt.Fprintln(os.Stderr, "mkdemo:", err)
			os.Exit(1)
		}
		for i := 1; i <= s.count; i++ {
			p := filepath.Join(dir, fmt.Sprintf(s.name, i))
			bytes += s.size
			if fi, err := os.Stat(p); err == nil && fi.Size() == s.size {
				kept++
				continue
			}
			if err := write(p, s.size, uint64(i)); err != nil {
				fmt.Fprintln(os.Stderr, "mkdemo:", err)
				os.Exit(1)
			}
			made++
		}
	}
	fmt.Printf("mkdemo: %d files written, %d already there, %.0f MB in all\n", made, kept, float64(bytes)/1e6)
}

func write(path string, size int64, seed uint64) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	rng := rand.New(rand.NewChaCha8([32]byte{byte(seed), byte(seed >> 8), byte(size)}))
	buf := make([]byte, 1<<20)
	for left := size; left > 0; {
		n := int64(len(buf))
		if n > left {
			n = left
		}
		for i := 0; i < int(n); i += 8 {
			v := rng.Uint64()
			for k := 0; k < 8 && i+k < int(n); k++ {
				buf[i+k] = byte(v >> (8 * k))
			}
		}
		if _, err := f.Write(buf[:n]); err != nil {
			f.Close()
			return err
		}
		left -= n
	}
	return f.Close()
}

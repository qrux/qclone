// Package plan turns tclone's arguments into copy roots and walks them.
//
// Sources follow the rsync/cp convention:
//
//	tclone /path/to/source  /dst   ->  /dst/source/...   (the directory itself)
//	tclone /path/to/source/ /dst   ->  /dst/...          (its contents)
//
// A trailing "/." or a source of "." also means contents. A file source lands
// in the destination under its own name. A symlink source without a trailing
// slash is copied as a link; with one, it is followed.
package plan

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// Kind is what a root is.
type Kind int

const (
	Dir Kind = iota
	File
	Symlink
)

// Root is one source argument, resolved.
type Root struct {
	Arg      string // as typed
	Src      string // cleaned
	Dst      string // the path in the destination that stands for Src
	Contents bool   // trailing slash: Dst is the destination itself
	Kind     Kind
	Label    string // prefix for display paths: the source's name, or "" for contents
}

// Plan is the whole job.
type Plan struct {
	Roots   []Root
	Dst     string
	Overlap bool // two roots write into the same part of the destination
}

// Resolve checks the arguments: SRC... DST.
func Resolve(args []string) (*Plan, error) {
	if len(args) < 2 {
		return nil, errors.New("need at least one source and a destination")
	}
	dst := filepath.Clean(args[len(args)-1])
	if fi, err := os.Stat(dst); err == nil && !fi.IsDir() {
		return nil, fmt.Errorf("destination %s exists and is not a directory", dst)
	} else if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	p := &Plan{Dst: dst}
	for _, arg := range args[:len(args)-1] {
		r, err := resolveOne(arg, dst)
		if err != nil {
			return nil, err
		}
		p.Roots = append(p.Roots, r)
	}
	if err := checkInside(p); err != nil {
		return nil, err
	}
	for i := range p.Roots {
		for j := i + 1; j < len(p.Roots); j++ {
			a, b := p.Roots[i], p.Roots[j]
			if within(a.Dst, b.Dst) || within(b.Dst, a.Dst) {
				if a.Kind != Dir && b.Kind != Dir && a.Dst == b.Dst {
					return nil, fmt.Errorf("%s and %s would both be copied to %s", a.Arg, b.Arg, a.Dst)
				}
				p.Overlap = true
			}
		}
	}
	return p, nil
}

func contentsArg(arg string) bool {
	if strings.HasSuffix(arg, "/") || strings.HasSuffix(arg, "/.") {
		return true
	}
	b := filepath.Base(arg)
	return b == "." || b == ".."
}

func resolveOne(arg, dst string) (Root, error) {
	if arg == "" {
		return Root{}, errors.New("empty source path")
	}
	src := filepath.Clean(arg)
	r := Root{Arg: arg, Src: src}
	if contentsArg(arg) {
		fi, err := os.Stat(src)
		if err != nil {
			return r, err
		}
		if !fi.IsDir() {
			return r, fmt.Errorf("%s: not a directory", arg)
		}
		r.Contents, r.Kind, r.Dst = true, Dir, dst
		return r, nil
	}
	fi, err := os.Lstat(src)
	if err != nil {
		return r, err
	}
	switch m := fi.Mode(); {
	case m.IsDir():
		r.Kind = Dir
	case m.IsRegular():
		r.Kind = File
	case m&fs.ModeSymlink != 0:
		r.Kind = Symlink
	default:
		return r, fmt.Errorf("%s: not a file, directory or link", arg)
	}
	r.Label = filepath.Base(src)
	r.Dst = filepath.Join(dst, r.Label)
	return r, nil
}

// checkInside refuses to copy a directory into itself.
func checkInside(p *Plan) error {
	d, err := realish(p.Dst)
	if err != nil {
		return err
	}
	for _, r := range p.Roots {
		if r.Kind != Dir {
			continue
		}
		s, err := filepath.EvalSymlinks(r.Src)
		if err != nil {
			return err
		}
		if s, err = filepath.Abs(s); err != nil {
			return err
		}
		if within(d, s) {
			return fmt.Errorf("destination %s is inside source %s", p.Dst, r.Arg)
		}
	}
	return nil
}

// realish resolves the longest existing prefix of path and appends the rest.
func realish(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	rest := ""
	for cur := abs; ; {
		if r, err := filepath.EvalSymlinks(cur); err == nil {
			return filepath.Join(r, rest), nil
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return abs, nil
		}
		rest = filepath.Join(filepath.Base(cur), rest)
		cur = parent
	}
}

// within reports whether path is dir or under it.
func within(path, dir string) bool {
	if path == dir {
		return true
	}
	if dir == "/" {
		return strings.HasPrefix(path, "/")
	}
	return strings.HasPrefix(path, dir+"/")
}

// Entry is one thing found under a root.
type Entry struct {
	Root int
	Src  string
	Dst  string
	Show string // path as shown in the display: relative to the destination
	Rel  string // path relative to the root ("" for the root itself)
	Info fs.FileInfo
	Err  error // the walk could not read this path
}

// Walk visits every root in order, parents before children. A directory that
// can't be read is reported with Err set and skipped; the walk goes on.
func Walk(ctx context.Context, p *Plan, visit func(Entry)) error {
	for i, r := range p.Roots {
		if r.Kind != Dir {
			fi, err := os.Lstat(r.Src)
			visit(Entry{Root: i, Src: r.Src, Dst: r.Dst, Show: r.Label, Info: fi, Err: err})
			continue
		}
		top := r.Src
		if r.Contents {
			// A trailing slash follows a link to a directory; WalkDir would not.
			if t, err := filepath.EvalSymlinks(r.Src); err == nil {
				top = t
			}
		}
		walkErr := filepath.WalkDir(top, func(path string, d fs.DirEntry, err error) error {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			rel, _ := filepath.Rel(top, path)
			if rel == "." {
				rel = ""
			}
			e := Entry{Root: i, Src: path, Dst: filepath.Join(r.Dst, rel), Show: filepath.Join(r.Label, rel), Rel: rel, Err: err}
			if err == nil {
				e.Info, e.Err = d.Info()
			}
			visit(e)
			if e.Err != nil && d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		})
		if walkErr != nil && ctx.Err() != nil {
			return ctx.Err()
		}
	}
	return nil
}

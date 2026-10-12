package plan

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

func tree(t *testing.T) string {
	t.Helper()
	base := t.TempDir()
	for _, p := range []string{"source/a.txt", "source/sub/b.txt", "other/c.txt", "lone.txt"} {
		full := filepath.Join(base, p)
		os.MkdirAll(filepath.Dir(full), 0o755)
		os.WriteFile(full, []byte(p), 0o644)
	}
	os.Symlink("source", filepath.Join(base, "link"))
	return base
}

func TestDirectoryVersusContents(t *testing.T) {
	base := tree(t)
	dst := filepath.Join(base, "dst")
	cases := []struct {
		arg      string
		wantDst  string
		contents bool
		label    string
	}{
		{"source", "dst/source", false, "source"},
		{"source/", "dst", true, ""},
		{"source/.", "dst", true, ""},
		{"lone.txt", "dst/lone.txt", false, "lone.txt"},
		{"link", "dst/link", false, "link"},
		{"link/", "dst", true, ""},
	}
	for _, c := range cases {
		p, err := Resolve([]string{filepath.Join(base, c.arg) + trailing(c.arg), dst})
		if err != nil {
			t.Fatalf("%s: %v", c.arg, err)
		}
		r := p.Roots[0]
		if got, _ := filepath.Rel(base, r.Dst); got != c.wantDst || r.Contents != c.contents || r.Label != c.label {
			t.Errorf("%s -> dst %s contents %v label %q; want %s %v %q", c.arg, got, r.Contents, r.Label, c.wantDst, c.contents, c.label)
		}
	}
	if p, _ := Resolve([]string{filepath.Join(base, "link"), dst}); p.Roots[0].Kind != Symlink {
		t.Error("a link without a trailing slash should be copied as a link")
	}
}

// filepath.Join drops trailing slashes; put them back.
func trailing(arg string) string {
	if strings.HasSuffix(arg, "/") {
		return "/"
	}
	if strings.HasSuffix(arg, "/.") {
		return "/."
	}
	return ""
}

func TestCurrentDirectoryIsContents(t *testing.T) {
	base := tree(t)
	wd, _ := os.Getwd()
	defer os.Chdir(wd)
	os.Chdir(filepath.Join(base, "source"))
	p, err := Resolve([]string{".", filepath.Join(base, "dst")})
	if err != nil {
		t.Fatal(err)
	}
	if !p.Roots[0].Contents {
		t.Error(`"." should copy contents`)
	}
}

func TestRefusals(t *testing.T) {
	base := tree(t)
	j := func(s ...string) string { return filepath.Join(append([]string{base}, s...)...) }
	bad := [][]string{
		{j("source")},                                          // no destination
		{j("missing"), j("dst")},                               // no such source
		{j("lone.txt") + "/", j("dst")},                        // trailing slash on a file
		{j("source"), j("source", "inside")},                   // into itself
		{j("source") + "/", j("source")},                       // contents into itself
		{j("link") + "/", j("source", "x")},                    // into itself through a link
		{j("source"), j("lone.txt")},                           // destination is a file
		{j("lone.txt"), j("other") + "/../lone.txt", j("dst")}, // two files, same name
	}
	for _, args := range bad {
		if _, err := Resolve(args); err == nil {
			t.Errorf("Resolve(%q) succeeded; want an error", args)
		}
	}
}

func TestOverlapDetected(t *testing.T) {
	base := tree(t)
	p, err := Resolve([]string{filepath.Join(base, "source") + "/", filepath.Join(base, "other") + "/", filepath.Join(base, "dst")})
	if err != nil {
		t.Fatal(err)
	}
	if !p.Overlap {
		t.Error("two contents copies into one destination overlap")
	}
	p, _ = Resolve([]string{filepath.Join(base, "source"), filepath.Join(base, "other"), filepath.Join(base, "dst")})
	if p.Overlap {
		t.Error("dst/source and dst/other do not overlap")
	}
}

func TestWalkMultipleRoots(t *testing.T) {
	base := tree(t)
	dst := filepath.Join(base, "dst")
	p, err := Resolve([]string{filepath.Join(base, "source"), filepath.Join(base, "other") + "/", filepath.Join(base, "lone.txt"), dst})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	Walk(context.Background(), p, func(e Entry) {
		if e.Err != nil {
			t.Fatalf("walk error at %s: %v", e.Src, e.Err)
		}
		rel, _ := filepath.Rel(dst, e.Dst)
		got = append(got, rel+" ("+e.Show+")")
	})
	sort.Strings(got)
	want := []string{". ()", "c.txt (c.txt)", "lone.txt (lone.txt)", "source (source)", "source/a.txt (source/a.txt)", "source/sub (source/sub)", "source/sub/b.txt (source/sub/b.txt)"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("walk:\n got %q\nwant %q", got, want)
	}
}

func TestWalkFollowsLinkWithSlash(t *testing.T) {
	base := tree(t)
	p, err := Resolve([]string{filepath.Join(base, "link") + "/", filepath.Join(base, "dst")})
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	Walk(context.Background(), p, func(e Entry) {
		if e.Err == nil && e.Info.Mode().IsRegular() {
			n++
		}
	})
	if n != 2 {
		t.Fatalf("found %d files through link/, want 2", n)
	}
}

func TestWalkReportsUnreadableDirectory(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads everything")
	}
	base := tree(t)
	locked := filepath.Join(base, "source", "sub")
	os.Chmod(locked, 0)
	defer os.Chmod(locked, 0o755)
	p, _ := Resolve([]string{filepath.Join(base, "source"), filepath.Join(base, "dst")})
	errs, files := 0, 0
	Walk(context.Background(), p, func(e Entry) {
		if e.Err != nil {
			errs++
		} else if e.Info.Mode().IsRegular() {
			files++
		}
	})
	if errs != 1 || files != 1 {
		t.Fatalf("errors %d files %d; want 1 and 1", errs, files)
	}
}

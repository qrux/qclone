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
		{j("source"), j("SOURCE", "backup")},                   // into itself, other case (APFS ignores case)
		{j("source", "sub") + "/", j("source")},                // contents source inside its destination
		{j("lone.txt"), base},                                  // a file onto itself
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
	Walk(context.Background(), p, true, func(e Entry) bool {
		if e.Err != nil {
			t.Fatalf("walk error at %s: %v", e.Src, e.Err)
		}
		rel, _ := filepath.Rel(dst, e.Dst)
		got = append(got, rel+" ("+e.Show+")")
		return true
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
	Walk(context.Background(), p, true, func(e Entry) bool {
		if e.Err == nil && e.Info.Mode().IsRegular() {
			n++
		}
		return true
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
	Walk(context.Background(), p, true, func(e Entry) bool {
		if e.Err != nil {
			errs++
		} else if e.Info.Mode().IsRegular() {
			files++
		}
		return true
	})
	if errs != 1 || files != 1 {
		t.Fatalf("errors %d files %d; want 1 and 1", errs, files)
	}
}

func TestWalkOrders(t *testing.T) {
	base := t.TempDir()
	for _, n := range []string{"c", "a", "b"} {
		os.WriteFile(filepath.Join(base, n), nil, 0o644)
	}
	p, _ := Resolve([]string{base + "/", filepath.Join(t.TempDir(), "dst")})
	names := func(sorted bool) string {
		var got []string
		Walk(context.Background(), p, sorted, func(e Entry) bool {
			if e.Rel != "" {
				got = append(got, e.Rel)
			}
			return true
		})
		return strings.Join(got, "")
	}
	if got := names(true); got != "abc" {
		t.Fatalf("sorted walk = %q", got)
	}
	if got := names(false); len(got) != 3 {
		t.Fatalf("unsorted walk = %q, want all three in any order", got)
	}
	for in, want := range map[string]Order{"hash": OrderHash, "dfs": OrderDFS, "dirent": OrderDirent} {
		if o, err := ParseOrder(in); err != nil || o != want || o.String() != in {
			t.Errorf("ParseOrder(%q) = %v, %v", in, o, err)
		}
	}
	if _, err := ParseOrder("random"); err == nil {
		t.Error("ParseOrder accepted an unknown order")
	}
}

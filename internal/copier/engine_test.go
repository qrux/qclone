package copier

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"io/fs"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"qclone/internal/plan"
	"qclone/internal/sim"
	"qclone/internal/tuner"
)

type spec struct {
	path string
	size int
	mode fs.FileMode
}

// makeTree writes files of pseudo-random bytes with old mtimes.
func makeTree(t *testing.T, root string, files []spec) {
	t.Helper()
	rng := rand.New(rand.NewSource(int64(len(files))))
	mt := time.Date(2014, 3, 1, 12, 0, 0, 123456789, time.UTC)
	for _, f := range files {
		p := filepath.Join(root, f.path)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		b := make([]byte, f.size)
		rng.Read(b)
		mode := f.mode
		if mode == 0 {
			mode = 0o644
		}
		if err := os.WriteFile(p, b, mode); err != nil {
			t.Fatal(err)
		}
		os.Chmod(p, mode)
		os.Chtimes(p, mt, mt)
	}
}

func opts(errPath string) Options {
	cfg := tuner.DefaultConfig()
	cfg.MaxReaders = 8
	return Options{
		Readers:      4,
		Tuner:        cfg,
		MaxStuck:     16,
		Chunk:        256 << 10,
		ModifyWindow: time.Second,
		Retries:      3,
		Errors:       NewErrorLog(errPath, []string{"test"}, time.Now()),
	}
}

func run(t *testing.T, args []string, o Options) (*Engine, Snapshot) {
	t.Helper()
	p, err := plan.Resolve(args)
	if err != nil {
		t.Fatal(err)
	}
	e := New(p, o)
	if err := e.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	o.Errors.Close()
	return e, e.Snapshot()
}

func sum(t *testing.T, p string) [32]byte {
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return sha256.Sum256(b)
}

func sameTree(t *testing.T, src, dst string) int {
	t.Helper()
	n := 0
	filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		rel, _ := filepath.Rel(src, p)
		q := filepath.Join(dst, rel)
		si, _ := os.Lstat(p)
		di, err := os.Lstat(q)
		if err != nil {
			t.Errorf("missing %s", rel)
			return nil
		}
		if si.Mode() != di.Mode() {
			t.Errorf("%s: mode %v, want %v", rel, di.Mode(), si.Mode())
		}
		switch {
		case si.Mode().IsRegular():
			n++
			if sum(t, p) != sum(t, q) {
				t.Errorf("%s: contents differ", rel)
			}
			if !si.ModTime().Equal(di.ModTime()) {
				t.Errorf("%s: mtime %v, want %v", rel, di.ModTime(), si.ModTime())
			}
		case si.Mode()&fs.ModeSymlink != 0:
			a, _ := os.Readlink(p)
			b, _ := os.Readlink(q)
			if a != b {
				t.Errorf("%s: link %q, want %q", rel, b, a)
			}
		case si.IsDir() && rel != ".":
			if !si.ModTime().Equal(di.ModTime()) {
				t.Errorf("dir %s: mtime %v, want %v", rel, di.ModTime(), si.ModTime())
			}
		}
		return nil
	})
	return n
}

func noPartials(t *testing.T, dst string) {
	t.Helper()
	filepath.WalkDir(dst, func(p string, d fs.DirEntry, err error) error {
		if d != nil && strings.HasPrefix(d.Name(), ".qclone-") {
			t.Errorf("leftover partial file %s", p)
		}
		return nil
	})
}

func TestCopiesTreeExactly(t *testing.T) {
	base := t.TempDir()
	src := filepath.Join(base, "source")
	makeTree(t, src, []spec{
		{"big.bin", 5<<20 + 12345, 0},    // many chunks, odd tail
		{"exact.bin", 1 << 20, 0},        // exact multiple of the chunk
		{"empty.txt", 0, 0},              // zero bytes
		{"sub/deeper/a.txt", 777, 0o600}, // private mode
		{"sub/script.sh", 4096, 0o755},   // executable
	})
	for i := 0; i < 200; i++ { // many small files side by side
		makeTree(t, filepath.Join(src, "many"), []spec{{filepath.Join("d"+string(rune('a'+i%7)), "f"+strings.Repeat("x", i%5)+string(rune('A'+i%26))+".dat"), 1000 + i*37, 0}})
	}
	os.MkdirAll(filepath.Join(src, "empty-dir"), 0o750)
	os.Symlink("big.bin", filepath.Join(src, "link-to-big"))
	os.Symlink("/nonexistent/target", filepath.Join(src, "dangling"))
	old := time.Date(2012, 1, 2, 3, 4, 5, 0, time.UTC)
	os.Chtimes(filepath.Join(src, "sub"), old, old)
	os.Chtimes(filepath.Join(src, "empty-dir"), old, old)

	dst := filepath.Join(base, "dst")
	errFile := filepath.Join(base, "errors.txt")
	_, s := run(t, []string{src, dst}, opts(errFile))
	n := sameTree(t, src, filepath.Join(dst, "source"))
	if s.FilesDone != int64(n) || s.FilesFailed != 0 || s.Errors != 0 {
		t.Fatalf("done %d of %d, failed %d, errors %d (%s)", s.FilesDone, n, s.FilesFailed, s.Errors, s.LastError)
	}
	if s.BytesDone != s.BytesTotal {
		t.Fatalf("bytes done %d of %d", s.BytesDone, s.BytesTotal)
	}
	if _, err := os.Stat(errFile); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("a clean run should not create the error file")
	}
	noPartials(t, dst)
}

func TestContentsAndMultipleSources(t *testing.T) {
	base := t.TempDir()
	makeTree(t, filepath.Join(base, "a"), []spec{{"one.txt", 10, 0}, {"x/two.txt", 20, 0}})
	makeTree(t, filepath.Join(base, "b"), []spec{{"three.txt", 30, 0}})
	makeTree(t, base, []spec{{"lone.txt", 40, 0}})
	dst := filepath.Join(base, "dst")
	run(t, []string{filepath.Join(base, "a") + "/", filepath.Join(base, "b"), filepath.Join(base, "lone.txt"), dst}, opts(filepath.Join(base, "e.txt")))
	for _, p := range []string{"one.txt", "x/two.txt", "b/three.txt", "lone.txt"} {
		if _, err := os.Stat(filepath.Join(dst, p)); err != nil {
			t.Errorf("missing %s: %v", p, err)
		}
	}
	if _, err := os.Stat(filepath.Join(dst, "a")); err == nil {
		t.Error("a/ (trailing slash) should copy contents, not the directory")
	}
}

func TestSkipsUnchangedOnRerun(t *testing.T) {
	base := t.TempDir()
	src := filepath.Join(base, "s")
	makeTree(t, src, []spec{{"a.bin", 300000, 0}, {"b.bin", 1000, 0}})
	dst := filepath.Join(base, "d")
	run(t, []string{src + "/", dst}, opts(filepath.Join(base, "e.txt")))
	// Change one file's contents and size.
	os.WriteFile(filepath.Join(src, "b.bin"), []byte("changed"), 0o644)
	_, s := run(t, []string{src + "/", dst}, opts(filepath.Join(base, "e.txt")))
	if s.FilesSkipped != 1 || s.FilesDone != 1 {
		t.Fatalf("skipped %d, copied %d; want 1 and 1", s.FilesSkipped, s.FilesDone)
	}
	if got, _ := os.ReadFile(filepath.Join(dst, "b.bin")); string(got) != "changed" {
		t.Fatalf("b.bin = %q", got)
	}
}

func TestErrorsGoToTheFileNotStderr(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads everything")
	}
	base := t.TempDir()
	src := filepath.Join(base, "s")
	makeTree(t, src, []spec{{"ok.txt", 100, 0}, {"locked.txt", 100, 0}, {"bad-sector.bin", 3 << 20, 0}})
	os.Chmod(filepath.Join(src, "locked.txt"), 0)
	defer os.Chmod(filepath.Join(src, "locked.txt"), 0o644)
	syscall.Mkfifo(filepath.Join(src, "pipe"), 0o644)

	// Capture stderr to prove nothing goes there.
	r, w, _ := os.Pipe()
	saved := os.Stderr
	os.Stderr = w
	defer func() { os.Stderr = saved }()

	dst := filepath.Join(base, "d")
	errFile := filepath.Join(base, "errors.txt")
	o := opts(errFile)
	p, _ := plan.Resolve([]string{src + "/", dst})
	e := New(p, o)
	// An unreadable region at 2 MiB, as a dying drive returns: EIO every time.
	e.readFault = func(show string, off int64, n int) error {
		if show == "bad-sector.bin" && off+int64(n) > 2<<20 {
			return syscall.EIO
		}
		return nil
	}
	e.Run(context.Background())
	o.Errors.Close()
	w.Close()
	var stderr bytes.Buffer
	stderr.ReadFrom(r)
	os.Stderr = saved

	if stderr.Len() != 0 {
		t.Fatalf("wrote to stderr: %q", stderr.String())
	}
	s := e.Snapshot()
	if s.FilesDone != 1 || s.FilesFailed != 2 || s.Errors != 3 {
		t.Fatalf("done %d failed %d errors %d; want 1, 2, 3", s.FilesDone, s.FilesFailed, s.Errors)
	}
	log, err := os.ReadFile(errFile)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"locked.txt", "permission denied", "bad-sector.bin", "unreadable: ", "1048576 bytes", "kept for the next run", "named pipe", "# command: qclone", "# stopped"} {
		if !bytes.Contains(log, []byte(want)) {
			t.Errorf("error file lacks %q:\n%s", want, log)
		}
	}
	if _, err := os.Stat(filepath.Join(dst, "bad-sector.bin")); err == nil {
		t.Error("a file that failed must not appear at the destination")
	}
	if s.Retries < 2 {
		t.Errorf("retries = %d, want the bad chunk retried", s.Retries)
	}
	if s.BadBytes != 1<<20 || s.BadFiles != 1 {
		t.Errorf("bad bytes %d in %d files; want the last 1 MiB of one file", s.BadBytes, s.BadFiles)
	}
	// The readable 2 MiB are kept for the next run.
	if n := partials(dst); n != 2 {
		t.Errorf("%d .qclone- files kept; want the part and its map", n)
	}
}

func partials(dst string) int {
	n := 0
	filepath.WalkDir(dst, func(p string, d fs.DirEntry, err error) error {
		if d != nil && strings.HasPrefix(d.Name(), ".qclone-") {
			n++
		}
		return nil
	})
	return n
}

// Ctrl-C keeps what was copied of a file in flight, and the next run carries
// on from there instead of reading it all again.
func TestCancelKeepsProgressAndResumes(t *testing.T) {
	base := t.TempDir()
	src := filepath.Join(base, "s")
	makeTree(t, src, []spec{{"old/slow.bin", 24 << 20, 0}})
	dst := filepath.Join(base, "d")
	o := opts(filepath.Join(base, "e.txt"))
	o.Sim = sim.NewDevice(sim.Degraded()) // "old/" is slow: about 0.6 MB/s per read
	p, _ := plan.Resolve([]string{src + "/", dst})
	e := New(p, o)
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(2 * time.Second); cancel() }()
	e.Run(ctx)
	s := e.Snapshot()
	if !s.Canceled || s.Errors != 0 {
		t.Fatalf("canceled %v errors %d (%s)", s.Canceled, s.Errors, s.LastError)
	}
	if _, err := os.Stat(filepath.Join(dst, "old", "slow.bin")); err == nil {
		t.Fatal("canceled file was installed")
	}
	if n := partials(dst); n != 2 {
		t.Fatalf("%d .qclone- files after cancel; want the part and its map", n)
	}
	first := e.readBytes.Load()

	o2 := opts(filepath.Join(base, "e2.txt"))
	o2.Sim = sim.NewDevice(sim.Degraded())
	e2 := New(p, o2)
	e2.Run(context.Background())
	s2 := e2.Snapshot()
	if s2.FilesDone != 1 || s2.Errors != 0 {
		t.Fatalf("rerun: done %d errors %d (%s)", s2.FilesDone, s2.Errors, s2.LastError)
	}
	if s2.BytesResumed == 0 || s2.BytesResumed > first {
		t.Fatalf("resumed %d bytes; the first run read %d", s2.BytesResumed, first)
	}
	if got := e2.readBytes.Load(); got+s2.BytesResumed != 24<<20 {
		t.Fatalf("rerun read %d + resumed %d, want exactly the file's %d", got, s2.BytesResumed, 24<<20)
	}
	sameTree(t, src, dst)
	noPartials(t, dst)
}

// Under the simulated degraded drive, the rate shown is positive at every
// frame once bytes have moved, and the copy is still exact.
func TestRateNeverZeroOnSimulatedDegradedDrive(t *testing.T) {
	base := t.TempDir()
	src := filepath.Join(base, "s")
	makeTree(t, src, []spec{{"old/a.bin", 1 << 20, 0}, {"old/b.bin", 1 << 20, 0}, {"new/c.bin", 8 << 20, 0}})
	dst := filepath.Join(base, "d")
	o := opts(filepath.Join(base, "e.txt"))
	o.Sim = sim.NewDevice(sim.Degraded())
	p, _ := plan.Resolve([]string{src + "/", dst})
	e := New(p, o)
	done := make(chan struct{})
	go func() { e.Run(context.Background()); close(done) }()
	frames, zero := 0, 0
	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()
loop:
	for {
		select {
		case <-done:
			break loop
		case <-tick.C:
			s := e.Snapshot()
			if s.BytesDone > 0 {
				frames++
				if !s.RateOK || s.Rate <= 0 {
					zero++
				}
			}
		}
	}
	if frames < 10 || zero != 0 {
		t.Fatalf("%d frames after the first byte, %d with no positive rate", frames, zero)
	}
	sameTree(t, src, dst)
}

// Two sources whose files differ only by letter case land on one file on a
// disk that ignores case: the second is skipped and logged, and the first is
// installed whole (they used to share a temp file and mix).
func TestCaseCollisionSkipsSecond(t *testing.T) {
	base := t.TempDir()
	if caseSensitive(base) {
		t.Skip("temp folder is on a case-sensitive volume")
	}
	makeTree(t, filepath.Join(base, "a"), []spec{{"Docs/x.bin", 3 << 20, 0}})
	makeTree(t, filepath.Join(base, "b"), []spec{{"docs/x.bin", 1 << 20, 0}})
	dst := filepath.Join(base, "dst")
	_, s := run(t, []string{filepath.Join(base, "a") + "/", filepath.Join(base, "b") + "/", dst}, opts(filepath.Join(base, "e.txt")))
	if s.FilesDone != 1 || s.Errors != 1 {
		t.Fatalf("done %d errors %d (%s); want 1 and 1", s.FilesDone, s.Errors, s.LastError)
	}
	if sum(t, filepath.Join(dst, "Docs", "x.bin")) != sum(t, filepath.Join(base, "a", "Docs", "x.bin")) {
		t.Fatal("destination does not hold the first source's file")
	}
	noPartials(t, dst)
}

// A read-only source directory must not stop a rerun from retrying a file
// that failed in it.
func TestRerunRetriesUnderReadOnlyDirectory(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes everywhere")
	}
	base := t.TempDir()
	src := filepath.Join(base, "s")
	makeTree(t, src, []spec{{"ro/flaky.bin", 1 << 20, 0}, {"ro/fine.txt", 10, 0}})
	os.Chmod(filepath.Join(src, "ro"), 0o555)
	defer os.Chmod(filepath.Join(src, "ro"), 0o755)
	dst := filepath.Join(base, "d")
	defer filepath.WalkDir(dst, func(p string, d fs.DirEntry, err error) error {
		if d != nil && d.IsDir() {
			os.Chmod(p, 0o755) // so t.TempDir can clean up
		}
		return nil
	})
	o := opts(filepath.Join(base, "e1.txt"))
	p, _ := plan.Resolve([]string{src + "/", dst})
	e := New(p, o)
	e.readFault = func(show string, off int64, n int) error {
		if strings.HasSuffix(show, "flaky.bin") {
			return syscall.EIO
		}
		return nil
	}
	e.Run(context.Background())
	o.Errors.Close()
	if fi, _ := os.Stat(filepath.Join(dst, "ro")); fi.Mode().Perm() != 0o555 {
		t.Fatalf("dir mode %v after the run, want the source's 0555", fi.Mode().Perm())
	}
	_, s := run(t, []string{src + "/", dst}, opts(filepath.Join(base, "e2.txt")))
	if s.FilesDone != 1 || s.FilesSkipped != 1 || s.Errors != 0 {
		t.Fatalf("rerun: done %d skipped %d errors %d (%s)", s.FilesDone, s.FilesSkipped, s.Errors, s.LastError)
	}
	sameTree(t, src, dst)
}

// Ctrl-C while a bad chunk is waiting to be retried is a cancel, not an error.
func TestCancelDuringRetryIsNotAnError(t *testing.T) {
	base := t.TempDir()
	src := filepath.Join(base, "s")
	makeTree(t, src, []spec{{"bad.bin", 1 << 20, 0}})
	dst := filepath.Join(base, "d")
	o := opts(filepath.Join(base, "e.txt"))
	o.Retries = 50
	p, _ := plan.Resolve([]string{src + "/", dst})
	e := New(p, o)
	e.readFault = func(string, int64, int) error { return syscall.EIO }
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(600 * time.Millisecond); cancel() }()
	e.Run(ctx)
	s := e.Snapshot()
	if !s.Canceled || s.FilesFailed != 0 || s.Errors != 0 {
		t.Fatalf("canceled %v failed %d errors %d (%s)", s.Canceled, s.FilesFailed, s.Errors, s.LastError)
	}
	noPartials(t, dst)
}

// A symlink where the destination needs a directory is not followed.
func TestDestinationSymlinkNotFollowed(t *testing.T) {
	base := t.TempDir()
	src := filepath.Join(base, "s")
	makeTree(t, src, []spec{{"sub/f.txt", 10, 0}, {"top.txt", 10, 0}})
	outside := filepath.Join(base, "outside")
	os.MkdirAll(outside, 0o755)
	dst := filepath.Join(base, "d")
	os.MkdirAll(dst, 0o755)
	os.Symlink(outside, filepath.Join(dst, "sub"))
	_, s := run(t, []string{src + "/", dst}, opts(filepath.Join(base, "e.txt")))
	if entries, _ := os.ReadDir(outside); len(entries) != 0 {
		t.Fatalf("wrote %d entries through the symlink, outside the destination", len(entries))
	}
	if s.FilesDone != 1 || s.Errors != 1 {
		t.Fatalf("done %d errors %d (%s)", s.FilesDone, s.Errors, s.LastError)
	}
}

// A partial file left by an interrupted run is replaced, not left behind.
func TestReusesLeftoverPartial(t *testing.T) {
	base := t.TempDir()
	src := filepath.Join(base, "s")
	makeTree(t, src, []spec{{"a.bin", 500000, 0}})
	dst := filepath.Join(base, "d")
	os.MkdirAll(dst, 0o755)
	e := New(&plan.Plan{Dst: dst}, opts(filepath.Join(base, "x.txt")))
	stale, _ := e.claimTmp(filepath.Join(dst, "a.bin"))
	os.WriteFile(stale, []byte("old partial"), 0o600)
	run(t, []string{src + "/", dst}, opts(filepath.Join(base, "e.txt")))
	sameTree(t, src, dst)
	noPartials(t, dst)
}

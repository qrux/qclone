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

	"tclone/internal/plan"
	"tclone/internal/sim"
	"tclone/internal/tuner"
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
		Start:        tuner.Setting{Readers: 4, Chunk: 256 << 10},
		Tuner:        cfg,
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
		if d != nil && strings.HasPrefix(d.Name(), ".tclone-") {
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
	e.readFault = func(show string, off int64) error {
		if show == "bad-sector.bin" && off >= 2<<20 {
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
	for _, want := range []string{"locked.txt", "permission denied", "bad-sector.bin", "input/output error (3 tries)", "named pipe", "# command: tclone", "# stopped"} {
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
	noPartials(t, dst)
}

func TestCancelCleansUp(t *testing.T) {
	base := t.TempDir()
	src := filepath.Join(base, "s")
	makeTree(t, src, []spec{{"old/slow.bin", 4 << 20, 0}})
	dst := filepath.Join(base, "d")
	o := opts(filepath.Join(base, "e.txt"))
	o.Sim = sim.NewDevice(sim.Degraded()) // "old/" is slow: about 0.6 MB/s per read
	p, _ := plan.Resolve([]string{src + "/", dst})
	e := New(p, o)
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(700 * time.Millisecond); cancel() }()
	e.Run(ctx)
	s := e.Snapshot()
	if !s.Canceled {
		t.Fatal("not marked canceled")
	}
	if _, err := os.Stat(filepath.Join(dst, "old", "slow.bin")); err == nil {
		t.Fatal("canceled file was installed")
	}
	if s.Errors != 0 {
		t.Fatalf("cancel logged %d errors: %s", s.Errors, s.LastError)
	}
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

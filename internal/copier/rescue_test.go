package copier

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"

	"qclone/internal/plan"
	"qclone/internal/sim"
)

func TestRanges(t *testing.T) {
	var r ranges
	r.add(100, 50)  // [100,150)
	r.add(0, 10)    // [0,10)
	r.add(150, 10)  // touches: [100,160)
	r.add(5, 100)   // bridges: [0,160)
	r.add(300, 0)   // empty: ignored
	r.add(200, 100) // [200,300)
	want := ranges{{0, 160}, {200, 100}}
	if !reflect.DeepEqual(r, want) {
		t.Fatalf("ranges = %v, want %v", r, want)
	}
	if r.total() != 260 || r.last() != 300 {
		t.Fatalf("total %d last %d", r.total(), r.last())
	}
	if g := r.gaps(400); !reflect.DeepEqual(g, ranges{{160, 40}, {300, 100}}) {
		t.Fatalf("gaps = %v", g)
	}
	if g := (ranges{}).gaps(10); !reflect.DeepEqual(g, ranges{{0, 10}}) {
		t.Fatalf("gaps of nothing = %v", g)
	}
	if _, ok := fromPairs([][2]int64{{0, 10}, {5, 20}}, 20); ok {
		t.Fatal("a range past the end must be refused")
	}
}

// badDrive fails any read that touches a bad span, as a dying drive does
// for a sector it can't correct.
func badDrive(bad ranges) func(string, int64, int) error {
	return func(show string, off int64, n int) error {
		for _, s := range bad {
			if off < s.end() && s.Off < off+int64(n) {
				return syscall.EIO
			}
		}
		return nil
	}
}

func runWith(t *testing.T, args []string, o Options, fault func(string, int64, int) error) *Engine {
	t.Helper()
	p, err := plan.Resolve(args)
	if err != nil {
		t.Fatal(err)
	}
	e := New(p, o)
	e.readFault = fault
	if err := e.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	o.Errors.Close()
	return e
}

func TestBadSectorsMappedThenRescuedThenPatched(t *testing.T) {
	base := t.TempDir()
	src := filepath.Join(base, "s")
	makeTree(t, src, []spec{{"disk.img", 6 << 20, 0}})
	dst := filepath.Join(base, "d")
	bad := ranges{{2<<20 + 8192, 4096}, {5 << 20, 16384}}
	args := []string{src + "/", dst}
	orig, _ := os.ReadFile(filepath.Join(src, "disk.img"))

	// 1. Without --rescue: exact ranges found, file not installed, progress kept.
	err1 := filepath.Join(base, "e1.txt")
	e := runWith(t, args, opts(err1), badDrive(bad))
	s := e.Snapshot()
	if s.BadRanges != 2 || s.BadBytes != 20480 || s.FilesFailed != 1 || s.FilesDone != 0 {
		t.Fatalf("run 1: bad %d ranges %d bytes, failed %d, done %d", s.BadRanges, s.BadBytes, s.FilesFailed, s.FilesDone)
	}
	if _, err := os.Stat(filepath.Join(dst, "disk.img")); err == nil {
		t.Fatal("run 1 installed a file with unreadable ranges")
	}
	log1, _ := os.ReadFile(err1)
	for _, want := range []string{"4096 bytes at 2105344", "16384 bytes at 5242880", "(2 ranges, 20480 bytes)", "add --rescue"} {
		if !bytes.Contains(log1, []byte(want)) {
			t.Errorf("run 1 error file lacks %q:\n%s", want, log1)
		}
	}
	b := e.tmpBase(filepath.Join(dst, "disk.img"))
	m, err := loadMap(mapPath(b))
	if err != nil {
		t.Fatalf("run 1 left no map: %v", err)
	}
	if got, _ := fromPairs(m.Bad, 6<<20); !reflect.DeepEqual(got, bad) {
		t.Fatalf("map's bad ranges %v, want %v", got, bad)
	}

	// 2. --rescue, drive no better: only the holes are read again, the file is
	// installed with zeros exactly there, and a marker remembers them.
	o2 := opts(filepath.Join(base, "e2.txt"))
	o2.Rescue = true
	e = runWith(t, args, o2, badDrive(bad))
	s = e.Snapshot()
	if s.FilesRescued != 1 || s.FilesDone != 1 || s.BadBytes != 20480 || s.BytesResumed != 6<<20-20480 {
		t.Fatalf("run 2: rescued %d done %d bad %d resumed %d", s.FilesRescued, s.FilesDone, s.BadBytes, s.BytesResumed)
	}
	if got := e.readBytes.Load(); got != 0 {
		t.Fatalf("run 2 read %d bytes; only the bad ranges should have been tried, and they fail", got)
	}
	got, _ := os.ReadFile(filepath.Join(dst, "disk.img"))
	want := append([]byte(nil), orig...)
	for _, sp := range bad {
		copy(want[sp.Off:sp.end()], make([]byte, sp.Len))
	}
	if !bytes.Equal(got, want) {
		t.Fatal("run 2: installed file is not the source with zeros in exactly the bad ranges")
	}
	if fi, _ := os.Stat(filepath.Join(dst, "disk.img")); !fi.ModTime().Equal(time.Date(2014, 3, 1, 12, 0, 0, 123456789, time.UTC)) {
		t.Fatalf("run 2: mtime %v", fi.ModTime())
	}
	if _, err := os.Stat(markerPath(b)); err != nil {
		t.Fatalf("run 2 left no .bad marker: %v", err)
	}
	if _, err := os.Stat(mapPath(b)); err == nil {
		t.Fatal("run 2 left the .map behind after installing")
	}

	// 3. The drive reads the sectors now: the marker makes qclone re-read just
	// those ranges, even though size and time match, and the file is whole.
	e = runWith(t, args, opts(filepath.Join(base, "e3.txt")), nil)
	s = e.Snapshot()
	if s.FilesDone != 1 || s.FilesSkipped != 0 || s.Errors != 0 {
		t.Fatalf("run 3: done %d skipped %d errors %d (%s)", s.FilesDone, s.FilesSkipped, s.Errors, s.LastError)
	}
	if got := e.readBytes.Load(); got != 20480 {
		t.Fatalf("run 3 read %d bytes, want just the 20480 that were missing", got)
	}
	sameTree(t, src, dst)
	noPartials(t, dst)

	// 4. And now it's simply unchanged.
	e = runWith(t, args, opts(filepath.Join(base, "e4.txt")), nil)
	if s = e.Snapshot(); s.FilesSkipped != 1 || s.FilesDone != 0 {
		t.Fatalf("run 4: skipped %d done %d", s.FilesSkipped, s.FilesDone)
	}
}

// A bad sector in a big chunk costs a handful of reads, not one per sector.
func TestBisectionIsCheap(t *testing.T) {
	base := t.TempDir()
	src := filepath.Join(base, "s")
	makeTree(t, src, []spec{{"f.bin", 1 << 20, 0}})
	reads := 0
	fault := badDrive(ranges{{777 * 1024 &^ 4095, 4096}})
	o := opts(filepath.Join(base, "e.txt"))
	o.Chunk = 1 << 20
	o.Rescue = true
	runWith(t, []string{src + "/", filepath.Join(base, "d")}, o, func(s string, off int64, n int) error {
		reads++
		return fault(s, off, n)
	})
	// 3 tries of the whole chunk, then about two reads per level of halving.
	if reads > 3+2*8 {
		t.Fatalf("%d reads to find one bad sector in 1 MiB", reads)
	}
}

// A part and map left by a run that was killed outright are picked up.
func TestResumesFromLeftoverMap(t *testing.T) {
	base := t.TempDir()
	src := filepath.Join(base, "s")
	makeTree(t, src, []spec{{"f.bin", 3 << 20, 0}})
	srcFile := filepath.Join(src, "f.bin")
	fi, _ := os.Stat(srcFile)
	orig, _ := os.ReadFile(srcFile)
	dst := filepath.Join(base, "d")
	os.MkdirAll(dst, 0o755)
	b := New(&plan.Plan{Dst: dst}, opts(filepath.Join(base, "x.txt"))).tmpBase(filepath.Join(dst, "f.bin"))

	leave := func(mtime int64) {
		os.WriteFile(partPath(b), orig[:1<<20], 0o600) // the first MiB, as if copied before the kill
		saveMap(mapPath(b), &mapFile{V: 1, Name: "f.bin", Size: 3 << 20, MTime: mtime, Done: [][2]int64{{0, 1 << 20}}})
	}

	leave(fi.ModTime().UnixNano())
	e := runWith(t, []string{src + "/", dst}, opts(filepath.Join(base, "e1.txt")), nil)
	if got := e.readBytes.Load(); got != 2<<20 {
		t.Fatalf("read %d bytes, want the missing 2 MiB", got)
	}
	sameTree(t, src, dst)
	noPartials(t, dst)

	// A map made for an older version of the source is not trusted.
	os.Remove(filepath.Join(dst, "f.bin"))
	leave(fi.ModTime().UnixNano() - 1)
	e = runWith(t, []string{src + "/", dst}, opts(filepath.Join(base, "e2.txt")), nil)
	if got := e.readBytes.Load(); got != 3<<20 {
		t.Fatalf("read %d bytes, want the whole file again", got)
	}
	sameTree(t, src, dst)
	noPartials(t, dst)
}

func TestDescribeLimits(t *testing.T) {
	var r ranges
	for i := int64(0); i < 40; i++ {
		r.add(i*8192, 4096)
	}
	d := r.describe(32)
	if !strings.Contains(d, "and 8 more") || strings.Count(d, " bytes at ") != 32 {
		t.Fatalf("describe = %q", d)
	}
}

// While a big file is copying, its map is rewritten every couple of seconds,
// and every range it claims really is in the part: what a killed run leaves.
func TestMapFlushedWhileRunning(t *testing.T) {
	base := t.TempDir()
	src := filepath.Join(base, "s")
	makeTree(t, src, []spec{{"old/big.bin", 32 << 20, 0}})
	orig, _ := os.ReadFile(filepath.Join(src, "old", "big.bin"))
	dst := filepath.Join(base, "d")
	o := opts(filepath.Join(base, "e.txt"))
	o.Sim = sim.NewDevice(sim.Degraded())
	p, _ := plan.Resolve([]string{src + "/", dst})
	e := New(p, o)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { e.Run(ctx); close(done) }()
	b := e.tmpBase(filepath.Join(dst, "old", "big.bin"))
	var m *mapFile
	for deadline := time.Now().Add(8 * time.Second); time.Now().Before(deadline); time.Sleep(200 * time.Millisecond) {
		if got, err := loadMap(mapPath(b)); err == nil && len(got.Done) > 0 {
			m = got
			break
		}
	}
	if m == nil {
		cancel()
		<-done
		t.Fatal("no map written while the copy ran")
	}
	part, _ := os.ReadFile(partPath(b))
	doneSet, ok := fromPairs(m.Done, 32<<20)
	if !ok {
		t.Fatalf("map has bad ranges: %v", m.Done)
	}
	for _, s := range doneSet {
		if s.end() > int64(len(part)) || !bytes.Equal(part[s.Off:s.end()], orig[s.Off:s.end()]) {
			t.Fatalf("map claims [%d,%d) but the part doesn't hold it", s.Off, s.end())
		}
	}
	cancel()
	<-done
}

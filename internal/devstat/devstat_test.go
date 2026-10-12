package devstat

import (
	"os"
	"runtime"
	"strings"
	"testing"
	"time"
)

// Reads the counters of the internal disk that holds the test's temp folder.
func TestForPathInternalDisk(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("macOS only")
	}
	dir := t.TempDir()
	src, err := ForPath(dir)
	if err != nil {
		t.Fatalf("ForPath: %v", err)
	}
	defer src.Close()
	info := src.Info()
	t.Logf("disk %+v", info)
	if !strings.HasPrefix(info.Name, "disk") {
		t.Fatalf("whole-disk name %q", info.Name)
	}
	a, err := src.Stats()
	if err != nil {
		t.Fatal(err)
	}
	// Make some I/O so the counters move.
	f, _ := os.CreateTemp(dir, "io")
	f.Write(make([]byte, 1<<20))
	f.Sync()
	f.Close()
	time.Sleep(50 * time.Millisecond)
	b, err := src.Stats()
	if err != nil {
		t.Fatal(err)
	}
	if b.ReadBytes < a.ReadBytes || b.WriteBytes < a.WriteBytes || b.ReadOps < a.ReadOps {
		t.Fatalf("counters went backward: %+v then %+v", a, b)
	}
	if a.ReadOps == 0 {
		t.Fatalf("no reads recorded on the boot disk: %+v", a)
	}
}

func TestDiff(t *testing.T) {
	a := Stats{ReadBytes: 0, ReadOps: 0, ReadTime: 0}
	b := Stats{ReadBytes: 100e6, ReadOps: 100, ReadTime: 4 * time.Second, ReadRetries: 3}
	r := Diff(a, b, 2*time.Second)
	if r.ReadBps != 50e6 || r.ReadIOPS != 50 || r.ReadLatency != 40*time.Millisecond || r.QueueDepth != 2 || r.Retries != 3 {
		t.Fatalf("diff = %+v", r)
	}
	if r := Diff(b, a, time.Second); r.ReadBps != 0 {
		t.Fatalf("counter reset should read as zero, got %+v", r)
	}
}

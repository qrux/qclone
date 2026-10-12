// Package sim pretends the source is a degraded SSD, for the demo and the
// tests. It is never used unless asked for (tclone --simulate degraded).
//
// The drive is modeled as one bandwidth server shared by all reads (what the
// controller and its error-correction engine can deliver in total) plus a
// floor on each read's own time (one read can only go so fast, however idle
// the drive). So n reads in flight deliver about min(n * OpRate, Bandwidth):
// more reads help until the drive saturates, which is the question tclone's
// tuner has to answer by measurement on a real drive.
package sim

import (
	"hash/fnv"
	"io"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"tclone/internal/devstat"
)

// Region is how one part of the drive behaves.
type Region struct {
	OpRate    float64       // bytes/s a single read can reach on its own
	Bandwidth float64       // bytes/s the whole drive can deliver to all reads together
	Overhead  time.Duration // fixed cost per read
}

// Profile maps files and offsets to regions.
type Profile struct {
	Fast, Slow Region
	RegionSize int64   // big files are split into regions this size
	SlowShare  float64 // share of a big file's regions that are slow
	OldMarker  string  // any path component starting with this marks the whole file slow

	BadEvery  int           // in slow regions, about one 4 KiB sector in this many can't be read (0: none)
	FailDelay time.Duration // how long a read that hits one takes to fail
}

// Degraded is the case tclone was built for: about 2 MB/s in old regions at rclone's
// default four transfers, and 500 MB/s in recently written ones. Whether a real
// drive's slow regions scale with more reads in flight is unknown; this one
// does, up to 8 MB/s.
func Degraded() Profile {
	return Profile{
		Fast:       Region{OpRate: 300e6, Bandwidth: 500e6, Overhead: 100 * time.Microsecond},
		Slow:       Region{OpRate: 0.6e6, Bandwidth: 8e6, Overhead: 2 * time.Millisecond},
		RegionSize: 32 << 20,
		SlowShare:  0.3,
		OldMarker:  "old",
	}
}

// Failing is Degraded with some sectors in its slow regions that can't be
// read at all, as on a drive past saving: for the rescue demo.
func Failing() Profile {
	p := Degraded()
	p.BadEvery = 6000
	p.FailDelay = 200 * time.Millisecond
	return p
}

// badSector says whether the 4 KiB sector at index i of the file named key is unreadable.
func (p Profile) badSector(key string, size, i int64) bool {
	if p.BadEvery <= 0 || p.RegionFor(key, size, i*4096) != p.Slow {
		return false
	}
	h := fnv.New32a()
	io.WriteString(h, key)
	var b [8]byte
	for k := range b {
		b[k] = byte(i >> (8 * k))
	}
	h.Write(b[:])
	h.Write([]byte("bad"))
	return h.Sum32()%uint32(p.BadEvery) == 0
}

// Model is the drive's queue in virtual time.
type Model struct {
	mu   sync.Mutex
	free time.Duration // when the bandwidth server is next idle
}

// Schedule returns when a read of n bytes in region r, issued at start, ends.
func (m *Model) Schedule(start time.Duration, r Region, n int) time.Duration {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.free < start {
		m.free = start
	}
	m.free += secs(float64(n) / r.Bandwidth)
	end := start + r.Overhead + secs(float64(n)/r.OpRate)
	if m.free > end {
		end = m.free
	}
	return end
}

func secs(s float64) time.Duration { return time.Duration(s * float64(time.Second)) }

// RegionFor says how the read at off in the file named key (a path relative
// to its source root) behaves.
func (p Profile) RegionFor(key string, size, off int64) Region {
	if p.OldMarker != "" {
		for _, part := range strings.Split(filepath.ToSlash(key), "/") {
			if strings.HasPrefix(strings.ToLower(part), p.OldMarker) {
				return p.Slow
			}
		}
	}
	if p.RegionSize > 0 && size >= 2*p.RegionSize {
		h := fnv.New32a()
		io.WriteString(h, key)
		var b [8]byte
		idx := off / p.RegionSize
		for i := range b {
			b[i] = byte(idx >> (8 * i))
		}
		h.Write(b[:])
		if float64(h.Sum32()%1000) < p.SlowShare*1000 {
			return p.Slow
		}
	}
	return p.Fast
}

// Device runs the model in real time and counts what it did, so it can stand
// in for the source disk in the display.
type Device struct {
	p     Profile
	m     Model
	start time.Time

	mu    sync.Mutex
	stats devstat.Stats
}

// NewDevice starts a simulated drive.
func NewDevice(p Profile) *Device { return &Device{p: p, start: time.Now()} }

// ReadAt reads from f as the simulated drive would: the bytes are real, the
// time taken is the model's.
func (d *Device) ReadAt(key string, size int64, f io.ReaderAt, buf []byte, off int64) (int, error) {
	for i := off / 4096; i*4096 < off+int64(len(buf)); i++ {
		if d.p.badSector(key, size, i) {
			time.Sleep(d.p.FailDelay)
			d.mu.Lock()
			d.stats.ReadErrors++
			d.mu.Unlock()
			return 0, syscall.EIO
		}
	}
	r := d.p.RegionFor(key, size, off)
	issued := time.Now()
	end := d.m.Schedule(issued.Sub(d.start), r, len(buf))
	n, err := f.ReadAt(buf, off)
	if wait := time.Until(d.start.Add(end)); wait > 0 {
		time.Sleep(wait)
	}
	took := time.Since(issued)
	d.mu.Lock()
	d.stats.ReadBytes += uint64(n)
	d.stats.ReadOps++
	d.stats.ReadTime += took
	d.mu.Unlock()
	return n, err
}

// Info implements devstat.Source.
func (d *Device) Info() devstat.Info {
	return devstat.Info{Name: "sim0", Model: "simulated degraded SSD", Interconnect: "simulated", Medium: "Solid State", Simulated: true}
}

// Stats implements devstat.Source.
func (d *Device) Stats() (devstat.Stats, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.stats, nil
}

// Close implements devstat.Source.
func (d *Device) Close() {}

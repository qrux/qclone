// Package devstat reads per-disk I/O counters: the same numbers iostat prints,
// taken straight from the kernel (IOKit's IOBlockStorageDriver "Statistics"
// on macOS) so they can be polled several times a second without spawning
// anything.
package devstat

import "time"

// Stats are cumulative counters since the disk appeared.
type Stats struct {
	ReadBytes, WriteBytes     uint64
	ReadOps, WriteOps         uint64
	ReadTime, WriteTime       time.Duration // summed over ops, so overlapping ops add up
	ReadErrors, WriteErrors   uint64
	ReadRetries, WriteRetries uint64
}

// Info describes the physical disk behind a path.
type Info struct {
	Name         string // BSD name of the whole disk, as iostat shows it: disk0, disk4
	Model        string
	Interconnect string // USB, PCI-Express, SATA, Apple Fabric, ...
	Medium       string // Solid State, Rotational
	Simulated    bool
}

// Source is something that can be polled for Stats.
type Source interface {
	Info() Info
	Stats() (Stats, error)
	Close()
}

// Rates are what changed between two samples.
type Rates struct {
	ReadBps, WriteBps   float64
	ReadIOPS, WriteIOPS float64
	ReadLatency         time.Duration // mean time per read op
	WriteLatency        time.Duration
	QueueDepth          float64 // mean ops in flight (reads + writes)
	ReadErrors, Retries uint64  // new in this interval
	WriteErrors         uint64
}

// Diff turns two samples dt apart into rates.
func Diff(a, b Stats, dt time.Duration) Rates {
	s := dt.Seconds()
	if s <= 0 {
		return Rates{}
	}
	r := Rates{
		ReadBps:     float64(sub(b.ReadBytes, a.ReadBytes)) / s,
		WriteBps:    float64(sub(b.WriteBytes, a.WriteBytes)) / s,
		ReadIOPS:    float64(sub(b.ReadOps, a.ReadOps)) / s,
		WriteIOPS:   float64(sub(b.WriteOps, a.WriteOps)) / s,
		ReadErrors:  sub(b.ReadErrors, a.ReadErrors),
		WriteErrors: sub(b.WriteErrors, a.WriteErrors),
		Retries:     sub(b.ReadRetries, a.ReadRetries) + sub(b.WriteRetries, a.WriteRetries),
	}
	rt := b.ReadTime - a.ReadTime
	wt := b.WriteTime - a.WriteTime
	if rt < 0 {
		rt = 0
	}
	if wt < 0 {
		wt = 0
	}
	if n := sub(b.ReadOps, a.ReadOps); n > 0 {
		r.ReadLatency = rt / time.Duration(n)
	}
	if n := sub(b.WriteOps, a.WriteOps); n > 0 {
		r.WriteLatency = wt / time.Duration(n)
	}
	r.QueueDepth = (rt + wt).Seconds() / s
	return r
}

func sub(b, a uint64) uint64 {
	if b < a {
		return 0 // counters reset (disk re-attached)
	}
	return b - a
}

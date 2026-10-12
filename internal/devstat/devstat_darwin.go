//go:build darwin

package devstat

/*
#cgo LDFLAGS: -framework IOKit -framework CoreFoundation
#include <stdint.h>
#include <stdlib.h>
#include <string.h>
#include <CoreFoundation/CoreFoundation.h>
#include <IOKit/IOKitLib.h>
#include <IOKit/IOBSD.h>

typedef struct {
	int64_t bytesR, bytesW, opsR, opsW, timeR, timeW, errR, errW, retR, retW;
} tc_stats;

static int64_t tc_num(CFDictionaryRef d, const char *key) {
	CFStringRef k = CFStringCreateWithCString(NULL, key, kCFStringEncodingUTF8);
	const void *v = CFDictionaryGetValue(d, k);
	CFRelease(k);
	int64_t n = 0;
	if (v && CFGetTypeID(v) == CFNumberGetTypeID())
		CFNumberGetValue((CFNumberRef)v, kCFNumberSInt64Type, &n);
	return n;
}

static void tc_str(CFDictionaryRef d, const char *key, char *out, int outLen) {
	CFStringRef k = CFStringCreateWithCString(NULL, key, kCFStringEncodingUTF8);
	const void *v = d ? CFDictionaryGetValue(d, k) : NULL;
	CFRelease(k);
	if (v && CFGetTypeID(v) == CFStringGetTypeID())
		CFStringGetCString((CFStringRef)v, out, outLen, kCFStringEncodingUTF8);
}

static CFTypeRef tc_prop(io_object_t o, const char *key) {
	CFStringRef k = CFStringCreateWithCString(NULL, key, kCFStringEncodingUTF8);
	CFTypeRef v = IORegistryEntryCreateCFProperty(o, k, kCFAllocatorDefault, 0);
	CFRelease(k);
	return v;
}

int tc_read_stats(io_object_t drv, tc_stats *s) {
	CFTypeRef p = tc_prop(drv, "Statistics");
	if (!p) return -1;
	if (CFGetTypeID(p) != CFDictionaryGetTypeID()) { CFRelease(p); return -1; }
	CFDictionaryRef d = (CFDictionaryRef)p;
	s->bytesR = tc_num(d, "Bytes (Read)");
	s->bytesW = tc_num(d, "Bytes (Write)");
	s->opsR   = tc_num(d, "Operations (Read)");
	s->opsW   = tc_num(d, "Operations (Write)");
	s->timeR  = tc_num(d, "Total Time (Read)");
	s->timeW  = tc_num(d, "Total Time (Write)");
	s->errR   = tc_num(d, "Errors (Read)");
	s->errW   = tc_num(d, "Errors (Write)");
	s->retR   = tc_num(d, "Retries (Read)");
	s->retW   = tc_num(d, "Retries (Write)");
	CFRelease(p);
	return 0;
}

// tc_find_driver walks up the IOService plane from the media named bsd (for
// example disk3s1s1, an APFS volume) to the IOBlockStorageDriver of the
// physical disk under it, filling in the whole disk's name and what IOKit
// knows about the device. Returns the driver (caller releases) or 0.
io_object_t tc_find_driver(const char *bsd, char *whole, char *model, char *link, char *medium, int n) {
	CFMutableDictionaryRef m = IOBSDNameMatching(kIOMainPortDefault, 0, bsd);
	if (!m) return 0;
	io_object_t e = IOServiceGetMatchingService(kIOMainPortDefault, m);
	while (e) {
		if (IOObjectConformsTo(e, "IOMedia")) {
			CFTypeRef w = tc_prop(e, "Whole");
			if (w && CFGetTypeID(w) == CFBooleanGetTypeID() && CFBooleanGetValue((CFBooleanRef)w)) {
				CFTypeRef name = tc_prop(e, "BSD Name");
				if (name && CFGetTypeID(name) == CFStringGetTypeID())
					CFStringGetCString((CFStringRef)name, whole, n, kCFStringEncodingUTF8);
				if (name) CFRelease(name);
			}
			if (w) CFRelease(w);
		}
		if (IOObjectConformsTo(e, "IOBlockStorageDriver")) {
			io_object_t dev = 0;
			if (IORegistryEntryGetParentEntry(e, kIOServicePlane, &dev) == KERN_SUCCESS) {
				CFTypeRef dc = tc_prop(dev, "Device Characteristics");
				if (dc && CFGetTypeID(dc) == CFDictionaryGetTypeID()) {
					tc_str((CFDictionaryRef)dc, "Product Name", model, n);
					tc_str((CFDictionaryRef)dc, "Medium Type", medium, n);
				}
				if (dc) CFRelease(dc);
				CFTypeRef pc = tc_prop(dev, "Protocol Characteristics");
				if (pc && CFGetTypeID(pc) == CFDictionaryGetTypeID())
					tc_str((CFDictionaryRef)pc, "Physical Interconnect", link, n);
				if (pc) CFRelease(pc);
				IOObjectRelease(dev);
			}
			return e;
		}
		io_object_t parent = 0;
		kern_return_t kr = IORegistryEntryGetParentEntry(e, kIOServicePlane, &parent);
		IOObjectRelease(e);
		if (kr != KERN_SUCCESS) return 0;
		e = parent;
	}
	return 0;
}

void tc_release(io_object_t o) { IOObjectRelease(o); }
*/
import "C"

import (
	"bytes"
	"fmt"
	"strings"
	"syscall"
	"time"
	"unsafe"
)

type ioDisk struct {
	drv  C.io_object_t
	info Info
}

// ForPath finds the physical disk that holds path. It reads only that disk's
// counters; it never opens the disk or lists other disks.
func ForPath(path string) (Source, error) {
	var fs syscall.Statfs_t
	if err := syscall.Statfs(path, &fs); err != nil {
		return nil, err
	}
	from := cstr(fs.Mntfromname[:])
	if !strings.HasPrefix(from, "/dev/disk") {
		return nil, fmt.Errorf("not on a local disk (%s)", cstr(fs.Fstypename[:]))
	}
	bsd := C.CString(strings.TrimPrefix(from, "/dev/"))
	defer C.free(unsafe.Pointer(bsd))
	const n = 256
	buf := make([]byte, 4*n)
	p := (*C.char)(unsafe.Pointer(&buf[0]))
	whole := p
	model := (*C.char)(unsafe.Pointer(&buf[n]))
	link := (*C.char)(unsafe.Pointer(&buf[2*n]))
	medium := (*C.char)(unsafe.Pointer(&buf[3*n]))
	drv := C.tc_find_driver(bsd, whole, model, link, medium, n)
	if drv == 0 {
		return nil, fmt.Errorf("no block storage driver above %s", from)
	}
	d := &ioDisk{drv: drv, info: Info{
		Name:         cbytes(buf[:n]),
		Model:        strings.TrimSpace(cbytes(buf[n : 2*n])),
		Interconnect: cbytes(buf[2*n : 3*n]),
		Medium:       cbytes(buf[3*n:]),
	}}
	if d.info.Name == "" {
		d.info.Name = strings.TrimPrefix(from, "/dev/")
	}
	return d, nil
}

func (d *ioDisk) Info() Info { return d.info }

func (d *ioDisk) Stats() (Stats, error) {
	var s C.tc_stats
	if C.tc_read_stats(d.drv, &s) != 0 {
		return Stats{}, fmt.Errorf("%s: no statistics", d.info.Name)
	}
	return Stats{
		ReadBytes: uint64(s.bytesR), WriteBytes: uint64(s.bytesW),
		ReadOps: uint64(s.opsR), WriteOps: uint64(s.opsW),
		ReadTime: time.Duration(s.timeR), WriteTime: time.Duration(s.timeW),
		ReadErrors: uint64(s.errR), WriteErrors: uint64(s.errW),
		ReadRetries: uint64(s.retR), WriteRetries: uint64(s.retW),
	}, nil
}

func (d *ioDisk) Close() {
	if d.drv != 0 {
		C.tc_release(d.drv)
		d.drv = 0
	}
}

func cstr(b []int8) string {
	u := make([]byte, 0, len(b))
	for _, c := range b {
		if c == 0 {
			break
		}
		u = append(u, byte(c))
	}
	return string(u)
}

func cbytes(b []byte) string {
	if i := bytes.IndexByte(b, 0); i >= 0 {
		b = b[:i]
	}
	return string(b)
}

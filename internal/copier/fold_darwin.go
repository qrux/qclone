//go:build darwin

package copier

/*
#include <stdlib.h>
#include <unistd.h>
*/
import "C"

import "unsafe"

// caseSensitive asks the volume holding path whether names differ by case
// (APFS and HFS+ usually say no). Unknown counts as no: folding only makes
// duplicate detection stricter.
func caseSensitive(path string) bool {
	p := C.CString(path)
	defer C.free(unsafe.Pointer(p))
	return C.pathconf(p, C._PC_CASE_SENSITIVE) == 1
}

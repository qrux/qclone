//go:build darwin

package copier

/*
#include <stdlib.h>
#include <sys/clonefile.h>
*/
import "C"

import "unsafe"

// cloneFile makes dst a copy of src: an APFS clone when the volume can (no
// data copied, blocks shared until changed), a plain copy otherwise.
func cloneFile(src, dst string) error {
	cs, cd := C.CString(src), C.CString(dst)
	defer C.free(unsafe.Pointer(cs))
	defer C.free(unsafe.Pointer(cd))
	if r, _ := C.clonefile(cs, cd, C.CLONE_NOFOLLOW); r == 0 {
		return nil
	}
	return copyPlain(src, dst)
}

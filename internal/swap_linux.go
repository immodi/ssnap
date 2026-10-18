//go:build linux && amd64

package ssnap

import (
	"syscall"
	"unsafe"
)

// sysRenameat2 is the renameat2(2) syscall number on linux/amd64.
const sysRenameat2 = 316

// atFDCWD instructs renameat2 to resolve paths relative to the current
// working directory. It is kept as a variable so it can be converted to
// uintptr despite being negative.
var atFDCWD = -100

// RENAME_EXCHANGE tells renameat2 to swap the two paths atomically.
const renameExchange = 2

// exchangeDirsImpl atomically swaps the directories a and b using renameat2.
// It returns errNoExchange when the kernel or filesystem does not support it,
// so callers can fall back to a sequence of renames.
func exchangeDirsImpl(a, b string) error {
	pa, err := syscall.BytePtrFromString(a)
	if err != nil {
		return err
	}
	pb, err := syscall.BytePtrFromString(b)
	if err != nil {
		return err
	}
	_, _, errno := syscall.Syscall6(
		sysRenameat2,
		uintptr(atFDCWD), uintptr(unsafe.Pointer(pa)),
		uintptr(atFDCWD), uintptr(unsafe.Pointer(pb)),
		uintptr(renameExchange), 0,
	)
	if errno != 0 {
		switch errno {
		case syscall.ENOSYS, syscall.EINVAL, syscall.EOPNOTSUPP:
			return errNoExchange
		}
		return errno
	}
	return nil
}

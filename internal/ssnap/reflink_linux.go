//go:build linux

package ssnap

import (
	"os"
	"syscall"
)

// ficlone is the FICLONE ioctl, defined as _IOW(0x94, 9, int) = 0x40049409.
// It asks the kernel to copy-on-write clone the source file into the
// destination file, which is near-instant and shares no data with the
// original once either side is modified.
const ficlone = 0x40049409

// cloneFile attempts a reflink from src into dst. A nil error means the clone
// succeeded; any other value means the caller must fall back to a byte copy.
func cloneFile(dst, src *os.File) error {
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, dst.Fd(), uintptr(ficlone), src.Fd())
	if errno != 0 {
		return errno
	}
	return nil
}

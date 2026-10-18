//go:build linux

package ssnap

import "syscall"

// statfsFree returns the free space, in bytes, available on the filesystem
// that holds path.
func statfsFree(path string) (uint64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, err
	}
	return uint64(st.Bavail) * uint64(st.Bsize), nil
}

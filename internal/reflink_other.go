//go:build !linux

package ssnap

import "os"

// cloneFile reports that reflinks are unavailable, so copyFile falls back to a
// byte-for-byte copy.
func cloneFile(dst, src *os.File) error {
	return errReflinkUnsupported
}

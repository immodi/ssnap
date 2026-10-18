//go:build !linux

package ssnap

import "math"

// statfsFree cannot be determined portably, so it reports effectively
// unlimited space and the guard never fires.
func statfsFree(path string) (uint64, error) {
	return math.MaxUint64, nil
}

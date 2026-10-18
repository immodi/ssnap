//go:build !(linux && amd64)

package ssnap

// exchangeDirsImpl is unavailable on this platform, so callers use the rename
// fallback instead.
func exchangeDirsImpl(a, b string) error {
	return errNoExchange
}

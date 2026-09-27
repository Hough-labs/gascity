//go:build windows

package fsys

// FreeBytes returns [ErrFreeBytesUnsupported]: there is no statfs on
// Windows, so callers treat free space as unknown.
func FreeBytes(_ string) (int64, error) {
	return -1, ErrFreeBytesUnsupported
}

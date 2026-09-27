//go:build !windows

package fsys

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// FreeBytes returns the bytes available to an unprivileged process in the
// filesystem containing path. Uses f_bavail×f_bsize.
//
// On APFS, f_bavail excludes purgeable space and reflects actual write
// capacity. Do NOT use f_bfree, os.Stat().Size(), or the Finder "available"
// figure — all three include purgeable space and overstate free capacity on
// APFS-formatted volumes.
func FreeBytes(path string) (int64, error) {
	var stat unix.Statfs_t
	if err := unix.Statfs(path, &stat); err != nil {
		return -1, fmt.Errorf("statfs %q: %w", path, err)
	}
	return int64(stat.Bavail * uint64(stat.Bsize)), nil
}

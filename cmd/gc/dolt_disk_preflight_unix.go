//go:build !windows

package main

import "github.com/gastownhall/gascity/internal/fsys"

// doltContainerFreeBytesFunc is the injectable disk-space reader used by
// checkManagedDoltDiskPreflight. Tests replace this with a fake; production
// uses containerFreeBytes.
var doltContainerFreeBytesFunc = containerFreeBytes

// containerFreeBytes returns the bytes available to an unprivileged process
// in the filesystem containing path. See fsys.FreeBytes for why this reads
// f_bavail rather than a figure that counts APFS purgeable space.
func containerFreeBytes(path string) (int64, error) {
	return fsys.FreeBytes(path)
}

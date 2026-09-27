//go:build !windows

package fsys

import (
	"errors"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"
)

func TestFreeBytesReportsSpaceOnExistingDir(t *testing.T) {
	n, err := FreeBytes(t.TempDir())
	if err != nil {
		t.Fatalf("FreeBytes: %v", err)
	}
	if n <= 0 {
		t.Fatalf("FreeBytes = %d, want > 0", n)
	}
}

func TestFreeBytesMissingPathReturnsNotExist(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "absent")
	n, err := FreeBytes(missing)
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("FreeBytes(%q) error = %v, want fs.ErrNotExist", missing, err)
	}
	if !strings.Contains(err.Error(), missing) {
		t.Errorf("error %q should name the path %q", err, missing)
	}
	if n != -1 {
		t.Errorf("FreeBytes = %d on error, want -1", n)
	}
}

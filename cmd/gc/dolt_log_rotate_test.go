package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeDoltLogFixture writes a dolt.log of exactly size bytes whose first
// bytes are marker, so a test can tell the generations apart after a shift.
func writeDoltLogFixture(t *testing.T, path, marker string, size int) {
	t.Helper()
	body := marker
	if len(body) < size {
		body += strings.Repeat("x", size-len(body))
	}
	if err := os.WriteFile(path, []byte(body[:size]), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func readDoltLogFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}

func requireDoltLogAbsent(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("expected %s to be absent, stat err = %v", path, err)
	}
}

func TestManagedDoltLogRotateUnderBoundLeavesLogAlone(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "dolt.log")
	writeDoltLogFixture(t, path, "live", 63)

	if err := rotateManagedDoltLog(path, 64, 3); err != nil {
		t.Fatalf("rotateManagedDoltLog: %v", err)
	}

	if got := len(readDoltLogFile(t, path)); got != 63 {
		t.Errorf("log size = %d, want the untouched 63", got)
	}
	requireDoltLogAbsent(t, path+".1")
}

func TestManagedDoltLogRotateAtBoundRotates(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "dolt.log")
	writeDoltLogFixture(t, path, "live", 64)

	if err := rotateManagedDoltLog(path, 64, 3); err != nil {
		t.Fatalf("rotateManagedDoltLog: %v", err)
	}

	// The live path is renamed away, not truncated in place: the next
	// opener creates it fresh.
	requireDoltLogAbsent(t, path)
	if got := readDoltLogFile(t, path+".1"); !strings.HasPrefix(got, "live") || len(got) != 64 {
		t.Errorf("dolt.log.1 = %d bytes starting %q, want the whole 64-byte log", len(got), got[:4])
	}
}

func TestManagedDoltLogRotateHonorsRetainedGenerations(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "dolt.log")
	const keep = 3

	// Five rotations against a three-generation window: the oldest two
	// must fall off rather than accumulate.
	markers := []string{"gen1", "gen2", "gen3", "gen4", "gen5"}
	for _, marker := range markers {
		writeDoltLogFixture(t, path, marker, 64)
		if err := rotateManagedDoltLog(path, 64, keep); err != nil {
			t.Fatalf("rotateManagedDoltLog(%s): %v", marker, err)
		}
	}

	// Newest rotation is .1 and the window walks backwards from there.
	want := map[string]string{
		path + ".1": "gen5",
		path + ".2": "gen4",
		path + ".3": "gen3",
	}
	for generation, marker := range want {
		if got := readDoltLogFile(t, generation); !strings.HasPrefix(got, marker) {
			t.Errorf("%s starts %q, want %q", generation, got[:4], marker)
		}
	}
	requireDoltLogAbsent(t, path+".4")
	requireDoltLogAbsent(t, path+".5")
}

func TestManagedDoltLogRotatePrunesGenerationsBeyondRetention(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "dolt.log")
	writeDoltLogFixture(t, path, "live", 64)
	// Generations left behind by a previously larger retention setting,
	// including a gap where one was deleted by hand.
	for _, gen := range []string{".1", ".2", ".4", ".5", ".6"} {
		writeDoltLogFixture(t, path+gen, "old"+gen, 8)
	}
	// A neighbor that merely shares the prefix must survive: pruning keys
	// on a positive decimal suffix, not on the prefix alone.
	neighbor := path + ".gz"
	writeDoltLogFixture(t, neighbor, "keep", 8)

	if err := rotateManagedDoltLog(path, 64, 2); err != nil {
		t.Fatalf("rotateManagedDoltLog: %v", err)
	}

	if got := readDoltLogFile(t, path+".1"); !strings.HasPrefix(got, "live") {
		t.Errorf("dolt.log.1 starts %q, want the rotated live log", got[:4])
	}
	if got := readDoltLogFile(t, path+".2"); !strings.HasPrefix(got, "old.1") {
		t.Errorf("dolt.log.2 starts %q, want the shifted old .1", got[:5])
	}
	for _, gen := range []string{".3", ".4", ".5", ".6"} {
		requireDoltLogAbsent(t, path+gen)
	}
	if got := readDoltLogFile(t, neighbor); !strings.HasPrefix(got, "keep") {
		t.Errorf("dolt.log.gz starts %q, want to be left alone", got[:4])
	}
}

func TestManagedDoltLogRotateDisabledAndMissingAreNoOps(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "dolt.log")
	writeDoltLogFixture(t, path, "live", 4096)

	for _, maxBytes := range []int64{0, -1} {
		if err := rotateManagedDoltLog(path, maxBytes, 3); err != nil {
			t.Fatalf("rotateManagedDoltLog(maxBytes=%d): %v", maxBytes, err)
		}
		if got := len(readDoltLogFile(t, path)); got != 4096 {
			t.Errorf("maxBytes=%d rotated a log it should have ignored (size %d)", maxBytes, got)
		}
		requireDoltLogAbsent(t, path+".1")
	}

	missing := filepath.Join(dir, "absent", "dolt.log")
	if err := rotateManagedDoltLog(missing, 1, 3); err != nil {
		t.Errorf("rotateManagedDoltLog on a missing log = %v, want no error", err)
	}
	if err := rotateManagedDoltLog("   ", 1, 3); err != nil {
		t.Errorf("rotateManagedDoltLog on a blank path = %v, want no error", err)
	}
}

// TestManagedDoltLogRotateKeepsLiveWriterHandleIntact is the constraint that
// shapes this whole mechanism: the managed log is held open across a server's
// whole life by the supervising watchdog, and the dolt child inherits that
// descriptor. Rotation must therefore never truncate the file or invalidate
// the handle — it renames, so a live holder keeps writing into the same inode
// under its new name and loses nothing, not even a line it was midway
// through.
func TestManagedDoltLogRotateKeepsLiveWriterHandleIntact(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "dolt.log")

	writer, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatalf("open live writer: %v", err)
	}
	defer writer.Close() //nolint:errcheck

	const complete = "complete line before rotation\n"
	if _, err := writer.WriteString(complete); err != nil {
		t.Fatalf("write before rotation: %v", err)
	}
	// Leave the writer mid-line, exactly as a supervising process can be
	// when a size bound is crossed.
	const partialHead = "line split across the "
	if _, err := writer.WriteString(partialHead); err != nil {
		t.Fatalf("write partial line: %v", err)
	}

	if err := rotateManagedDoltLog(path, int64(len(complete)), 3); err != nil {
		t.Fatalf("rotateManagedDoltLog: %v", err)
	}

	const partialTail = "rotation\n"
	if _, err := writer.WriteString(partialTail); err != nil {
		t.Fatalf("live writer handle broke across rotation: %v", err)
	}
	if err := writer.Sync(); err != nil {
		t.Fatalf("sync live writer after rotation: %v", err)
	}

	rotated := readDoltLogFile(t, path+".1")
	if want := complete + partialHead + partialTail; rotated != want {
		t.Errorf("rotated generation = %q, want %q (the handle must keep every byte, and the split line must land whole)", rotated, want)
	}
	// Nothing was written to the fresh path: the live holder followed its
	// inode, so rotation neither truncated the log nor split the in-flight
	// line across two files.
	requireDoltLogAbsent(t, path)
}

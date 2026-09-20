package t3bridge

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

// testRetryBackoff is the negligible inter-attempt wait these tests pass to
// latestSeqWithBackoff. It is an argument rather than a shrunk package var
// because the var was shared mutable state: writing it raced a leaked
// event-watcher goroutine still reading it (gascity-20h2).
const testRetryBackoff = time.Millisecond

func TestLatestSeqWithBackoffRetriesThenSucceeds(t *testing.T) {
	calls := 0
	seq, err := latestSeqWithBackoff(context.Background(), testRetryBackoff, func() (uint64, error) {
		calls++
		if calls < 3 {
			return 0, fmt.Errorf("transient hiccup %d", calls)
		}
		return 42, nil
	})
	if err != nil {
		t.Fatalf("latestSeqWithBackoff: %v", err)
	}
	if seq != 42 {
		t.Fatalf("seq = %d, want 42", seq)
	}
	if calls != 3 {
		t.Fatalf("LatestSeq calls = %d, want 3", calls)
	}
}

func TestLatestSeqWithBackoffHonorsContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	calls := 0
	_, err := latestSeqWithBackoff(ctx, testRetryBackoff, func() (uint64, error) {
		calls++
		return 0, errors.New("always fails")
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if calls == 0 {
		t.Fatal("expected at least one LatestSeq attempt before honoring cancel")
	}
}

func TestLatestSeqWithBackoffGivesUpAfterMaxAttempts(t *testing.T) {
	calls := 0
	_, err := latestSeqWithBackoff(context.Background(), testRetryBackoff, func() (uint64, error) {
		calls++
		return 0, fmt.Errorf("attempt %d", calls)
	})
	if err == nil {
		t.Fatal("expected an error after exhausting the attempt budget")
	}
	if calls != 5 {
		t.Fatalf("LatestSeq calls = %d, want 5 (maxAttempts)", calls)
	}
}

package api

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/testutil"
	"github.com/gastownhall/gascity/internal/worker"
)

type peekOnlyHandle struct {
	output string
}

func (h peekOnlyHandle) Peek(context.Context, int) (string, error) {
	return h.output, nil
}

func TestStreamSessionPeekAcceptsPeekCapability(t *testing.T) {
	srv := New(newSessionFakeState(t))
	info := session.Info{ID: "sess-1", Template: "probe", Provider: "claude"}
	// The streaming goroutine writes the body while this goroutine reads it, so
	// the recorder has to be the locked one; a bare httptest.NewRecorder races.
	rec := newSyncResponseRecorder()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		srv.streamSessionPeek(ctx, rec, info, peekOnlyHandle{output: "hello from peek"})
		close(done)
	}()
	defer func() {
		cancel()
		<-done
	}()

	body := waitForRecorderSubstring(t, rec, "hello from peek", testutil.GoroutineRaceTimeout)
	if !strings.Contains(body, "hello from peek") {
		t.Fatalf("stream body missing peek output: %s", body)
	}
	if !strings.Contains(body, `"provider":"claude"`) {
		t.Fatalf("stream body missing provider envelope: %s", body)
	}
}

type peekPendingHandle struct {
	mu      sync.Mutex
	output  string
	pending *worker.PendingInteraction
}

func (h *peekPendingHandle) Peek(context.Context, int) (string, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.output, nil
}

func (h *peekPendingHandle) Pending(context.Context) (*worker.PendingInteraction, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.pending == nil {
		return nil, nil
	}
	copyPending := *h.pending
	copyPending.Options = append([]string(nil), h.pending.Options...)
	copyPending.Metadata = cloneStringMap(h.pending.Metadata)
	return &copyPending, nil
}

func (h *peekPendingHandle) PendingStatus(ctx context.Context) (*worker.PendingInteraction, bool, error) {
	pending, err := h.Pending(ctx)
	return pending, true, err
}

func (h *peekPendingHandle) Respond(context.Context, worker.InteractionResponse) error {
	return nil
}

func (h *peekPendingHandle) SetPending(pending *worker.PendingInteraction) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if pending == nil {
		h.pending = nil
		return
	}
	copyPending := *pending
	copyPending.Options = append([]string(nil), pending.Options...)
	copyPending.Metadata = cloneStringMap(pending.Metadata)
	h.pending = &copyPending
}

func TestStreamSessionPeekRawWorkerWakeEmitsPendingWithoutOutputChange(t *testing.T) {
	fs := newSessionFakeState(t)
	srv := New(fs)
	info := session.Info{ID: "sess-1", Template: "probe", Provider: "claude"}
	handle := &peekPendingHandle{output: "steady output"}
	rec := newSyncResponseRecorder()

	// Hang backstop only: the stream is torn down by the explicit cancel() below,
	// so this must outlast every testutil.GoroutineRaceTimeout wait that follows.
	ctx, cancel := context.WithTimeout(context.Background(), 3*testutil.GoroutineRaceTimeout)
	defer cancel()

	done := make(chan struct{})
	go func() {
		srv.streamSessionPeekRaw(ctx, rec, info, handle)
		close(done)
	}()

	if body := waitForRecorderSubstring(t, rec, "steady output", testutil.GoroutineRaceTimeout); !strings.Contains(body, "steady output") {
		t.Fatalf("stream body missing initial peek output: %s", body)
	} else if !strings.Contains(body, `"provider":"claude"`) {
		t.Fatalf("stream body missing provider envelope: %s", body)
	}

	handle.SetPending(&worker.PendingInteraction{
		RequestID: "req-1",
		Kind:      "approval",
		Prompt:    "Proceed?",
	})
	fs.eventProv.(*events.Fake).Record(events.Event{
		Type:    events.WorkerOperation,
		Actor:   "worker",
		Subject: info.ID,
	})

	body := waitForRecorderSubstring(t, rec, "req-1", testutil.GoroutineRaceTimeout)

	cancel()
	<-done

	if !strings.Contains(body, "req-1") {
		t.Fatalf("raw stream body missing pending interaction after worker wake: %s", body)
	}
}

var _ worker.PeekHandle = peekOnlyHandle{}

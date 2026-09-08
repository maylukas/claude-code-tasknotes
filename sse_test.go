package main

import (
	"bufio"
	"net/http"
	"strings"
	"testing"
	"time"
)

// readSSEEvent reads from an SSE response body until it sees a
// non-comment, non-blank line (i.e. skips heartbeat comment lines, which
// start with ':') or the deadline elapses.
func readSSEEvent(t *testing.T, body *bufio.Reader, deadline time.Duration) string {
	t.Helper()
	done := make(chan string, 1)
	go func() {
		for {
			line, err := body.ReadString('\n')
			if err != nil {
				done <- ""
				return
			}
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, ":") {
				continue
			}
			done <- line
			return
		}
	}()
	select {
	case line := <-done:
		return line
	case <-time.After(deadline):
		t.Fatal("timed out waiting for an SSE event")
		return ""
	}
}

// TestHandleEvents_MutationPushesAnEvent verifies the core SSE contract:
// a mutation elsewhere in the daemon (via triggerRenders, the same choke
// point the dashboard/session-page writers already use) results in a
// pushed event on an open GET /events connection — the client's signal to
// re-fetch GET /status.
func TestHandleEvents_MutationPushesAnEvent(t *testing.T) {
	_, ts := newTestServer(t)

	req, err := http.NewRequest(http.MethodGet, ts.URL+"/events", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("expected Content-Type text/event-stream, got %q", ct)
	}

	reader := bufio.NewReader(resp.Body)

	// Give the handler a moment to actually subscribe before mutating —
	// otherwise there's a race where the mutation's notify() fires before
	// this connection's subscribe() has registered.
	time.Sleep(20 * time.Millisecond)

	// Trigger a real mutation through the normal path (register calls
	// triggerRenders internally via saveLocked+triggerRenders).
	register(t, ts, "orchestrator-myapp-g1", "myapp")

	line := readSSEEvent(t, reader, 2*time.Second)
	if !strings.HasPrefix(line, "data:") {
		t.Fatalf("expected a data: line, got %q", line)
	}
	if !strings.Contains(line, `"type":"status"`) {
		t.Errorf(`expected the event to carry {"type":"status"}, got %q`, line)
	}
}

// TestHandleEvents_NoMutationNoEvent verifies an idle connection with no
// mutation elsewhere produces no event at all — the push is genuinely
// change-driven, not a disguised poll.
func TestHandleEvents_NoMutationNoEvent(t *testing.T) {
	_, ts := newTestServer(t)

	req, err := http.NewRequest(http.MethodGet, ts.URL+"/events", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	reader := bufio.NewReader(resp.Body)
	done := make(chan string, 1)
	go func() {
		line, _ := reader.ReadString('\n')
		done <- strings.TrimSpace(line)
	}()

	select {
	case line := <-done:
		if line != "" {
			t.Errorf("expected no line at all with no mutation, got %q", line)
		}
	case <-time.After(300 * time.Millisecond):
		// Correct: nothing arrived.
	}
}

// TestSSEHub_NotifyDoesNotBlockOnAFullSubscriber verifies a slow/stuck
// subscriber (its channel already full — hasn't consumed a previous
// notify) never blocks notify() for every other subscriber, matching the
// documented "drop a duplicate wake, never block" contract.
func TestSSEHub_NotifyDoesNotBlockOnAFullSubscriber(t *testing.T) {
	hub := newSSEHub()
	slow := hub.subscribe()
	slow <- struct{}{} // fill it, simulating a subscriber that hasn't read yet

	fast := hub.subscribe()

	done := make(chan struct{})
	go func() {
		hub.notify()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("notify() blocked on a full subscriber channel")
	}

	select {
	case <-fast:
	default:
		t.Error("expected the non-full subscriber to receive the notify")
	}
}

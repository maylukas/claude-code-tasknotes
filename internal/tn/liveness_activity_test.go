package tn

import (
	"net/http"
	"testing"
	"time"
)

// TestFixThree files verify the 2026-08-25 incident's root fix: LastSeenAt
// must advance on ANY authenticated, agent-initiated action, not just an
// inbox poll — an orchestrator doing real synchronous work (acking,
// declaring a worker, reporting context) goes dark on the old poll-only
// signal even though it is demonstrably alive. Each test backdates the
// agent's LastSeenAt, calls the endpoint, and asserts it moved forward.

func lastSeenAt(t *testing.T, srv *Server, name string) time.Time {
	t.Helper()
	srv.mu.Lock()
	defer srv.mu.Unlock()
	a, ok := srv.state.Agents[name]
	if !ok {
		t.Fatalf("agent %s not registered", name)
	}
	return a.LastSeenAt
}

func backdateLastSeen(srv *Server, name string, ago time.Duration) {
	srv.mu.Lock()
	defer srv.mu.Unlock()
	srv.state.Agents[name].LastSeenAt = time.Now().Add(-ago)
}

func TestHandleAck_BumpsLastSeenAt(t *testing.T) {
	srv, ts := newTestServer(t)
	register(t, ts, "agent-1", "proj-a")

	sendResp := mustPost(t, ts.URL+"/messages", map[string]any{
		"to": "agent-1", "text": "hello",
	})
	var sent struct {
		Message Message `json:"message"`
	}
	decodeInto(t, sendResp, &sent)
	sendResp.Body.Close()

	backdateLastSeen(srv, "agent-1", 80*time.Second) // still "alive" but stale-looking
	before := lastSeenAt(t, srv, "agent-1")

	ackResp := mustPost(t, ts.URL+"/ack", map[string]any{"name": "agent-1", "msgId": sent.Message.ID})
	ackResp.Body.Close()
	if ackResp.StatusCode != http.StatusOK {
		t.Fatalf("ack: expected 200, got %d", ackResp.StatusCode)
	}

	after := lastSeenAt(t, srv, "agent-1")
	if !after.After(before) {
		t.Errorf("expected LastSeenAt to advance on ack, before=%v after=%v", before, after)
	}
}

func TestHandleWorkerStart_BumpsLastSeenAt(t *testing.T) {
	srv, ts := newTestServer(t)
	register(t, ts, "orchestrator-myapp-g1", "myapp")
	backdateLastSeen(srv, "orchestrator-myapp-g1", 80*time.Second)
	before := lastSeenAt(t, srv, "orchestrator-myapp-g1")

	resp := mustPost(t, ts.URL+"/workers/start", map[string]any{"name": "orchestrator-myapp-g1", "taskPath": "Tasks/A.md"})
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("workers/start: expected 200, got %d", resp.StatusCode)
	}

	after := lastSeenAt(t, srv, "orchestrator-myapp-g1")
	if !after.After(before) {
		t.Errorf("expected LastSeenAt to advance on worker start (this is the exact action g84221 was doing when it was falsely declared dead), before=%v after=%v", before, after)
	}
}

func TestHandleWorkerEnd_BumpsLastSeenAt(t *testing.T) {
	srv, ts := newTestServer(t)
	register(t, ts, "orchestrator-myapp-g1", "myapp")
	mustPost(t, ts.URL+"/workers/start", map[string]any{"name": "orchestrator-myapp-g1", "taskPath": "Tasks/A.md"}).Body.Close()

	backdateLastSeen(srv, "orchestrator-myapp-g1", 80*time.Second)
	before := lastSeenAt(t, srv, "orchestrator-myapp-g1")

	resp := mustPost(t, ts.URL+"/workers/end", map[string]any{"name": "orchestrator-myapp-g1", "taskPath": "Tasks/A.md"})
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("workers/end: expected 200, got %d", resp.StatusCode)
	}

	after := lastSeenAt(t, srv, "orchestrator-myapp-g1")
	if !after.After(before) {
		t.Errorf("expected LastSeenAt to advance on worker end, before=%v after=%v", before, after)
	}
}

func TestHandleContext_BumpsLastSeenAt(t *testing.T) {
	srv, ts := newTestServer(t)
	register(t, ts, "orchestrator-myapp-g1", "myapp")
	backdateLastSeen(srv, "orchestrator-myapp-g1", 80*time.Second)
	before := lastSeenAt(t, srv, "orchestrator-myapp-g1")

	resp := mustPost(t, ts.URL+"/agents/context", map[string]any{"name": "orchestrator-myapp-g1", "usedPct": 22.0})
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("agents/context: expected 200, got %d", resp.StatusCode)
	}

	after := lastSeenAt(t, srv, "orchestrator-myapp-g1")
	if !after.After(before) {
		t.Errorf("expected LastSeenAt to advance on a context report, before=%v after=%v", before, after)
	}
}

// TestAppendActivityLocked_AlreadyBumpsLastSeenAt is a regression guard,
// not a new fix: `tn log` (handleActivity -> appendActivityLocked) already
// bumped LastSeenAt before this incident — confirming that here so the
// four tests above aren't mistaken for covering every liveness gap;
// logging was already covered, ack/worker-start/worker-end/context were
// the gap.
func TestAppendActivityLocked_AlreadyBumpsLastSeenAt(t *testing.T) {
	srv, ts := newTestServer(t)
	register(t, ts, "orchestrator-myapp-g1", "myapp")
	backdateLastSeen(srv, "orchestrator-myapp-g1", 80*time.Second)
	before := lastSeenAt(t, srv, "orchestrator-myapp-g1")

	resp := mustPost(t, ts.URL+"/activity", map[string]any{"name": "orchestrator-myapp-g1", "text": "did a thing"})
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("activity: expected 200, got %d", resp.StatusCode)
	}

	after := lastSeenAt(t, srv, "orchestrator-myapp-g1")
	if !after.After(before) {
		t.Errorf("expected LastSeenAt to advance on tn log, before=%v after=%v", before, after)
	}
}

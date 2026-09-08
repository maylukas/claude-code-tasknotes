package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// newTestServer builds a Server backed by a temp-dir state file and a
// no-op spawnFunc, wired into an httptest.Server.
func newTestServer(t *testing.T) (*Server, *httptest.Server) {
	t.Helper()
	dir := t.TempDir()
	cfg := ServeConfig{Port: 0, Projects: map[string]ProjectConfig{}}
	srv := newServer(filepath.Join(dir, "state.json"), cfg, func(project, cwd string, env map[string]string) error { return nil })
	ts := httptest.NewServer(newMux(srv))
	t.Cleanup(ts.Close)
	return srv, ts
}

func newTestServerWithConfig(t *testing.T, cfg ServeConfig, spawnFunc func(project, cwd string, env map[string]string) error) *httptest.Server {
	t.Helper()
	dir := t.TempDir()
	srv := newServer(filepath.Join(dir, "state.json"), cfg, spawnFunc)
	ts := httptest.NewServer(newMux(srv))
	t.Cleanup(ts.Close)
	return ts
}

func mustPost(t *testing.T, url string, body any) *http.Response {
	t.Helper()
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.Post(url, "application/json", bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func register(t *testing.T, ts *httptest.Server, name, project string) {
	t.Helper()
	resp := mustPost(t, ts.URL+"/agents/register", map[string]any{"name": name, "project": project})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("register %s: expected 200, got %d", name, resp.StatusCode)
	}
}

func decodeInto(t *testing.T, resp *http.Response, v any) {
	t.Helper()
	defer resp.Body.Close()
	if err := json.NewDecoder(resp.Body).Decode(v); err != nil {
		t.Fatalf("decode response: %v", err)
	}
}

// TestRegisterAlive verifies a freshly registered agent shows up as alive
// via GET /agents.
func TestRegisterAlive(t *testing.T) {
	_, ts := newTestServer(t)
	register(t, ts, "agent-1", "proj-a")

	resp, err := http.Get(ts.URL + "/agents")
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		Agents []agentView `json:"agents"`
	}
	decodeInto(t, resp, &out)

	if len(out.Agents) != 1 {
		t.Fatalf("expected 1 agent, got %+v", out.Agents)
	}
	if out.Agents[0].Name != "agent-1" || out.Agents[0].Project != "proj-a" {
		t.Errorf("unexpected agent: %+v", out.Agents[0])
	}
	if !out.Agents[0].Alive {
		t.Errorf("expected agent to be alive, got %+v", out.Agents[0])
	}
}

// getAgent fetches the current /agents view for name, failing the test if
// it isn't found.
func getAgent(t *testing.T, ts *httptest.Server, name string) agentView {
	t.Helper()
	resp, err := http.Get(ts.URL + "/agents")
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		Agents []agentView `json:"agents"`
	}
	decodeInto(t, resp, &out)
	for _, a := range out.Agents {
		if a.Name == name {
			return a
		}
	}
	t.Fatalf("agent %q not found in %+v", name, out.Agents)
	return agentView{}
}

// TestRegisterSetsAccepting_DrainFlipsIt verifies the generations & drain
// lifecycle (SPEC-generations.md): register always starts (or resumes)
// Accepting=true, POST /agents/drain flips it to false idempotently, and a
// subsequent re-register resumes accepting again.
func TestRegisterSetsAccepting_DrainFlipsIt(t *testing.T) {
	_, ts := newTestServer(t)
	register(t, ts, "orchestrator-myapp-g1", "myapp")

	if a := getAgent(t, ts, "orchestrator-myapp-g1"); !a.Accepting {
		t.Fatalf("expected Accepting=true right after register, got %+v", a)
	}

	resp := mustPost(t, ts.URL+"/agents/drain", map[string]any{"name": "orchestrator-myapp-g1"})
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 from drain, got %d", resp.StatusCode)
	}
	if a := getAgent(t, ts, "orchestrator-myapp-g1"); a.Accepting {
		t.Fatalf("expected Accepting=false after drain, got %+v", a)
	}

	// Idempotent: draining an already-draining agent is not an error.
	resp2 := mustPost(t, ts.URL+"/agents/drain", map[string]any{"name": "orchestrator-myapp-g1"})
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 from a repeat drain, got %d", resp2.StatusCode)
	}

	// Re-register resumes accepting.
	register(t, ts, "orchestrator-myapp-g1", "myapp")
	if a := getAgent(t, ts, "orchestrator-myapp-g1"); !a.Accepting {
		t.Fatalf("expected Accepting=true again after re-register, got %+v", a)
	}
}

// TestRegisterStoresTmuxSession verifies `tn register --tmux-session S`
// (the register HTTP body's tmuxSession field) is stored on the agent and
// visible via GET /agents.
func TestRegisterStoresTmuxSession(t *testing.T) {
	_, ts := newTestServer(t)
	resp := mustPost(t, ts.URL+"/agents/register", map[string]any{
		"name": "orchestrator-myapp-g42", "project": "myapp", "tmuxSession": "tn-myapp-g42",
	})
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	a := getAgent(t, ts, "orchestrator-myapp-g42")
	if a.TmuxSession != "tn-myapp-g42" {
		t.Errorf("expected TmuxSession %q, got %q", "tn-myapp-g42", a.TmuxSession)
	}
}

// TestRegisterRetargetsLogicalQueue verifies that when nothing was alive to
// receive an assignment, a message queued to the logical name
// orchestrator-<slug> gets re-targeted to whichever concrete generation
// registers next for that project — otherwise it would strand forever
// behind a name no generation actually uses.
func TestRegisterRetargetsLogicalQueue(t *testing.T) {
	srv, ts := newTestServer(t)

	// Nothing alive for myapp: a message queued straight to the logical name.
	srv.mu.Lock()
	srv.state.Messages = append(srv.state.Messages,
		&Message{ID: "m1", To: "orchestrator-myapp", Status: "queued", Text: "Task assigned: Foo (Tasks/Foo.md)", CreatedAt: time.Now()},
		&Message{ID: "m2", To: "orchestrator-myapp", Status: "queued", Text: "@claude: are you there", CreatedAt: time.Now()},
		&Message{ID: "m3", To: "orchestrator-other", Status: "queued", Text: "unrelated", CreatedAt: time.Now()},
	)
	srv.mu.Unlock()

	register(t, ts, "orchestrator-myapp-g7", "myapp")

	srv.mu.Lock()
	defer srv.mu.Unlock()
	var m1, m2, m3 *Message
	for _, m := range srv.state.Messages {
		switch m.ID {
		case "m1":
			m1 = m
		case "m2":
			m2 = m
		case "m3":
			m3 = m
		}
	}
	if m1.To != "orchestrator-myapp-g7" || m2.To != "orchestrator-myapp-g7" {
		t.Errorf("expected both myapp logical-queue messages re-targeted to orchestrator-myapp-g7, got m1.To=%q m2.To=%q", m1.To, m2.To)
	}
	if m3.To != "orchestrator-other" {
		t.Errorf("expected an unrelated project's queued message untouched, got %q", m3.To)
	}
}

// TestHandleContext_StoresContextPct verifies POST /agents/context stores
// the reported percentage and is visible via GET /agents.
func TestHandleContext_StoresContextPct(t *testing.T) {
	_, ts := newTestServer(t)
	register(t, ts, "orchestrator-myapp-g1", "myapp")

	resp := mustPost(t, ts.URL+"/agents/context", map[string]any{"name": "orchestrator-myapp-g1", "usedPct": 23.0})
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	a := getAgent(t, ts, "orchestrator-myapp-g1")
	if a.ContextPct == nil || *a.ContextPct != 23.0 {
		t.Errorf("expected ContextPct 23, got %+v", a.ContextPct)
	}
}

// TestHandleContext_UnknownAgent404 verifies reporting context for an agent
// that was never registered fails rather than silently creating one.
func TestHandleContext_UnknownAgent404(t *testing.T) {
	_, ts := newTestServer(t)
	resp := mustPost(t, ts.URL+"/agents/context", map[string]any{"name": "ghost", "usedPct": 50.0})
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("expected 404, got %d", resp.StatusCode)
	}
}

// TestHandleContext_BelowThresholdNoOp verifies a report under
// drainContextPct never flips Accepting.
func TestHandleContext_BelowThresholdNoOp(t *testing.T) {
	_, ts := newTestServer(t)
	register(t, ts, "orchestrator-myapp-g1", "myapp")

	resp := mustPost(t, ts.URL+"/agents/context", map[string]any{"name": "orchestrator-myapp-g1", "usedPct": 39.0})
	resp.Body.Close()

	if a := getAgent(t, ts, "orchestrator-myapp-g1"); !a.Accepting {
		t.Errorf("expected Accepting to stay true below drainContextPct, got %+v", a)
	}
}

// TestHandleContext_ThresholdAutoDrainsOnce verifies crossing
// drainContextPct flips Accepting to false, queues the informational
// drain message exactly once, and a repeat high report doesn't re-queue it
// (one-shot per SPEC-generations.md).
func TestHandleContext_ThresholdAutoDrainsOnce(t *testing.T) {
	srv, ts := newTestServer(t)
	register(t, ts, "orchestrator-myapp-g1", "myapp")

	resp1 := mustPost(t, ts.URL+"/agents/context", map[string]any{"name": "orchestrator-myapp-g1", "usedPct": 41.0})
	resp1.Body.Close()

	a := getAgent(t, ts, "orchestrator-myapp-g1")
	if a.Accepting {
		t.Fatalf("expected Accepting=false after crossing drainContextPct, got %+v", a)
	}

	msgs := queuedMessagesTo(srv, "orchestrator-myapp-g1")
	if len(msgs) != 1 {
		t.Fatalf("expected exactly 1 informational drain message queued, got %+v", msgs)
	}
	if !strings.Contains(msgs[0].Text, "draining") || !strings.Contains(msgs[0].Text, "tn retire --name orchestrator-myapp-g1") {
		t.Errorf("expected the message to explain draining and mention tn retire, got %q", msgs[0].Text)
	}

	// A second high report must not queue a second message.
	resp2 := mustPost(t, ts.URL+"/agents/context", map[string]any{"name": "orchestrator-myapp-g1", "usedPct": 55.0})
	resp2.Body.Close()

	if msgs := queuedMessagesTo(srv, "orchestrator-myapp-g1"); len(msgs) != 1 {
		t.Errorf("expected still exactly 1 drain message after a second high report, got %d", len(msgs))
	}
}

// TestHandleRetire_UnknownAgent404 verifies retiring an agent that was
// never registered fails.
func TestHandleRetire_UnknownAgent404(t *testing.T) {
	_, ts := newTestServer(t)
	resp := mustPost(t, ts.URL+"/agents/retire", map[string]any{"name": "ghost"})
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("expected 404, got %d", resp.StatusCode)
	}
}

// TestHandleRetire_PendingMessagesConflict verifies retire refuses (409)
// when the agent still has an unacked (queued or delivered) message, and
// leaves it registered — it must ack everything first.
func TestHandleRetire_PendingMessagesConflict(t *testing.T) {
	srv, ts := newTestServer(t)
	register(t, ts, "orchestrator-myapp-g1", "myapp")

	srv.mu.Lock()
	srv.state.Messages = append(srv.state.Messages, &Message{
		ID: "m1", To: "orchestrator-myapp-g1", Status: "queued", CreatedAt: time.Now(),
	})
	srv.mu.Unlock()

	resp := mustPost(t, ts.URL+"/agents/retire", map[string]any{"name": "orchestrator-myapp-g1"})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("expected 409, got %d", resp.StatusCode)
	}
	var out struct {
		Error string `json:"error"`
	}
	decodeInto(t, resp, &out)
	if out.Error != "pending messages" {
		t.Errorf(`expected error "pending messages", got %q`, out.Error)
	}

	if a := getAgent(t, ts, "orchestrator-myapp-g1"); a.DeregisteredAt != nil {
		t.Errorf("expected the agent to remain registered after a refused retire, got %+v", a)
	}
}

// TestHandleRetire_CleanRetire verifies a retire with no pending messages
// marks the agent deregistered immediately (before any kill happens) and,
// once finishRetire runs (called directly here, bypassing the real
// retireKillDelay sleep), kills the agent's stored tmux session and
// appends an activity entry.
func TestHandleRetire_CleanRetire(t *testing.T) {
	srv, ts := newTestServer(t)
	resp := mustPost(t, ts.URL+"/agents/register", map[string]any{
		"name": "orchestrator-myapp-g1", "project": "myapp", "tmuxSession": "tn-myapp-g1",
	})
	resp.Body.Close()

	var killedSession string
	var killCalls int
	srv.mu.Lock()
	srv.killSession = func(session string) error {
		killCalls++
		killedSession = session
		return nil
	}
	srv.mu.Unlock()

	retireResp := mustPost(t, ts.URL+"/agents/retire", map[string]any{"name": "orchestrator-myapp-g1"})
	defer retireResp.Body.Close()
	if retireResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", retireResp.StatusCode)
	}

	if a := getAgent(t, ts, "orchestrator-myapp-g1"); a.Alive {
		t.Errorf("expected the agent deregistered (not alive) immediately, got %+v", a)
	}
	if killCalls != 0 {
		t.Fatalf("expected the kill NOT to have happened synchronously within the request, got %d calls", killCalls)
	}

	// Bypass the real retireKillDelay sleep — call the delayed part directly.
	srv.finishRetire("orchestrator-myapp-g1", "tn-myapp-g1")

	if killCalls != 1 || killedSession != "tn-myapp-g1" {
		t.Errorf("expected killSession called once with tn-myapp-g1, got %d calls with %q", killCalls, killedSession)
	}
	srv.mu.Lock()
	activity := srv.state.Activity
	srv.mu.Unlock()
	found := false
	for _, e := range activity {
		if e.Agent == "orchestrator-myapp-g1" && strings.Contains(e.Text, "Retired") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected a Retired activity entry for orchestrator-myapp-g1, got %+v", activity)
	}
}

// TestHandleRetire_NonDrainingWithPendingStill409 verifies the narrowed
// 409 rule: an agent that is NOT draining (still Accepting) with unacked
// messages is still refused — a live accepting agent retiring with unread
// mail is a mistake worth blocking, unlike a drained one just finishing.
func TestHandleRetire_NonDrainingWithPendingStill409(t *testing.T) {
	srv, ts := newTestServer(t)
	register(t, ts, "orchestrator-myapp-g1", "myapp") // Accepting=true by default

	srv.mu.Lock()
	srv.state.Messages = append(srv.state.Messages, &Message{
		ID: "m1", To: "orchestrator-myapp-g1", Status: "queued", CreatedAt: time.Now(),
	})
	srv.mu.Unlock()

	resp := mustPost(t, ts.URL+"/agents/retire", map[string]any{"name": "orchestrator-myapp-g1"})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("expected 409 for a non-draining agent with pending messages, got %d", resp.StatusCode)
	}
	if a := getAgent(t, ts, "orchestrator-myapp-g1"); a.DeregisteredAt != nil {
		t.Errorf("expected the agent to remain registered after a refused retire, got %+v", a)
	}
}

// TestHandleRetire_DrainingReassignsPendingToAcceptingAgent verifies the
// core fix: a DRAINING agent with unacked messages retires successfully
// (200, not 409), and every pending message ends up reassigned — status
// reset to queued, To pointed at the current accepting agent for the
// project — rather than being lost or permanently blocking retirement.
// Message id, taskPath, from, and text must all survive the move
// unchanged (a re-delivery, not a new message).
func TestHandleRetire_DrainingReassignsPendingToAcceptingAgent(t *testing.T) {
	srv, ts := newTestServer(t)
	register(t, ts, "orchestrator-myapp-g1", "myapp")
	drainResp := mustPost(t, ts.URL+"/agents/drain", map[string]any{"name": "orchestrator-myapp-g1"})
	drainResp.Body.Close()
	register(t, ts, "orchestrator-myapp-g2", "myapp") // the current accepting agent

	srv.mu.Lock()
	srv.state.Messages = append(srv.state.Messages, &Message{
		ID: "m-old-1", To: "orchestrator-myapp-g1", TaskPath: "Tasks/A.md", From: "team-lead",
		Text: "status update A", Status: "delivered", CreatedAt: time.Now().Add(-time.Hour),
	}, &Message{
		ID: "m-old-2", To: "orchestrator-myapp-g1", TaskPath: "Tasks/B.md", From: "team-lead",
		Text: "status update B", Status: "queued", CreatedAt: time.Now().Add(-time.Minute),
	})
	srv.mu.Unlock()

	resp := mustPost(t, ts.URL+"/agents/retire", map[string]any{"name": "orchestrator-myapp-g1"})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 for a draining agent's retire, got %d", resp.StatusCode)
	}
	if a := getAgent(t, ts, "orchestrator-myapp-g1"); a.DeregisteredAt == nil {
		t.Errorf("expected the agent deregistered after a successful retire, got %+v", a)
	}

	srv.mu.Lock()
	defer srv.mu.Unlock()
	byID := map[string]*Message{}
	for _, m := range srv.state.Messages {
		byID[m.ID] = m
	}
	m1, m2 := byID["m-old-1"], byID["m-old-2"]
	if m1 == nil || m2 == nil {
		t.Fatalf("expected both messages to still exist by their original ids, got %+v", srv.state.Messages)
	}
	for _, m := range []*Message{m1, m2} {
		if m.To != "orchestrator-myapp-g2" {
			t.Errorf("expected %s reassigned to the accepting agent orchestrator-myapp-g2, got %q", m.ID, m.To)
		}
		if m.Status != "queued" {
			t.Errorf("expected %s reset to queued, got %q", m.ID, m.Status)
		}
		if m.DeliveredAt != nil {
			t.Errorf("expected %s's DeliveredAt cleared, got %v", m.ID, m.DeliveredAt)
		}
	}
	if m1.TaskPath != "Tasks/A.md" || m1.From != "team-lead" || m1.Text != "status update A" {
		t.Errorf("expected m-old-1's taskPath/from/text preserved, got %+v", m1)
	}
	if m2.TaskPath != "Tasks/B.md" || m2.From != "team-lead" || m2.Text != "status update B" {
		t.Errorf("expected m-old-2's taskPath/from/text preserved, got %+v", m2)
	}
}

// TestHandleRetire_DrainingWithNoAcceptingAgentReassignsToLogicalQueue
// verifies the fallback destination: with no accepting agent at all for
// the project, a draining agent's pending messages land on the logical
// orchestrator-<slug> queue name (picked up by whichever generation
// registers next, via retargetLogicalQueueLocked) rather than being
// stranded on the retiring agent's now-dead concrete name.
func TestHandleRetire_DrainingWithNoAcceptingAgentReassignsToLogicalQueue(t *testing.T) {
	srv, ts := newTestServer(t)
	register(t, ts, "orchestrator-myapp-g1", "myapp")
	drainResp := mustPost(t, ts.URL+"/agents/drain", map[string]any{"name": "orchestrator-myapp-g1"})
	drainResp.Body.Close()
	// No other agent registered for myapp — nothing accepting.

	srv.mu.Lock()
	srv.state.Messages = append(srv.state.Messages, &Message{
		ID: "m-stranded", To: "orchestrator-myapp-g1", Status: "delivered", CreatedAt: time.Now(),
	})
	srv.mu.Unlock()

	resp := mustPost(t, ts.URL+"/agents/retire", map[string]any{"name": "orchestrator-myapp-g1"})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	msgs := queuedMessagesTo(srv, "orchestrator-myapp")
	found := false
	for _, m := range msgs {
		if m.ID == "m-stranded" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected m-stranded reassigned to the logical queue orchestrator-myapp, got messages to that name: %+v", msgs)
	}
}

// TestSendInboxAck verifies a message sent directly to a registered agent is
// delivered via inbox and can be acknowledged with a stored response.
func TestSendInboxAck(t *testing.T) {
	_, ts := newTestServer(t)
	register(t, ts, "agent-1", "proj-a")

	sendResp := mustPost(t, ts.URL+"/messages", map[string]any{"to": "agent-1", "text": "hello"})
	if sendResp.StatusCode != http.StatusOK {
		t.Fatalf("send: expected 200, got %d", sendResp.StatusCode)
	}
	var sent struct {
		Message Message `json:"message"`
	}
	decodeInto(t, sendResp, &sent)
	if sent.Message.ID == "" {
		t.Fatalf("expected a message id, got %+v", sent.Message)
	}

	inboxResp, err := http.Get(ts.URL + "/inbox?name=agent-1&wait=1")
	if err != nil {
		t.Fatal(err)
	}
	var inbox struct {
		Messages []*Message `json:"messages"`
	}
	decodeInto(t, inboxResp, &inbox)
	if len(inbox.Messages) != 1 || inbox.Messages[0].ID != sent.Message.ID {
		t.Fatalf("expected 1 delivered message with id %s, got %+v", sent.Message.ID, inbox.Messages)
	}
	if inbox.Messages[0].Text != "hello" {
		t.Errorf("unexpected message text: %q", inbox.Messages[0].Text)
	}

	ackResp := mustPost(t, ts.URL+"/ack", map[string]any{"name": "agent-1", "msgId": sent.Message.ID, "response": "done"})
	ackResp.Body.Close()
	if ackResp.StatusCode != http.StatusOK {
		t.Fatalf("ack: expected 200, got %d", ackResp.StatusCode)
	}

	getResp, err := http.Get(ts.URL + "/messages?id=" + sent.Message.ID)
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Message Message `json:"message"`
	}
	decodeInto(t, getResp, &got)
	if got.Message.Status != "acked" {
		t.Errorf("expected status acked, got %q", got.Message.Status)
	}
	if got.Message.Response != "done" {
		t.Errorf("expected response %q, got %q", "done", got.Message.Response)
	}
}

// TestDispatchMessage_ReturnsIndependentSnapshot is the regression test
// for a real `go test -race` finding: dispatchMessage used to return the
// *Message pointer stored in s.state.Messages, and handleSendMessage
// JSON-encoded that same pointer AFTER releasing s.mu — racing with any
// concurrent write to it (an inbox poll marking it delivered, an ack
// marking it acked). Verified directly: mutate the STORED message's
// fields (simulating exactly that concurrent write) immediately after
// dispatchMessage returns, and confirm the returned value is unaffected —
// proving it's an independent copy, not an alias.
func TestDispatchMessage_ReturnsIndependentSnapshot(t *testing.T) {
	srv, ts := newTestServer(t)
	register(t, ts, "agent-snap", "proj-snap")

	got, _, _ := srv.dispatchMessage(sendRequest{To: "agent-snap", Text: "hello"})
	if got.Status != "queued" {
		t.Fatalf("expected the snapshot's initial status to be queued, got %q", got.Status)
	}

	// Simulate a concurrent mutation of the SAME message in state (what an
	// inbox poll or ack would do) immediately after dispatchMessage returned.
	srv.mu.Lock()
	for _, m := range srv.state.Messages {
		if m.ID == got.ID {
			m.Status = "acked"
			now := time.Now()
			m.AckedAt = &now
			m.Response = "done"
		}
	}
	srv.mu.Unlock()

	if got.Status != "queued" || got.AckedAt != nil || got.Response != "" {
		t.Errorf("expected the returned snapshot unaffected by the later mutation, got %+v", got)
	}
}

// TestInboxLongPollDelivers verifies a blocked /inbox call wakes up and
// delivers a message that arrives mid-wait, rather than busy-looping or
// waiting out the full timeout.
func TestInboxLongPollDelivers(t *testing.T) {
	_, ts := newTestServer(t)
	register(t, ts, "agent-2", "proj-b")

	type result struct {
		messages []*Message
		err      error
	}
	done := make(chan result, 1)
	go func() {
		resp, err := http.Get(ts.URL + "/inbox?name=agent-2&wait=5")
		if err != nil {
			done <- result{err: err}
			return
		}
		defer resp.Body.Close()
		var out struct {
			Messages []*Message `json:"messages"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			done <- result{err: err}
			return
		}
		done <- result{messages: out.Messages}
	}()

	time.Sleep(200 * time.Millisecond)
	sendResp := mustPost(t, ts.URL+"/messages", map[string]any{"to": "agent-2", "text": "hi there"})
	sendResp.Body.Close()

	select {
	case r := <-done:
		if r.err != nil {
			t.Fatal(r.err)
		}
		if len(r.messages) != 1 || r.messages[0].Text != "hi there" {
			t.Fatalf("expected 1 delivered message, got %+v", r.messages)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for long-poll to deliver; broadcast wakeup likely broken")
	}
}

// TestWebhookSpawnTagRoutes verifies a task.created webhook with a "spawn"
// tag routes an assignment message to the alive agent for its project.
func TestWebhookSpawnTagRoutes(t *testing.T) {
	_, ts := newTestServer(t)
	register(t, ts, "agent-3", "proj-c")

	payload := map[string]any{
		"event": "task.created",
		"data": map[string]any{
			"task": map[string]any{
				"path":     "Tasks/Foo.md",
				"title":    "Foo",
				"tags":     []string{"spawn"},
				"projects": []string{"proj-c"},
			},
		},
	}
	resp := mustPost(t, ts.URL+"/webhooks/tasknotes", payload)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("webhook: expected 200, got %d", resp.StatusCode)
	}

	msgResp, err := http.Get(ts.URL + "/messages?to=agent-3&status=queued")
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		Messages []*Message `json:"messages"`
	}
	decodeInto(t, msgResp, &out)

	if len(out.Messages) != 1 {
		t.Fatalf("expected 1 queued message routed by spawn tag, got %+v", out.Messages)
	}
	want := "Task assigned: Foo (Tasks/Foo.md)"
	if out.Messages[0].Text != want {
		t.Errorf("got text %q, want %q", out.Messages[0].Text, want)
	}
	if out.Messages[0].TaskPath != "Tasks/Foo.md" {
		t.Errorf("expected taskPath to be set, got %+v", out.Messages[0])
	}
}

// fakeTaskNotesUpdateAPI simulates just enough of the TaskNotes API to
// exercise the needs-input auto-flip: it records every PUT
// /api/tasks/:id call (path + decoded body) and returns 200 with the
// updated fields echoed back.
func fakeTaskNotesUpdateAPI(t *testing.T) (*httptest.Server, *[]struct {
	Path string
	Body map[string]any
}) {
	t.Helper()
	var mu sync.Mutex
	var calls []struct {
		Path string
		Body map[string]any
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/api/tasks/") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		id := strings.TrimPrefix(r.URL.Path, "/api/tasks/")
		w.Header().Set("Content-Type", "application/json")

		switch r.Method {
		case http.MethodGet:
			// autoFlipToInProgress GETs first (to clear the ask block /
			// prepend a history entry) before the PUT this helper is
			// mainly here to observe.
			json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"path": id, "details": ""}})
		case http.MethodPut:
			var body map[string]any
			json.NewDecoder(r.Body).Decode(&body)
			mu.Lock()
			calls = append(calls, struct {
				Path string
				Body map[string]any
			}{Path: id, Body: body})
			mu.Unlock()
			json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"path": id}})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &calls
}

// TestRouteTaskNotification_AutoFlipsNeedsInputOnClaudeReply verifies an
// @claude: line routing for a needs-input task both queues the message AND
// issues a PUT flipping the task's status to in-progress — the reply is
// the unblock signal.
func TestRouteTaskNotification_AutoFlipsNeedsInputOnClaudeReply(t *testing.T) {
	apiSrv, calls := fakeTaskNotesUpdateAPI(t)
	srv, ts := newTestServer(t)
	srv.tnClient = NewClient(Config{URL: apiSrv.URL})
	register(t, ts, "agent-needsinput", "proj-ni")

	srv.routeTaskNotification(webhookTask{
		Path: "Tasks/Blocked.md", Title: "Blocked", Status: "needs-input",
		Projects: []string{"proj-ni"}, Details: "@claude: here's my answer",
	})

	msgs := queuedMessagesTo(srv, "agent-needsinput")
	if len(msgs) != 1 || msgs[0].Text != "here's my answer" {
		t.Fatalf("expected the reply to route, got %+v", msgs)
	}

	if len(*calls) != 1 {
		t.Fatalf("expected exactly 1 PUT, got %d: %+v", len(*calls), *calls)
	}
	if (*calls)[0].Path != "Tasks/Blocked.md" {
		t.Errorf("expected PUT to Tasks/Blocked.md, got %q", (*calls)[0].Path)
	}
	if (*calls)[0].Body["status"] != "in-progress" {
		t.Errorf("expected status:in-progress in the PUT body, got %+v", (*calls)[0].Body)
	}
}

// TestAutoFlipToInProgress_ClearsAskAndLogsHistory verifies the
// SPEC-note-layout.md behavior specifically: an existing ask block is
// removed from the PUT'd details, and a new "user replied: ..." history
// entry (with the reply text) is prepended — in the same PUT as the
// status change.
func TestAutoFlipToInProgress_ClearsAskAndLogsHistory(t *testing.T) {
	const path = "Tasks/Blocked.md"
	existingDetails := applyNoteBodyEdit("Original description.", func(nb *noteBody) {
		nb.Ask = "Merge MR !973 or reply with an instruction."
	})

	var putBody map[string]any
	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.Method {
		case http.MethodGet:
			json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"path": path, "details": existingDetails}})
		case http.MethodPut:
			json.NewDecoder(r.Body).Decode(&putBody)
			json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"path": path}})
		}
	}))
	defer apiSrv.Close()

	srv, ts := newTestServer(t)
	srv.tnClient = NewClient(Config{URL: apiSrv.URL})
	register(t, ts, "agent-needsinput", "proj-ni")

	srv.routeTaskNotification(webhookTask{
		Path: path, Title: "Blocked", Status: "needs-input",
		Projects: []string{"proj-ni"}, Details: "@claude: merged it, please continue",
	})

	if putBody["status"] != "in-progress" {
		t.Fatalf("expected status:in-progress, got %+v", putBody)
	}
	newDetails, _ := putBody["details"].(string)
	if strings.Contains(newDetails, "tn:ask") {
		t.Errorf("expected the ask block cleared, got:\n%s", newDetails)
	}
	if !strings.Contains(newDetails, "user replied: merged it, please continue") {
		t.Errorf("expected a history entry with the reply text, got:\n%s", newDetails)
	}
	if !strings.Contains(newDetails, "Original description.") {
		t.Errorf("expected the description preserved, got:\n%s", newDetails)
	}
}

// TestRouteTaskNotification_NoAutoFlipOutsideNeedsInput verifies no PUT is
// issued when the task isn't in needs-input or review (even though the
// @claude: line still routes), and none when needs-input but no line
// actually routed.
func TestRouteTaskNotification_NoAutoFlipOutsideNeedsInput(t *testing.T) {
	apiSrv, calls := fakeTaskNotesUpdateAPI(t)
	srv, ts := newTestServer(t)
	srv.tnClient = NewClient(Config{URL: apiSrv.URL})
	register(t, ts, "agent-other", "proj-other")

	// Status "open": @claude: line routes normally, but no auto-flip.
	srv.routeTaskNotification(webhookTask{
		Path: "Tasks/Open.md", Title: "Open", Status: "open",
		Projects: []string{"proj-other"}, Details: "@claude: fyi only",
	})
	if msgs := queuedMessagesTo(srv, "agent-other"); len(msgs) != 1 {
		t.Fatalf("expected the line to still route for an open task, got %+v", msgs)
	}

	// Status "needs-input" but no @claude: line at all: nothing routes, no PUT.
	srv.routeTaskNotification(webhookTask{
		Path: "Tasks/Quiet.md", Title: "Quiet", Status: "needs-input",
		Projects: []string{"proj-other"}, Details: "just some notes, no mention",
	})

	if len(*calls) != 0 {
		t.Fatalf("expected no PUT for a non-needs-input status or a needs-input task with nothing routed, got %+v", *calls)
	}
}

// TestRouteTaskNotification_AutoFlipsReviewOnClaudeReply verifies an
// @claude: line routing for a review task both queues the message AND
// issues a PUT flipping the task's status back to in-progress — a reply on
// a review task is a review rejection/change request (ORCHESTRATOR.md),
// same treatment as the needs-input auto-flip above.
func TestRouteTaskNotification_AutoFlipsReviewOnClaudeReply(t *testing.T) {
	apiSrv, calls := fakeTaskNotesUpdateAPI(t)
	srv, ts := newTestServer(t)
	srv.tnClient = NewClient(Config{URL: apiSrv.URL})
	register(t, ts, "agent-review", "proj-review")

	srv.routeTaskNotification(webhookTask{
		Path: "Tasks/InReview.md", Title: "InReview", Status: "review",
		Projects: []string{"proj-review"}, Details: "@claude: actually please also handle X",
	})

	msgs := queuedMessagesTo(srv, "agent-review")
	if len(msgs) != 1 || msgs[0].Text != "actually please also handle X" {
		t.Fatalf("expected the reply to route, got %+v", msgs)
	}

	if len(*calls) != 1 {
		t.Fatalf("expected exactly 1 PUT, got %d: %+v", len(*calls), *calls)
	}
	if (*calls)[0].Path != "Tasks/InReview.md" {
		t.Errorf("expected PUT to Tasks/InReview.md, got %q", (*calls)[0].Path)
	}
	if (*calls)[0].Body["status"] != "in-progress" {
		t.Errorf("expected status:in-progress in the PUT body, got %+v", (*calls)[0].Body)
	}
}

// TestWebhookSkipsAssignmentInHumanAttentionStatus verifies a claude-tagged
// task parked in a human-attention status (triage/needs-input/review) is
// not assigned — the user must move it to open (their approval) before it
// routes. The same task posted again with status "open" then routes
// normally.
func TestWebhookSkipsAssignmentInHumanAttentionStatus(t *testing.T) {
	_, ts := newTestServer(t)
	register(t, ts, "agent-triage", "proj-triage")

	post := func(status string) {
		payload := map[string]any{
			"event": "task.updated",
			"data": map[string]any{
				"task": map[string]any{
					"path":     "Tasks/Parked.md",
					"title":    "Parked",
					"status":   status,
					"tags":     []string{"claude"},
					"projects": []string{"proj-triage"},
				},
			},
		}
		resp := mustPost(t, ts.URL+"/webhooks/tasknotes", payload)
		resp.Body.Close()
	}

	for _, status := range []string{"triage", "needs-input", "review"} {
		post(status)
	}

	msgResp, err := http.Get(ts.URL + "/messages?to=agent-triage&status=queued")
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		Messages []*Message `json:"messages"`
	}
	decodeInto(t, msgResp, &out)
	if len(out.Messages) != 0 {
		t.Fatalf("expected no assignment while parked in a human-attention status, got %+v", out.Messages)
	}

	// Moving it to open is the user's approval: it should route now.
	post("open")

	msgResp2, err := http.Get(ts.URL + "/messages?to=agent-triage&status=queued")
	if err != nil {
		t.Fatal(err)
	}
	var out2 struct {
		Messages []*Message `json:"messages"`
	}
	decodeInto(t, msgResp2, &out2)
	if len(out2.Messages) != 1 {
		t.Fatalf("expected 1 assignment once the task moved to open, got %+v", out2.Messages)
	}
}

// TestWebhookSkipsAssignmentInProgressOrDone is the regression test for the
// live bug: hasPendingAssignmentLocked alone stops blocking the moment the
// PRIOR assignment is acked, so every webhook-miss-reconciler pass (10min +
// every daemon restart) was re-assigning any claude-tagged
// recently-modified task even once it was in-progress or already done.
// assignableStatuses (a positive allowlist) must block both, and still
// route a genuinely open task.
func TestWebhookSkipsAssignmentInProgressOrDone(t *testing.T) {
	_, ts := newTestServer(t)
	register(t, ts, "agent-ip", "proj-ip")

	post := func(status string) {
		payload := map[string]any{
			"event": "task.updated",
			"data": map[string]any{
				"task": map[string]any{
					"path":     "Tasks/Working.md",
					"title":    "Working",
					"status":   status,
					"tags":     []string{"claude"},
					"projects": []string{"proj-ip"},
				},
			},
		}
		resp := mustPost(t, ts.URL+"/webhooks/tasknotes", payload)
		resp.Body.Close()
	}

	for _, status := range []string{"in-progress", "done"} {
		post(status)
	}

	msgResp, err := http.Get(ts.URL + "/messages?to=agent-ip&status=queued")
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		Messages []*Message `json:"messages"`
	}
	decodeInto(t, msgResp, &out)
	if len(out.Messages) != 0 {
		t.Fatalf("expected NO re-assignment for an in-progress or done task, got %+v", out.Messages)
	}

	// A genuinely open task must still route.
	post("open")

	msgResp2, err := http.Get(ts.URL + "/messages?to=agent-ip&status=queued")
	if err != nil {
		t.Fatal(err)
	}
	var out2 struct {
		Messages []*Message `json:"messages"`
	}
	decodeInto(t, msgResp2, &out2)
	if len(out2.Messages) != 1 {
		t.Fatalf("expected 1 assignment for the open task, got %+v", out2.Messages)
	}
}

// TestWebhookSpawnTagDedupes verifies that TaskNotes firing task.created and
// task.updated in quick succession for the same tagged task produces exactly
// one queued assignment, not two — and that once that assignment is acked, a
// subsequent webhook post is free to route a new one.
func TestWebhookSpawnTagDedupes(t *testing.T) {
	_, ts := newTestServer(t)
	register(t, ts, "agent-dedup", "proj-dedup")

	post := func(event, status string) {
		payload := map[string]any{
			"event": event,
			"data": map[string]any{
				"task": map[string]any{
					"path":     "Tasks/Dedup.md",
					"title":    "Dedup",
					"status":   status,
					"tags":     []string{"claude"},
					"projects": []string{"proj-dedup"},
				},
			},
		}
		resp := mustPost(t, ts.URL+"/webhooks/tasknotes", payload)
		resp.Body.Close()
	}

	// TaskNotes fires both events for the same edit.
	post("task.created", "open")
	post("task.updated", "open")

	queued := func() []*Message {
		resp, err := http.Get(ts.URL + "/messages?to=agent-dedup&status=queued")
		if err != nil {
			t.Fatal(err)
		}
		var out struct {
			Messages []*Message `json:"messages"`
		}
		decodeInto(t, resp, &out)
		return out.Messages
	}

	msgs := queued()
	if len(msgs) != 1 {
		t.Fatalf("expected exactly 1 queued assignment after created+updated pair, got %d: %+v", len(msgs), msgs)
	}
	firstID := msgs[0].ID

	// Ack it: per the AssignedTasks fix (see routeTaskNotification), an ack
	// alone must NOT re-arm assignment — the task is still in the same open
	// episode. This is the exact live bug: a reconciler pass re-observing a
	// still-open, already-acked task must not re-assign it.
	ackResp := mustPost(t, ts.URL+"/ack", map[string]any{"name": "agent-dedup", "msgId": firstID, "response": "done"})
	ackResp.Body.Close()

	post("task.updated", "open")

	msgs = queued()
	if len(msgs) != 0 {
		t.Fatalf("expected NO new assignment after ack + same-episode re-tag (still open), got %d: %+v", len(msgs), msgs)
	}

	// The task genuinely leaves assignableStatuses (in-progress) and comes
	// back to open — a new episode — so exactly one fresh assignment fires.
	post("task.updated", "in-progress")
	post("task.updated", "open")

	msgs = queued()
	if len(msgs) != 1 {
		t.Fatalf("expected exactly 1 new queued assignment after a genuine reopen, got %d: %+v", len(msgs), msgs)
	}
	if msgs[0].ID == "" || msgs[0].ID == firstID {
		t.Errorf("expected a distinct new message id, got %q (same as first: %v)", msgs[0].ID, msgs[0].ID == firstID)
	}
}

// TestWebhookAssignment_IntegrationBranchSuffixOnlyWhenSet verifies a
// webhook-routed assignment (routeTaskNotification, the same entry point
// the reconciler funnels through) carries " [integration branch: <b>]"
// when the task's own customProperties.integration-branch is set, and
// carries no suffix at all for an otherwise-identical task with no branch
// set — task-level only, no repo-wide/project-level source.
func TestWebhookAssignment_IntegrationBranchSuffixOnlyWhenSet(t *testing.T) {
	srv, ts := newTestServer(t)
	register(t, ts, "agent-ib", "proj-ib")

	post := func(path, title, branch, role string) {
		task := map[string]any{
			"path": path, "title": title, "status": "open",
			"tags": []string{"claude"}, "projects": []string{"proj-ib"},
		}
		cp := map[string]any{}
		if branch != "" {
			cp["integration-branch"] = branch
		}
		if role != "" {
			cp["integration-role"] = role
		}
		if len(cp) > 0 {
			task["customProperties"] = cp
		}
		payload := map[string]any{"event": "task.created", "data": map[string]any{"task": task}}
		resp := mustPost(t, ts.URL+"/webhooks/tasknotes", payload)
		resp.Body.Close()
	}

	post("Tasks/OnBranch.md", "OnBranch", "feat/e2e-recorder", "")
	post("Tasks/NoBranch.md", "NoBranch", "", "")
	post("Tasks/Owner.md", "Owner", "feat/e2e-recorder", "owner")

	msgs := queuedMessagesTo(srv, "agent-ib")
	if len(msgs) != 3 {
		t.Fatalf("expected 3 assignments, got %+v", msgs)
	}
	byPath := map[string]string{}
	for _, m := range msgs {
		byPath[m.TaskPath] = m.Text
	}
	wantOnBranch := "Task assigned: OnBranch (Tasks/OnBranch.md) [integration branch: feat/e2e-recorder]"
	if byPath["Tasks/OnBranch.md"] != wantOnBranch {
		t.Errorf("member: got %q, want %q", byPath["Tasks/OnBranch.md"], wantOnBranch)
	}
	wantNoBranch := "Task assigned: NoBranch (Tasks/NoBranch.md)"
	if byPath["Tasks/NoBranch.md"] != wantNoBranch {
		t.Errorf("got %q, want %q (no suffix when unset)", byPath["Tasks/NoBranch.md"], wantNoBranch)
	}
	wantOwner := "Task assigned: Owner (Tasks/Owner.md) [integration OWNER of feat/e2e-recorder — consolidated MR to development]"
	if byPath["Tasks/Owner.md"] != wantOwner {
		t.Errorf("owner: got %q, want %q", byPath["Tasks/Owner.md"], wantOwner)
	}
}

// TestNormalizeProjectSlug covers wikilink bracket stripping, path-prefix
// stripping, .md-suffix stripping, and the plain-slug passthrough that keeps
// pre-existing tests using bare project slugs working unchanged.
func TestNormalizeProjectSlug(t *testing.T) {
	cases := []struct{ in, want string }{
		{"proj-c", "proj-c"},
		{"[[E2E Tests]]", "e2e tests"},
		{"[[Myapp/Subprojects/E2E Tests]]", "e2e tests"},
		{"Tasks/SomeProject.md", "someproject"},
	}
	for _, c := range cases {
		if got := normalizeProjectSlug(c.in); got != c.want {
			t.Errorf("normalizeProjectSlug(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestWebhookTaskRoutingSlug covers webhookTask.routingSlug()'s resolution
// order directly: top-level claude-project, then customProperties
// claude-project, then a normalized projects[0] fallback, then empty.
func TestWebhookTaskRoutingSlug(t *testing.T) {
	t.Run("prefers top-level claude-project", func(t *testing.T) {
		tk := webhookTask{ClaudeProject: "top-level-slug", Projects: []string{"[[Ignored]]"}}
		if got := tk.routingSlug(); got != "top-level-slug" {
			t.Errorf("got %q", got)
		}
	})
	t.Run("prefers customProperties claude-project over projects fallback", func(t *testing.T) {
		tk := webhookTask{CustomProperties: map[string]string{"claude-project": "nested-slug"}, Projects: []string{"[[Ignored]]"}}
		if got := tk.routingSlug(); got != "nested-slug" {
			t.Errorf("got %q", got)
		}
	})
	t.Run("falls back to normalized wikilink projects[0]", func(t *testing.T) {
		tk := webhookTask{Projects: []string{"[[Myapp/Subprojects/E2E Tests]]"}}
		if got := tk.routingSlug(); got != "e2e tests" {
			t.Errorf("got %q", got)
		}
	})
	t.Run("empty when nothing to route on", func(t *testing.T) {
		tk := webhookTask{}
		if got := tk.routingSlug(); got != "" {
			t.Errorf("got %q, want empty", got)
		}
	})
}

// TestWebhookRoutingSlugFromCustomProperty verifies a webhook carrying a
// "claude-project" custom property routes using that slug, even though
// projects[] holds an unrelated wikilink.
func TestWebhookRoutingSlugFromCustomProperty(t *testing.T) {
	_, ts := newTestServer(t)
	register(t, ts, "agent-cp", "custom-slug")

	payload := map[string]any{
		"event": "task.created",
		"data": map[string]any{
			"task": map[string]any{
				"path":     "Tasks/CP.md",
				"title":    "CP",
				"tags":     []string{"claude"},
				"projects": []string{"[[Some Other Project]]"},
				"customProperties": map[string]any{
					"claude-project": "custom-slug",
				},
			},
		},
	}
	resp := mustPost(t, ts.URL+"/webhooks/tasknotes", payload)
	resp.Body.Close()

	msgResp, err := http.Get(ts.URL + "/messages?to=agent-cp&status=queued")
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		Messages []*Message `json:"messages"`
	}
	decodeInto(t, msgResp, &out)

	if len(out.Messages) != 1 {
		t.Fatalf("expected 1 queued message routed via claude-project, got %+v", out.Messages)
	}
	if out.Messages[0].Project != "custom-slug" {
		t.Errorf("expected message project %q, got %q", "custom-slug", out.Messages[0].Project)
	}
}

// TestWebhookRoutingSlugFromWikilinkFallback verifies that without a
// claude-project custom property, a path-prefixed projects[] wikilink is
// normalized into a bare slug for routing.
func TestWebhookRoutingSlugFromWikilinkFallback(t *testing.T) {
	_, ts := newTestServer(t)
	register(t, ts, "agent-wl", "E2E Tests")

	payload := map[string]any{
		"event": "task.created",
		"data": map[string]any{
			"task": map[string]any{
				"path":     "Tasks/WL.md",
				"title":    "WL",
				"tags":     []string{"claude"},
				"projects": []string{"[[Myapp/Subprojects/E2E Tests]]"},
			},
		},
	}
	resp := mustPost(t, ts.URL+"/webhooks/tasknotes", payload)
	resp.Body.Close()

	msgResp, err := http.Get(ts.URL + "/messages?to=agent-wl&status=queued")
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		Messages []*Message `json:"messages"`
	}
	decodeInto(t, msgResp, &out)

	if len(out.Messages) != 1 {
		t.Fatalf("expected 1 queued message routed via normalized wikilink slug, got %+v", out.Messages)
	}
	if out.Messages[0].Project != "e2e tests" {
		t.Errorf("expected message project %q, got %q", "e2e tests", out.Messages[0].Project)
	}
}

// TestExtractClaudeLines covers the loosened @claude line separators
// (colon, dash, em-dash, comma, bare space), the exclusions ("@claudeX",
// mid-line mentions), and that a bare "@claude" line with nothing to say
// extracts nothing.
func TestExtractClaudeLines(t *testing.T) {
	cases := []struct {
		name    string
		details string
		want    []string
	}{
		{"colon", "@claude: what is the plan here", []string{"what is the plan here"}},
		{"dash", "@claude - what is the plan here", []string{"what is the plan here"}},
		{"em-dash", "@claude — what is the plan here", []string{"what is the plan here"}},
		{"comma", "@claude, what is the plan here", []string{"what is the plan here"}},
		{"bare space", "@claude what is the plan here", []string{"what is the plan here"}},
		{"suffix not a boundary", "@claudeX this should not match", nil},
		{"mid-line mention", "please see @claude: this should not match", nil},
		{"bare with nothing after", "@claude", nil},
		{"bare with trailing whitespace only", "@claude   ", nil},
		// Callout-quoted forms: SPEC-note-layout.md's reply block tells the
		// user to write "@claude: ..." on a line inside a callout, where
		// wrapCallout `> `-prefixes every line. Without matching these, a
		// reply typed exactly where instructed would never route — this is
		// a hard requirement, not a nice-to-have (see claudeLineRe's doc).
		{"single quote-prefixed", "> @claude: fix the thing", []string{"fix the thing"}},
		{"quote-prefixed no space before text", "> @claude:fix the thing", []string{"fix the thing"}},
		{"quote-prefixed bare separator", "> @claude fix the thing", []string{"fix the thing"}},
		{"nested quote-prefixed", "> > @claude: fix the thing", []string{"fix the thing"}},
		{"quote-prefixed suffix not a boundary", "> @claudeX this should not match", nil},
		{"quote-prefixed mid-line mention still excluded", "> please see @claude: this should not match", nil},
		// Regression: the reply block's own unfilled placeholder line is
		// exactly "@claude:" with nothing after it (see defaultReplyBody).
		// An optional (rather than alternated) separator lets the regex
		// satisfy its capture group by treating the colon itself as
		// content instead of as a separator — verified live, this
		// self-triggered the needs-input auto-flip the instant `tn ask`
		// wrote the block, before any user reply. Must never match.
		{"bare colon nothing after", "@claude:", nil},
		{"quote-prefixed colon nothing after", "> @claude:", nil},
		{"quote-prefixed colon then only whitespace", "> @claude: ", nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := extractClaudeLines(c.details)
			if len(got) != len(c.want) {
				t.Fatalf("extractClaudeLines(%q) = %v, want %v", c.details, got, c.want)
			}
			for i := range c.want {
				if got[i] != c.want[i] {
					t.Errorf("line %d: got %q, want %q", i, got[i], c.want[i])
				}
			}
		})
	}

	// Multiple lines in one details blob, mixing separators.
	details := "notes\n@claude: first thing\nmore notes\n@claude - second thing\nplease see @claude: not this one"
	got := extractClaudeLines(details)
	want := []string{"first thing", "second thing"}
	if len(got) != len(want) {
		t.Fatalf("extractClaudeLines(multi-line) = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("line %d: got %q, want %q", i, got[i], want[i])
		}
	}
}

// TestExtractClaudeLines_FromRenderedReplyBlock is the realistic end-to-end
// version of the callout-quoted cases above: a task whose body actually
// went through applyNoteBodyEdit (tn ask), then the user typed a reply
// exactly where the reply block instructs them to ("@claude: ...", inside
// the callout, `> `-prefixed by wrapCallout) — this must extract exactly
// the reply text, from the real rendered form, not a hand-built string.
func TestExtractClaudeLines_FromRenderedReplyBlock(t *testing.T) {
	body := applyNoteBodyEdit("Original description.", func(nb *noteBody) {
		nb.Ask = "Merge or reply?"
		nb.Reply = defaultReplyBody
	})
	// The user replies by editing the placeholder line in Obsidian.
	body = strings.Replace(body, "> @claude:", "> @claude: go ahead and merge it", 1)

	got := extractClaudeLines(body)
	want := []string{"go ahead and merge it"}
	if len(got) != 1 || got[0] != want[0] {
		t.Fatalf("extractClaudeLines(rendered reply block) = %v, want %v\nbody:\n%s", got, want, body)
	}
}

// TestExtractClaudeLines_UnfilledReplyPlaceholderNeverRoutes is the
// regression test for a live incident: `tn ask`'s freshly written reply
// block (nb.Reply = defaultReplyBody, untouched by the user) must extract
// ZERO @claude lines. Caught by live smoke-testing — `tn ask` immediately
// self-triggered the needs-input auto-flip back to in-progress the instant
// it wrote the block, because the placeholder's bare "@claude:" (nothing
// after the colon) spuriously matched claudeLineRe's old optional-separator
// form.
func TestExtractClaudeLines_UnfilledReplyPlaceholderNeverRoutes(t *testing.T) {
	body := applyNoteBodyEdit("Original description.", func(nb *noteBody) {
		nb.Ask = "Merge or reply?"
		nb.Reply = defaultReplyBody
	})
	got := extractClaudeLines(body)
	if len(got) != 0 {
		t.Fatalf("expected an untouched reply placeholder to extract nothing, got %v\nbody:\n%s", got, body)
	}
}

// TestExtractClaudeLines_NeverMatchesInsideHistoryBlock is the regression
// test for a live incident: a user's original "@claude: ..." line, once
// swept into a permanent history entry (which happens naturally — a reply
// gets both routed AND logged), is `> `-prefixed by wrapCallout exactly
// like the reply block is, so claudeLineRe's callout-quote support would
// otherwise match it FOREVER on every future observation, regardless of
// dedup. Verified live: the same already-actioned reply text was
// re-dispatched as a "new" assignment four times over 30 minutes on a real
// task, each one reverting its status via the needs-input/review
// auto-flip. This constructs the exact shape observed live — a history
// entry whose OWN text ends with a raw "@claude: ..." line — and asserts
// extractClaudeLines finds NOTHING, even though the same text sitting
// OUTSIDE the history block (e.g. in the reply block) still routes
// normally.
func TestExtractClaudeLines_NeverMatchesInsideHistoryBlock(t *testing.T) {
	body := applyNoteBodyEdit("Original description.", func(nb *noteBody) {
		nb.History = []string{
			"**2026-08-19 09:43 — changepw:** Root cause found, MR up.\n\n" +
				"@claude: fix the followups. Do a thorough research on kratos and the error codes etc",
		}
	})
	if !strings.Contains(body, "<!-- tn:history -->") {
		t.Fatalf("expected a rendered history block, got:\n%s", body)
	}
	if got := extractClaudeLines(body); len(got) != 0 {
		t.Errorf("expected zero lines extracted from inside the history block, got %v\nbody:\n%s", got, body)
	}

	// Sanity check the negative: the identical text OUTSIDE history (e.g.
	// in the reply block) still routes normally — this isn't a case of the
	// regex silently breaking, only of history specifically being excluded.
	withReply := applyNoteBodyEdit("Original description.", func(nb *noteBody) {
		nb.Reply = "@claude: fix the followups. Do a thorough research on kratos and the error codes etc"
	})
	want := []string{"fix the followups. Do a thorough research on kratos and the error codes etc"}
	if got := extractClaudeLines(withReply); len(got) != 1 || got[0] != want[0] {
		t.Errorf("expected the same text in the reply block to still route, got %v", got)
	}
}

// TestExtractClaudeDirectives_MultiLineReply_CapturesEverything is the
// core regression test for the 2026-08-25 truncation incident:
// extractClaudeLines (the old regex-based extraction) only ever captured
// the FIRST LINE after "@claude:" because wrapCallout renders a
// multi-line reply as separate quoted lines and Go's regexp "." never
// crosses a newline. Confirmed live against real production messages: the
// note held the full text, the routed message did not. This asserts the
// fix — extractClaudeDirectives parses the note's structured blocks
// first (which already correctly reconstruct multi-line text) — recovers
// the COMPLETE reply, paragraph breaks included.
func TestExtractClaudeDirectives_MultiLineReply_CapturesEverything(t *testing.T) {
	multiline := "First paragraph.\n\nSecond paragraph, previously lost.\n\nThird paragraph, also previously lost."
	body := applyNoteBodyEdit("Original description.", func(nb *noteBody) {
		nb.Reply = "@claude: " + multiline
	})

	// The regression itself: the OLD function truncates to the first line.
	if old := extractClaudeLines(body); len(old) != 1 || old[0] != "First paragraph." {
		t.Fatalf("sanity check failed — expected extractClaudeLines to reproduce the historical bug (first line only), got %v", old)
	}

	got := extractClaudeDirectives(body)
	if len(got) != 1 || got[0] != multiline {
		t.Fatalf("extractClaudeDirectives(multi-line reply) = %q, want %q\nbody:\n%s", got, multiline, body)
	}
}

// TestExtractClaudeDirectives_MultipleMarkersInReplyBlock_EachBounded
// verifies a REPLY block with more than one "@claude" marker captures each
// directive bounded by the NEXT marker (or block end) — not the whole
// rest of the block collapsed into one, and not one directive bleeding
// into the next.
func TestExtractClaudeDirectives_MultipleMarkersInReplyBlock_EachBounded(t *testing.T) {
	body := applyNoteBodyEdit("Original description.", func(nb *noteBody) {
		nb.Reply = "@claude: do the first thing,\nwith a second line.\n\n@claude: do the second thing entirely unrelated."
	})
	got := extractClaudeDirectives(body)
	want := []string{
		"do the first thing,\nwith a second line.",
		"do the second thing entirely unrelated.",
	}
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("extractClaudeDirectives(two markers in reply) = %#v, want %#v\nbody:\n%s", got, want, body)
	}
}

// TestExtractClaudeDirectives_DescriptionBlock_StaysSingleLine documents a
// DELIBERATE scope boundary, not a gap: only nb.Reply gets the new
// capture-to-block-end treatment. Description is a general free-text
// field that can legitimately mix an "@claude:" directive with unrelated
// surrounding prose in the SAME block (this file's own dedup tests
// exercise exactly that shape for unanchored bodies), where capturing to
// block end would over-capture in a way it never does for Reply — whose
// entire purpose, per SPEC-note-layout.md, is to hold nothing but the
// answer. Every real 2026-08-25 incident happened in Reply specifically,
// so this narrower scope is a choice, not an oversight; widening it to
// Description would need a real boundary signal that field doesn't have.
func TestExtractClaudeDirectives_DescriptionBlock_StaysSingleLine(t *testing.T) {
	body := applyNoteBodyEdit("Some scene-setting text.\n\n@claude: first line only\nunrelated trailing text in the same block.", func(nb *noteBody) {
		nb.Ask = "placeholder so this note has at least one anchor"
	})
	got := extractClaudeDirectives(body)
	want := []string{"first line only"}
	if len(got) != 1 || got[0] != want[0] {
		t.Fatalf("extractClaudeDirectives(description) = %#v, want %#v — description must stay single-line, unrelated trailing text must not be swallowed\nbody:\n%s", got, want, body)
	}
}

// TestExtractClaudeDirectives_UnfilledPlaceholderNeverRoutes mirrors
// TestExtractClaudeLines_UnfilledReplyPlaceholderNeverRoutes for the new
// function — a bare "@claude:" placeholder (tn ask's freshly-written
// reply block, untouched by the user) must extract ZERO directives.
func TestExtractClaudeDirectives_UnfilledPlaceholderNeverRoutes(t *testing.T) {
	body := applyNoteBodyEdit("Original description.", func(nb *noteBody) {
		nb.Ask = "Merge or reply?"
		nb.Reply = defaultReplyBody
	})
	got := extractClaudeDirectives(body)
	if len(got) != 0 {
		t.Fatalf("expected an untouched reply placeholder to extract nothing, got %v\nbody:\n%s", got, body)
	}
}

// TestExtractClaudeDirectives_NeverMatchesInsideHistoryBlock mirrors
// TestExtractClaudeLines_NeverMatchesInsideHistoryBlock for the new
// function — parseNoteBody never even returns History's content into the
// set of blocks scanned, so a directive-shaped line swept into a
// permanent history entry must never re-route.
func TestExtractClaudeDirectives_NeverMatchesInsideHistoryBlock(t *testing.T) {
	body := applyNoteBodyEdit("Original description.", func(nb *noteBody) {
		nb.History = []string{
			"**2026-08-19 09:43 — changepw:** Root cause found, MR up.\n\n" +
				"@claude: fix the followups. Do a thorough research on kratos and the error codes etc",
		}
	})
	if got := extractClaudeDirectives(body); len(got) != 0 {
		t.Errorf("expected zero directives extracted from inside the history block, got %v\nbody:\n%s", got, body)
	}
}

// TestExtractClaudeDirectives_UnanchoredBody_FallsBackToSingleLine
// verifies a body with NO structured tn: anchors at all (parseNoteBody's
// ok=false case) — no reply callout exists to write the new fix's
// multi-line logic against, so this falls straight back to
// extractClaudeLines, unchanged from its historical behaviour. Matches
// this file's own dedup tests (TestClaudeLineDedup_*,
// TestWebhookClaudeDiffRoutesOncePerHashChange), which deliberately
// construct minimal unanchored payloads with an "@claude:" line followed
// by unrelated trailing content in the same body, and require that
// trailing content NOT be swallowed.
func TestExtractClaudeDirectives_UnanchoredBody_FallsBackToSingleLine(t *testing.T) {
	body := "Just a plain unstructured note.\n\n@claude: do the thing\n\nunrelated trailing text must not be swallowed."
	got := extractClaudeDirectives(body)
	want := []string{"do the thing"}
	if len(got) != 1 || got[0] != want[0] {
		t.Fatalf("extractClaudeDirectives(unanchored body) = %#v, want %#v", got, want)
	}
}

// TestRouteTaskNotification_HistoryEmbeddedClaudeLineNeverReassigns
// reproduces the full live incident end-to-end: a reply already routed and
// logged to history must not cause repeated "Task assigned:"-adjacent
// dispatch or repeated needs-input/review auto-flips on later
// observations of the SAME (unchanged in the relevant part) task, even
// across multiple webhook posts simulating the reconciler's repeated
// passes.
func TestRouteTaskNotification_HistoryEmbeddedClaudeLineNeverReassigns(t *testing.T) {
	srv, ts := newTestServer(t)
	register(t, ts, "agent-history", "proj-history")

	details := applyNoteBodyEdit("Original description.", func(nb *noteBody) {
		nb.History = []string{
			"**2026-08-19 09:43 — changepw:** Root cause found.\n\n" +
				"@claude: fix the followups. Do a thorough research on kratos and the error codes etc",
		}
	})

	task := webhookTask{
		Path: "Tasks/History.md", Title: "History", Status: "review",
		Tags: []string{"claude"}, Projects: []string{"proj-history"}, Details: details,
	}

	// Multiple observations of the SAME unchanged body — simulating
	// repeated reconciler/webhook passes, exactly what happened live.
	srv.routeTaskNotification(task)
	srv.routeTaskNotification(task)
	srv.routeTaskNotification(task)

	if msgs := queuedMessagesTo(srv, "agent-history"); len(msgs) != 0 {
		t.Fatalf("expected zero messages routed from the history-embedded line, got %+v", msgs)
	}
}

// TestClaudeLineDedup_SurvivesReindent verifies the line-level dedup key is
// the EXTRACTED text, not the raw (possibly quote-prefixed) line: a reply
// already routed must not re-route just because its surrounding
// blockquote depth changes (e.g. the user's reply gets nested one level
// deeper by further Obsidian editing) — same extracted text, same hash.
func TestClaudeLineDedup_SurvivesReindent(t *testing.T) {
	srv, ts := newTestServer(t)
	register(t, ts, "agent-reindent", "proj-reindent")

	post := func(details string) {
		payload := map[string]any{
			"event": "task.updated",
			"data": map[string]any{
				"task": map[string]any{
					"path": "Tasks/Reindent.md", "title": "Reindent",
					"details": details, "projects": []string{"proj-reindent"},
				},
			},
		}
		resp := mustPost(t, ts.URL+"/webhooks/tasknotes", payload)
		resp.Body.Close()
	}

	post("> @claude: please check the logs")
	msgs := queuedMessagesTo(srv, "agent-reindent")
	if len(msgs) != 1 || msgs[0].Text != "please check the logs" {
		t.Fatalf("expected exactly 1 routed line after the first post, got %+v", msgs)
	}

	// Re-indented one level deeper: different raw line, same extracted
	// text — the details hash also changes, so this exercises the actual
	// hash-short-circuit-miss -> line-level-dedup path, not just an
	// unchanged-details no-op.
	post("> > @claude: please check the logs")
	msgs = queuedMessagesTo(srv, "agent-reindent")
	if len(msgs) != 1 {
		t.Fatalf("expected still exactly 1 routed line after re-indenting (same extracted text), got %d: %+v", len(msgs), msgs)
	}
}

// TestWebhookClaudeDiffRoutesOncePerHashChange verifies @claude: lines in
// details are routed only when the details hash actually changes, and all
// matching lines are routed on each change.
func TestWebhookClaudeDiffRoutesOncePerHashChange(t *testing.T) {
	_, ts := newTestServer(t)
	register(t, ts, "agent-4", "proj-d")

	post := func(details string) {
		payload := map[string]any{
			"event": "task.updated",
			"data": map[string]any{
				"path":     "Tasks/Bar.md",
				"title":    "Bar",
				"projects": []string{"proj-d"},
				"details":  details,
			},
		}
		resp := mustPost(t, ts.URL+"/webhooks/tasknotes", payload)
		resp.Body.Close()
	}

	post("some notes\n@claude: please check the logs\nmore notes")
	post("some notes\n@claude: please check the logs\nmore notes")              // unchanged, no re-route
	post("some notes\n@claude: please check the logs\n@claude: also run tests") // changed: only the new line routes (line-level dedup)

	msgResp, err := http.Get(ts.URL + "/messages?to=agent-4&status=queued")
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		Messages []*Message `json:"messages"`
	}
	decodeInto(t, msgResp, &out)

	if len(out.Messages) != 2 {
		t.Fatalf("expected 1 message from the first post + 1 new message from the third post (not a re-route of the still-present first line), got %d: %+v", len(out.Messages), out.Messages)
	}
	texts := map[string]bool{}
	for _, m := range out.Messages {
		texts[m.Text] = true
	}
	for _, want := range []string{"please check the logs", "also run tests"} {
		if !texts[want] {
			t.Errorf("expected a routed message with text %q, got %+v", want, out.Messages)
		}
	}
}

// TestExtractWebhookTask_DetailsKnown verifies DetailsKnown is derived from
// whether the raw JSON payload actually contains a "details" key —
// regardless of that key's value — not from whether the resulting Go
// string happens to be empty. json.Unmarshal alone can't tell "absent"
// from "present and empty"; hasJSONKey can.
func TestExtractWebhookTask_DetailsKnown(t *testing.T) {
	t.Run("details present and non-empty", func(t *testing.T) {
		wt, ok := extractWebhookTask(json.RawMessage(`{"path":"Tasks/A.md","title":"A","details":"some text"}`))
		if !ok || !wt.DetailsKnown || wt.Details != "some text" {
			t.Errorf("got ok=%v DetailsKnown=%v Details=%q", ok, wt.DetailsKnown, wt.Details)
		}
	})
	t.Run("details present and empty", func(t *testing.T) {
		wt, ok := extractWebhookTask(json.RawMessage(`{"path":"Tasks/A.md","title":"A","details":""}`))
		if !ok || !wt.DetailsKnown || wt.Details != "" {
			t.Errorf("got ok=%v DetailsKnown=%v Details=%q", ok, wt.DetailsKnown, wt.Details)
		}
	})
	t.Run("details key absent", func(t *testing.T) {
		wt, ok := extractWebhookTask(json.RawMessage(`{"path":"Tasks/A.md","title":"A"}`))
		if !ok || wt.DetailsKnown || wt.Details != "" {
			t.Errorf("expected DetailsKnown=false for an absent key, got ok=%v DetailsKnown=%v Details=%q", ok, wt.DetailsKnown, wt.Details)
		}
	})
	t.Run("nested under task key, details absent", func(t *testing.T) {
		wt, ok := extractWebhookTask(json.RawMessage(`{"task":{"path":"Tasks/A.md","title":"A"},"customProperties":{"claude-project":"x"}}`))
		if !ok || wt.DetailsKnown {
			t.Errorf("expected DetailsKnown=false for a nested payload omitting details, got ok=%v DetailsKnown=%v", ok, wt.DetailsKnown)
		}
	})
	t.Run("nested under task key, details present", func(t *testing.T) {
		wt, ok := extractWebhookTask(json.RawMessage(`{"task":{"path":"Tasks/A.md","title":"A","details":"x"}}`))
		if !ok || !wt.DetailsKnown {
			t.Errorf("expected DetailsKnown=true for a nested payload including details, got ok=%v DetailsKnown=%v", ok, wt.DetailsKnown)
		}
	})
}

// TestTaskToWebhookTask_AlwaysDetailsKnown verifies taskToWebhookTask
// (the reconciler's GetTask-sourced conversion) always sets
// DetailsKnown=true — its one caller always passes a live-fetched Task,
// which always carries the real, current body, even when that body is
// genuinely empty.
func TestTaskToWebhookTask_AlwaysDetailsKnown(t *testing.T) {
	wt := taskToWebhookTask(Task{Path: "Tasks/A.md", Details: ""})
	if !wt.DetailsKnown {
		t.Error("expected taskToWebhookTask to always set DetailsKnown=true (GetTask-sourced, real body)")
	}
}

// TestWebhookClaudeDiff_UnconfirmedEmptyObservationNeverWipesDedup is the
// regression test for the live dedup-poisoning incident: a webhook payload
// that omits "details" entirely (e.g. a partial/status-only update — the
// exact shape a `tn status`/`tn done` call produces, and structurally the
// same risk as a query-sourced task, which POST /api/tasks/query never
// returns details for at all) must never be treated as "content changed to
// nothing". Before the fix, this wiped routedClaudeLines for the path; the
// NEXT confirmed-real observation of the SAME unchanged @claude line then
// saw "changed, nothing previously routed" and re-dispatched it —
// confirmed live across 20+ tasks (taskDetailsHash == sha256("")).
func TestWebhookClaudeDiff_UnconfirmedEmptyObservationNeverWipesDedup(t *testing.T) {
	srv, ts := newTestServer(t)
	register(t, ts, "agent-empty", "proj-empty")
	const path = "Tasks/Empty.md"

	postWithDetails := func(details string) {
		payload := map[string]any{
			"event": "task.updated",
			"data": map[string]any{
				"path": path, "title": "Empty", "projects": []string{"proj-empty"}, "details": details,
			},
		}
		resp := mustPost(t, ts.URL+"/webhooks/tasknotes", payload)
		resp.Body.Close()
	}
	postWithoutDetailsKey := func() {
		// Deliberately no "details" key at all.
		payload := map[string]any{
			"event": "task.updated",
			"data":  map[string]any{"path": path, "title": "Empty", "projects": []string{"proj-empty"}},
		}
		resp := mustPost(t, ts.URL+"/webhooks/tasknotes", payload)
		resp.Body.Close()
	}

	postWithDetails("@claude: please check the logs")
	msgs := queuedMessagesTo(srv, "agent-empty")
	if len(msgs) != 1 {
		t.Fatalf("expected exactly 1 dispatch from the real observation, got %+v", msgs)
	}

	srv.mu.Lock()
	hashBefore, hashKnownBefore := srv.state.TaskDetailsHash[path]
	routedBefore := append([]string(nil), srv.state.RoutedClaudeLines[path]...)
	srv.mu.Unlock()
	if !hashKnownBefore || len(routedBefore) != 1 {
		t.Fatalf("expected dedup state seeded after the real observation, hashKnown=%v routed=%v", hashKnownBefore, routedBefore)
	}

	postWithoutDetailsKey()

	srv.mu.Lock()
	hashAfter, hashKnownAfter := srv.state.TaskDetailsHash[path]
	routedAfter := srv.state.RoutedClaudeLines[path]
	srv.mu.Unlock()
	if !hashKnownAfter || hashAfter != hashBefore {
		t.Errorf("expected taskDetailsHash unchanged by the unconfirmed-empty observation, before=%q after=%q (known=%v)", hashBefore, hashAfter, hashKnownAfter)
	}
	if len(routedAfter) != 1 || routedAfter[0] != routedBefore[0] {
		t.Errorf("expected routedClaudeLines unchanged by the unconfirmed-empty observation, before=%v after=%v", routedBefore, routedAfter)
	}
	if msgs := queuedMessagesTo(srv, "agent-empty"); len(msgs) != 1 {
		t.Errorf("expected no new dispatch from the unconfirmed-empty observation, got %d: %+v", len(msgs), msgs)
	}
}

// TestWebhookClaudeDiff_AlternatingRealAndEmptyObservationsDispatchOnce is
// the exact live repro: a still-present, unchanging @claude: line observed
// through an alternating sequence of real (details known) and
// unconfirmed-empty (details omitted) observations — mirroring the
// webhook/reconciler-vs-partial-update alternation that produced the
// live ~10-minute redispatch cadence — must dispatch exactly once total.
func TestWebhookClaudeDiff_AlternatingRealAndEmptyObservationsDispatchOnce(t *testing.T) {
	srv, ts := newTestServer(t)
	register(t, ts, "agent-alt", "proj-alt")
	const path = "Tasks/Alt.md"

	postWithDetails := func(details string) {
		payload := map[string]any{
			"event": "task.updated",
			"data": map[string]any{
				"path": path, "title": "Alt", "projects": []string{"proj-alt"}, "details": details,
			},
		}
		resp := mustPost(t, ts.URL+"/webhooks/tasknotes", payload)
		resp.Body.Close()
	}
	postWithoutDetailsKey := func() {
		payload := map[string]any{
			"event": "task.updated",
			"data":  map[string]any{"path": path, "title": "Alt", "projects": []string{"proj-alt"}},
		}
		resp := mustPost(t, ts.URL+"/webhooks/tasknotes", payload)
		resp.Body.Close()
	}

	const line = "@claude: fix the followups"
	postWithDetails(line)
	postWithoutDetailsKey()
	postWithDetails(line)
	postWithoutDetailsKey()
	postWithDetails(line)

	if msgs := queuedMessagesTo(srv, "agent-alt"); len(msgs) != 1 {
		t.Fatalf("expected exactly 1 dispatch total across alternating real/empty/real observations, got %d: %+v", len(msgs), msgs)
	}
}

// TestWebhookClaudeDiff_ConfirmedEmptyDetailsStillUpdatesState verifies the
// fix doesn't overcorrect: a REAL observation confirming a task's
// description is now genuinely empty (the "details" key IS present, with
// an explicit "") must still update state — the task really has no
// @claude lines anymore, and if the line comes back later, it's a fresh
// episode that routes again normally.
func TestWebhookClaudeDiff_ConfirmedEmptyDetailsStillUpdatesState(t *testing.T) {
	srv, ts := newTestServer(t)
	register(t, ts, "agent-realempty", "proj-realempty")
	const path = "Tasks/RealEmpty.md"

	post := func(details string) {
		payload := map[string]any{
			"event": "task.updated",
			"data": map[string]any{
				"path": path, "title": "RealEmpty", "projects": []string{"proj-realempty"}, "details": details,
			},
		}
		resp := mustPost(t, ts.URL+"/webhooks/tasknotes", payload)
		resp.Body.Close()
	}

	post("@claude: please check the logs")
	if msgs := queuedMessagesTo(srv, "agent-realempty"); len(msgs) != 1 {
		t.Fatalf("expected exactly 1 dispatch from the real observation, got %+v", msgs)
	}

	post("") // confirmed-empty: "details" key present, value ""

	srv.mu.Lock()
	hash, hashKnown := srv.state.TaskDetailsHash[path]
	routed := srv.state.RoutedClaudeLines[path]
	srv.mu.Unlock()
	if wantHash := sha256Hex(""); !hashKnown || hash != wantHash {
		t.Errorf("expected taskDetailsHash updated to sha256(\"\"), got %q (known=%v)", hash, hashKnown)
	}
	if len(routed) != 0 {
		t.Errorf("expected routedClaudeLines cleared for a genuinely emptied description, got %v", routed)
	}

	post("@claude: please check the logs") // comes back: fresh episode, routes again
	if msgs := queuedMessagesTo(srv, "agent-realempty"); len(msgs) != 2 {
		t.Errorf("expected the line to route again once confirmed non-empty again, got %d: %+v", len(msgs), msgs)
	}
}

// TestDiffClaudeLinesLocked_MigrationPathProtectedFromUnconfirmedEmpty
// covers team-lead's flagged concern: the migration-seed branch (a stored
// hash predating line-level dedup, no routedClaudeLines entry yet) must
// also be protected — a stored hash of sha256("") could itself be a
// product of the poisoning bug, and an unconfirmed-empty observation
// matching it must not be allowed to "legitimately" seed an empty routed
// set, which would look identical to a correct migration.
func TestDiffClaudeLinesLocked_MigrationPathProtectedFromUnconfirmedEmpty(t *testing.T) {
	srv, _ := newTestServer(t)
	const path = "Tasks/Migrate.md"

	srv.mu.Lock()
	srv.state.TaskDetailsHash[path] = sha256Hex("")
	srv.mu.Unlock()

	got := srv.diffClaudeLinesLocked(path, "", false)
	if len(got) != 0 {
		t.Fatalf("expected no lines routed, got %v", got)
	}

	srv.mu.Lock()
	_, hasRoutedSet := srv.state.RoutedClaudeLines[path]
	srv.mu.Unlock()
	if hasRoutedSet {
		t.Error("expected the migration branch to NOT seed a routedClaudeLines entry from an unconfirmed-empty observation")
	}
}

// TestClaudeLineDedup_SameLineNeverRoutesTwice verifies a @claude: line that
// stays present across many unrelated detail edits (each changing the
// overall details hash, e.g. via repeated tn note appends) is only ever
// routed once — the exact "5x delivery" bug line-level dedup replaced
// hash-level dedup to fix.
func TestClaudeLineDedup_SameLineNeverRoutesTwice(t *testing.T) {
	srv, ts := newTestServer(t)
	register(t, ts, "agent-samedup", "proj-samedup")

	postDetails := func(details string) {
		payload := map[string]any{
			"event": "task.updated",
			"data": map[string]any{
				"path":     "Tasks/SameDup.md",
				"title":    "SameDup",
				"projects": []string{"proj-samedup"},
				"details":  details,
			},
		}
		resp := mustPost(t, ts.URL+"/webhooks/tasknotes", payload)
		resp.Body.Close()
	}

	postDetails("@claude: please check the logs")
	for i := 0; i < 5; i++ {
		postDetails(fmt.Sprintf("@claude: please check the logs\n\nnote %d: did some work", i))
	}

	msgs := queuedMessagesTo(srv, "agent-samedup")
	if len(msgs) != 1 {
		t.Fatalf("expected the line routed exactly once across 6 posts, got %d: %+v", len(msgs), msgs)
	}
	if msgs[0].Text != "please check the logs" {
		t.Errorf("got %q", msgs[0].Text)
	}
}

// TestClaudeLineDedup_NewLineAddedLaterRoutesOnce verifies a genuinely new
// @claude: line added in a later edit routes exactly once, without
// re-routing the line that was already there.
func TestClaudeLineDedup_NewLineAddedLaterRoutesOnce(t *testing.T) {
	srv, ts := newTestServer(t)
	register(t, ts, "agent-newline", "proj-newline")

	postDetails := func(details string) {
		payload := map[string]any{
			"event": "task.updated",
			"data": map[string]any{
				"path":     "Tasks/NewLine.md",
				"title":    "NewLine",
				"projects": []string{"proj-newline"},
				"details":  details,
			},
		}
		resp := mustPost(t, ts.URL+"/webhooks/tasknotes", payload)
		resp.Body.Close()
	}

	postDetails("@claude: first question")
	postDetails("@claude: first question\n@claude: second question")
	postDetails("@claude: first question\n@claude: second question\nsome unrelated note") // hash changes again, no new lines

	msgs := queuedMessagesTo(srv, "agent-newline")
	if len(msgs) != 2 {
		t.Fatalf("expected exactly 2 total messages (each line routed once), got %d: %+v", len(msgs), msgs)
	}
	counts := map[string]int{}
	for _, m := range msgs {
		counts[m.Text]++
	}
	if counts["first question"] != 1 || counts["second question"] != 1 {
		t.Errorf("expected each line routed exactly once, got %+v", counts)
	}
}

// TestClaudeLineDedup_RemovedThenReAddedRoutesAgain verifies a line that
// disappears from details and later reappears with identical text is
// treated as a new occurrence and routes again.
func TestClaudeLineDedup_RemovedThenReAddedRoutesAgain(t *testing.T) {
	srv, ts := newTestServer(t)
	register(t, ts, "agent-readd", "proj-readd")

	postDetails := func(details string) {
		payload := map[string]any{
			"event": "task.updated",
			"data": map[string]any{
				"path":     "Tasks/Readd.md",
				"title":    "Readd",
				"projects": []string{"proj-readd"},
				"details":  details,
			},
		}
		resp := mustPost(t, ts.URL+"/webhooks/tasknotes", payload)
		resp.Body.Close()
	}

	postDetails("@claude: recurring question")
	postDetails("some notes, the line is gone now")
	postDetails("@claude: recurring question") // identical text, re-added

	msgs := queuedMessagesTo(srv, "agent-readd")
	if len(msgs) != 2 {
		t.Fatalf("expected the line to route once on first add and once again on re-add, got %d: %+v", len(msgs), msgs)
	}
	for _, m := range msgs {
		if m.Text != "recurring question" {
			t.Errorf("unexpected message text %q", m.Text)
		}
	}
}

// TestClaudeLineDedup_MigrationSeedsWithoutRouting verifies the upgrade
// path: a task whose TaskDetailsHash entry predates line-level dedup (no
// RoutedClaudeLines entry yet) has its current lines seeded as
// already-routed on the first pass WITHOUT dispatching them, and a
// genuinely new line added afterward still routes normally.
func TestClaudeLineDedup_MigrationSeedsWithoutRouting(t *testing.T) {
	srv, ts := newTestServer(t)
	register(t, ts, "agent-migrate", "proj-migrate")

	const path = "Tasks/Migrate.md"
	details := "@claude: pre-existing question\nsome other notes"
	sum := sha256.Sum256([]byte(details))
	hash := hex.EncodeToString(sum[:])

	// Simulate a task tracked before line-level dedup existed: a
	// taskDetailsHash entry with no matching routedClaudeLines entry.
	srv.mu.Lock()
	srv.state.TaskDetailsHash[path] = hash
	srv.mu.Unlock()

	postDetails := func(d string) {
		payload := map[string]any{
			"event": "task.updated",
			"data": map[string]any{
				"path": path, "title": "Migrate", "projects": []string{"proj-migrate"}, "details": d,
			},
		}
		resp := mustPost(t, ts.URL+"/webhooks/tasknotes", payload)
		resp.Body.Close()
	}

	postDetails(details) // same hash: should seed, not route
	if msgs := queuedMessagesTo(srv, "agent-migrate"); len(msgs) != 0 {
		t.Fatalf("expected the pre-existing line to be seeded without routing, got %+v", msgs)
	}

	srv.mu.Lock()
	seeded, ok := srv.state.RoutedClaudeLines[path]
	srv.mu.Unlock()
	if !ok || len(seeded) != 1 {
		t.Fatalf("expected routedClaudeLines seeded with 1 hash, got %+v (ok=%v)", seeded, ok)
	}

	// A genuinely new line afterward should still route (only the new one).
	postDetails(details + "\n@claude: a brand new question")

	msgs := queuedMessagesTo(srv, "agent-migrate")
	if len(msgs) != 1 {
		t.Fatalf("expected exactly the new line to route, got %d: %+v", len(msgs), msgs)
	}
	if msgs[0].Text != "a brand new question" {
		t.Errorf("got %q", msgs[0].Text)
	}
}

// TestNoLiveAgentSendQueuedWarning verifies sending to a project with no
// alive agent and no autoSpawn config returns 202 with a warning, and queues
// to orchestrator-<project>.
func TestNoLiveAgentSendQueuedWarning(t *testing.T) {
	_, ts := newTestServer(t)

	resp := mustPost(t, ts.URL+"/messages", map[string]any{"project": "proj-e", "text": "hello"})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("expected 202, got %d", resp.StatusCode)
	}
	var out struct {
		Queued  bool    `json:"queued"`
		Warning string  `json:"warning"`
		Message Message `json:"message"`
	}
	decodeInto(t, resp, &out)

	if !out.Queued {
		t.Error("expected queued=true")
	}
	if out.Warning != "no live agent" {
		t.Errorf("expected warning %q, got %q", "no live agent", out.Warning)
	}
	if out.Message.To != "orchestrator-proj-e" {
		t.Errorf("expected message queued to orchestrator-proj-e, got %q", out.Message.To)
	}
}

// TestDispatchMessage_NewAssignmentSkipsDrainingAgent_TaskScopedReachesOwner
// covers the generations & drain routing split (SPEC-generations.md): a
// brand-new assignment (no owner yet) must skip a draining-only agent and
// fall to the logical queue, while a task-scoped follow-up for a task that
// draining agent already owns must still reach it directly.
func TestDispatchMessage_NewAssignmentSkipsDrainingAgent_TaskScopedReachesOwner(t *testing.T) {
	srv, ts := newTestServer(t)
	register(t, ts, "orchestrator-myapp-g1", "myapp")

	// orchestrator-myapp-g1 owns Tasks/Foo.md: an acked "Task assigned:" message.
	ackedAt := time.Now().Add(-time.Minute)
	srv.mu.Lock()
	srv.state.Messages = append(srv.state.Messages, &Message{
		ID: "assign-1", To: "orchestrator-myapp-g1", TaskPath: "Tasks/Foo.md",
		Text: "Task assigned: Foo (Tasks/Foo.md)", Status: "acked",
		CreatedAt: ackedAt, AckedAt: &ackedAt,
	})
	srv.mu.Unlock()

	resp := mustPost(t, ts.URL+"/agents/drain", map[string]any{"name": "orchestrator-myapp-g1"})
	resp.Body.Close()

	// A brand-new task (no owner yet) must NOT go to the draining agent.
	newMsg, status, _ := srv.dispatchMessage(sendRequest{Project: "myapp", TaskPath: "Tasks/Bar.md", Text: "Task assigned: Bar (Tasks/Bar.md)"})
	if newMsg.To != "orchestrator-myapp" {
		t.Errorf("expected a brand-new assignment to skip the draining agent and queue to the logical name, got %q", newMsg.To)
	}
	if status != http.StatusAccepted {
		t.Errorf("expected 202 (queued, no accepting agent), got %d", status)
	}

	// A task-scoped follow-up on the OWNED task must still reach the
	// draining owner directly.
	followUp, status2, _ := srv.dispatchMessage(sendRequest{Project: "myapp", TaskPath: "Tasks/Foo.md", Text: "@claude: here's more info"})
	if followUp.To != "orchestrator-myapp-g1" {
		t.Errorf("expected the task-scoped follow-up to reach the draining owner, got %q", followUp.To)
	}
	if status2 != http.StatusOK {
		t.Errorf("expected 200 (delivered directly to the owner), got %d", status2)
	}
}

// TestRouteTaskNotification_SpawnsFreshGenerationWhenOnlyDrainingAlive
// verifies the spawn trigger fires even though a generation IS alive for
// the project, as long as it's draining — draining-only must still spawn a
// fresh generation to take new work (SPEC-generations.md).
func TestRouteTaskNotification_SpawnsFreshGenerationWhenOnlyDrainingAlive(t *testing.T) {
	var callCount int
	cfg := ServeConfig{
		Port:     0,
		Projects: map[string]ProjectConfig{"myapp": {AutoSpawn: true, Cwd: "/repos/myapp"}},
	}
	ts := newTestServerWithConfig(t, cfg, func(project, cwd string, env map[string]string) error {
		callCount++
		return nil
	})
	register(t, ts, "orchestrator-myapp-g1", "myapp")
	drainResp := mustPost(t, ts.URL+"/agents/drain", map[string]any{"name": "orchestrator-myapp-g1"})
	drainResp.Body.Close()

	payload := map[string]any{
		"event": "task.created",
		"data": map[string]any{
			"task": map[string]any{
				"path":     "Tasks/New.md",
				"title":    "New",
				"status":   "open",
				"tags":     []string{"claude"},
				"projects": []string{"myapp"},
			},
		},
	}
	resp := mustPost(t, ts.URL+"/webhooks/tasknotes", payload)
	resp.Body.Close()

	if callCount != 1 {
		t.Errorf("expected spawnFunc called once for a draining-only project, got %d calls", callCount)
	}
}

// TestOrchestratorStartupUsesInjectedSpawnFunc verifies that when autoSpawn
// is enabled and no alive agent exists for a project, the injected
// spawnFunc is called with the right project and cwd (never touching real
// tmux).
func TestOrchestratorStartupUsesInjectedSpawnFunc(t *testing.T) {
	var gotProject, gotCwd string
	var callCount int
	cfg := ServeConfig{
		Port: 0,
		Projects: map[string]ProjectConfig{
			"proj-f": {AutoSpawn: true, Cwd: "/tmp/proj-f-repo"},
		},
	}
	ts := newTestServerWithConfig(t, cfg, func(project, cwd string, env map[string]string) error {
		callCount++
		gotProject, gotCwd = project, cwd
		return nil
	})

	resp := mustPost(t, ts.URL+"/messages", map[string]any{"project": "proj-f", "text": "hello"})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("expected 202, got %d", resp.StatusCode)
	}

	if callCount != 1 {
		t.Fatalf("expected spawnFunc to be called once, got %d", callCount)
	}
	if gotProject != "proj-f" || gotCwd != "/tmp/proj-f-repo" {
		t.Errorf("spawnFunc called with (%q, %q), want (%q, %q)", gotProject, gotCwd, "proj-f", "/tmp/proj-f-repo")
	}

	var out struct {
		Message Message `json:"message"`
	}
	decodeInto(t, resp, &out)
	if out.Message.To != "orchestrator-proj-f" {
		t.Errorf("expected message queued to orchestrator-proj-f, got %q", out.Message.To)
	}
}

// TestProjectConfigForCaseInsensitive is a direct unit test of the
// case-insensitive serve.json project-key lookup: it must match regardless
// of casing on either side, and return a zero-value ProjectConfig (autoSpawn
// false) for a genuinely unknown slug.
func TestProjectConfigForCaseInsensitive(t *testing.T) {
	srv, _ := newTestServer(t)
	srv.config.Projects = map[string]ProjectConfig{"Myapp": {AutoSpawn: true, Cwd: "/repos/myapp"}}

	cfg := srv.projectConfigFor("myapp")
	if !cfg.AutoSpawn || cfg.Cwd != "/repos/myapp" {
		t.Errorf("expected case-insensitive match against config key %q, got %+v", "Myapp", cfg)
	}

	cfg2 := srv.projectConfigFor("nonexistent")
	if cfg2.AutoSpawn || cfg2.Cwd != "" {
		t.Errorf("expected zero-value ProjectConfig for an unknown slug, got %+v", cfg2)
	}
}

// TestWebhookCaseInsensitiveProjectSpawn verifies a task's project wikilink
// ("[[Myapp]]", normalized+lowercased to "myapp") matches a serve.json config
// key "myapp" and spawns/routes using that lowercased slug, so orchestrator
// name and tmux session are consistent regardless of how the task spelled
// the project.
func TestWebhookCaseInsensitiveProjectSpawn(t *testing.T) {
	var gotProject, gotCwd string
	cfg := ServeConfig{
		Port: 0,
		Projects: map[string]ProjectConfig{
			"myapp": {AutoSpawn: true, Cwd: "/repos/myapp"},
		},
	}
	ts := newTestServerWithConfig(t, cfg, func(project, cwd string, env map[string]string) error {
		gotProject, gotCwd = project, cwd
		return nil
	})

	payload := map[string]any{
		"event": "task.created",
		"data": map[string]any{
			"task": map[string]any{
				"path":     "Tasks/Myapp.md",
				"title":    "Myapp task",
				"tags":     []string{"claude"},
				"projects": []string{"[[Myapp]]"},
			},
		},
	}
	resp := mustPost(t, ts.URL+"/webhooks/tasknotes", payload)
	resp.Body.Close()

	if gotProject != "myapp" || gotCwd != "/repos/myapp" {
		t.Errorf("spawnFunc called with (%q, %q), want (%q, %q)", gotProject, gotCwd, "myapp", "/repos/myapp")
	}

	msgResp, err := http.Get(ts.URL + "/messages?to=orchestrator-myapp&status=queued")
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		Messages []*Message `json:"messages"`
	}
	decodeInto(t, msgResp, &out)
	if len(out.Messages) != 1 {
		t.Fatalf("expected 1 message queued to orchestrator-myapp, got %+v", out.Messages)
	}
}

// TestWebhookClaudeProjectWikilinkNormalized verifies a claude-project
// custom property value that's itself a wikilink (TaskNotes user-field
// autosuggestion inserts note links) is normalized the same way as the
// projects[] fallback — brackets/path/.md stripped, lowercased — then
// matched against the serve.json config and used for the spawn.
func TestWebhookClaudeProjectWikilinkNormalized(t *testing.T) {
	var gotProject string
	cfg := ServeConfig{
		Port: 0,
		Projects: map[string]ProjectConfig{
			"myapp": {AutoSpawn: true, Cwd: "/repos/myapp"},
		},
	}
	ts := newTestServerWithConfig(t, cfg, func(project, cwd string, env map[string]string) error {
		gotProject = project
		return nil
	})

	payload := map[string]any{
		"event": "task.created",
		"data": map[string]any{
			"task": map[string]any{
				"path":  "Tasks/Myapp2.md",
				"title": "Myapp task 2",
				"tags":  []string{"claude"},
				"customProperties": map[string]any{
					"claude-project": "[[Repos/Myapp]]",
				},
			},
		},
	}
	resp := mustPost(t, ts.URL+"/webhooks/tasknotes", payload)
	resp.Body.Close()

	if gotProject != "myapp" {
		t.Errorf("expected spawnFunc called with %q, got %q", "myapp", gotProject)
	}

	msgResp, err := http.Get(ts.URL + "/messages?to=orchestrator-myapp&status=queued")
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		Messages []*Message `json:"messages"`
	}
	decodeInto(t, msgResp, &out)
	if len(out.Messages) != 1 {
		t.Fatalf("expected 1 message queued to orchestrator-myapp, got %+v", out.Messages)
	}
	if out.Messages[0].Project != "myapp" {
		t.Errorf("expected Message.Project %q, got %q", "myapp", out.Messages[0].Project)
	}
}

// TestBuildOrchestratorPrompt verifies the bootstrap prompt points spawned
// orchestrators at the repo's ORCHESTRATOR.md, carries the project slug,
// and — per SPEC-generations.md — carries the generation's agent name and
// tmux session literally as "register with the bridge as <agentName>; your
// tmux session is <tmuxSession>".
func TestBuildOrchestratorPrompt(t *testing.T) {
	prompt := buildOrchestratorPrompt("procfit", "orchestrator-procfit-g12345", "tn-procfit-g12345", "/opt/tn/ORCHESTRATOR.md")

	if !strings.Contains(prompt, "ORCHESTRATOR.md") {
		t.Errorf("expected prompt to reference ORCHESTRATOR.md, got %q", prompt)
	}
	if !strings.Contains(prompt, "project procfit") {
		t.Errorf("expected prompt to name the project, got %q", prompt)
	}
	if !strings.Contains(prompt, "project slug is procfit") {
		t.Errorf("expected prompt to state the project slug, got %q", prompt)
	}
	if !strings.Contains(prompt, "register with the bridge as orchestrator-procfit-g12345; your tmux session is tn-procfit-g12345") {
		t.Errorf("expected prompt to carry the generation's agent name and tmux session literally, got %q", prompt)
	}
	if strings.Contains(prompt, "tasknotes skill") {
		t.Errorf("expected prompt to no longer reference the global tasknotes skill, got %q", prompt)
	}
}

// TestResolveOrchestratorDoc covers resolveOrchestratorDoc's precedence
// order: env TN_ORCHESTRATOR_DOC > serve.json's orchestratorDoc >
// ~/.config/tn/ORCHESTRATOR.md > the daemon binary's own directory >
// ./ORCHESTRATOR.md relative to cwd — and the not-found fallback.
func TestResolveOrchestratorDoc(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("TN_ORCHESTRATOR_DOC", "")

	// Isolate from this repo's own ORCHESTRATOR.md, which otherwise
	// satisfies the ./ORCHESTRATOR.md-relative-to-cwd candidate whenever
	// tests run from the repo root.
	origWD, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	cwd := t.TempDir()
	if err := os.Chdir(cwd); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(origWD) })

	// Nothing configured and nothing on disk: falls back to the
	// config-dir path with found=false.
	wantFallback := filepath.Join(home, ".config", "tn", "ORCHESTRATOR.md")
	path, found := resolveOrchestratorDoc(ServeConfig{})
	if found {
		t.Errorf("expected found=false with nothing on disk, got path %q", path)
	}
	if path != wantFallback {
		t.Errorf("expected fallback path %q, got %q", wantFallback, path)
	}

	// ~/.config/tn/ORCHESTRATOR.md present: used when nothing higher in
	// precedence is set.
	if err := os.MkdirAll(filepath.Dir(wantFallback), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(wantFallback, []byte("contract"), 0o644); err != nil {
		t.Fatal(err)
	}
	path, found = resolveOrchestratorDoc(ServeConfig{})
	if !found || path != wantFallback {
		t.Errorf("expected config-dir path %q found, got %q found=%v", wantFallback, path, found)
	}

	// serve.json's orchestratorDoc outranks the config-dir default.
	fileDoc := filepath.Join(cwd, "from-file.md")
	if err := os.WriteFile(fileDoc, []byte("contract"), 0o644); err != nil {
		t.Fatal(err)
	}
	path, found = resolveOrchestratorDoc(ServeConfig{OrchestratorDoc: fileDoc})
	if !found || path != fileDoc {
		t.Errorf("expected serve.json path %q to win, got %q found=%v", fileDoc, path, found)
	}

	// env TN_ORCHESTRATOR_DOC outranks serve.json's orchestratorDoc.
	envDoc := filepath.Join(cwd, "from-env.md")
	if err := os.WriteFile(envDoc, []byte("contract"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TN_ORCHESTRATOR_DOC", envDoc)
	path, found = resolveOrchestratorDoc(ServeConfig{OrchestratorDoc: fileDoc})
	if !found || path != envDoc {
		t.Errorf("expected env path %q to win over serve.json path %q, got %q found=%v", envDoc, fileDoc, path, found)
	}

	// A configured-but-missing env path is not silently skipped in favor
	// of a lower-precedence candidate that DOES exist — resolveOrchestratorDoc
	// only stat-checks in order and returns the first hit, so a set-but-
	// wrong env value still loses to serve.json/config-dir/cwd candidates
	// that exist. Confirm that fallthrough works too.
	t.Setenv("TN_ORCHESTRATOR_DOC", filepath.Join(cwd, "does-not-exist.md"))
	path, found = resolveOrchestratorDoc(ServeConfig{OrchestratorDoc: fileDoc})
	if !found || path != fileDoc {
		t.Errorf("expected fallthrough to serve.json path %q when env path is missing, got %q found=%v", fileDoc, path, found)
	}
}

// TestBuildLaunchCommand verifies the spawned session's env is marked
// TN_HEADLESS=1 and TN_AGENT_NAME=<agentName> (the statusline wrapper's key
// for POST /agents/context, see SPEC-generations.md), and the prompt is
// safely shell-quoted (including prompts containing single quotes, since
// the prompt text is not attacker-controlled but may still contain
// arbitrary punctuation).
func TestBuildLaunchCommand(t *testing.T) {
	cmd := buildLaunchCommand("/home/user/.local/bin/claude", "orchestrator-myapp-g1", "hello world", nil)

	if !strings.HasPrefix(cmd, "TN_HEADLESS=1 TN_AGENT_NAME='orchestrator-myapp-g1' ") {
		t.Fatalf("expected launch command to start with TN_HEADLESS=1 TN_AGENT_NAME=..., got %q", cmd)
	}
	want := "TN_HEADLESS=1 TN_AGENT_NAME='orchestrator-myapp-g1' /home/user/.local/bin/claude --permission-mode auto 'hello world'"
	if cmd != want {
		t.Errorf("got %q, want %q", cmd, want)
	}

	quoted := buildLaunchCommand("claude", "orchestrator-myapp-g1", "it's a test", nil)
	if !strings.Contains(quoted, `it'\''s a test`) {
		t.Errorf("expected embedded single quote to be shell-escaped, got %q", quoted)
	}
}

// TestBinaryInfoChanged covers the self-restart watcher's pure
// change-detection helper: identical stats, a size change, an mtime-only
// change, and the nil-handling edge cases.
func TestBinaryInfoChanged(t *testing.T) {
	dir := t.TempDir()

	pathA := filepath.Join(dir, "bin-a")
	if err := os.WriteFile(pathA, []byte("v1"), 0o755); err != nil {
		t.Fatal(err)
	}
	same1, err := os.Stat(pathA)
	if err != nil {
		t.Fatal(err)
	}
	same2, err := os.Stat(pathA)
	if err != nil {
		t.Fatal(err)
	}
	if binaryInfoChanged(same1, same2) {
		t.Error("expected no change for two stats of the same untouched file")
	}

	if err := os.WriteFile(pathA, []byte("v2-is-longer"), 0o755); err != nil {
		t.Fatal(err)
	}
	resized, err := os.Stat(pathA)
	if err != nil {
		t.Fatal(err)
	}
	if !binaryInfoChanged(same1, resized) {
		t.Error("expected change detected for a different size")
	}

	pathB := filepath.Join(dir, "bin-b")
	if err := os.WriteFile(pathB, []byte("v2-is-longer"), 0o755); err != nil {
		t.Fatal(err)
	}
	sameSize, err := os.Stat(pathB)
	if err != nil {
		t.Fatal(err)
	}
	future := time.Now().Add(time.Hour).Truncate(time.Second)
	if err := os.Chtimes(pathB, future, future); err != nil {
		t.Fatal(err)
	}
	touched, err := os.Stat(pathB)
	if err != nil {
		t.Fatal(err)
	}
	if !binaryInfoChanged(sameSize, touched) {
		t.Error("expected change detected for a different mtime with the same size")
	}

	if !binaryInfoChanged(nil, resized) {
		t.Error("expected change when only one side is nil")
	}
	if !binaryInfoChanged(resized, nil) {
		t.Error("expected change when only one side is nil")
	}
	var nilA, nilB os.FileInfo
	if binaryInfoChanged(nilA, nilB) {
		t.Error("expected no change when both sides are nil")
	}
}

// TestExitForSelfRestart_WaitsForInFlightMutation is the regression guard
// for a restart-durability gap: the self-restart watcher used to call
// os.Exit(0) directly from its own goroutine, with nothing stopping it from
// firing WHILE another goroutine was mid-mutation (e.g. an assignment
// marker set in memory a moment before saveLocked would have persisted
// it). exitForSelfRestart now acquires the same mutex every mutating
// handler holds for its own atomic Lock/mutate/save/Unlock cycle, so exit
// cannot fire until any in-flight mutation has fully completed and
// released the lock. Verified directly: hold s.mu (simulating an in-flight
// mutation), confirm exit does NOT fire while held, release it, confirm
// exit proceeds immediately once the lock is free.
func TestExitForSelfRestart_WaitsForInFlightMutation(t *testing.T) {
	srv, _ := newTestServer(t)

	srv.mu.Lock() // simulate a mutation currently in flight

	exited := make(chan struct{})
	go func() {
		srv.exitForSelfRestart(func(int) { close(exited) })
	}()

	select {
	case <-exited:
		t.Fatal("expected exit to block while a mutation holds the lock")
	case <-time.After(150 * time.Millisecond):
		// Still blocked, as expected.
	}

	srv.mu.Unlock() // the in-flight mutation completes

	select {
	case <-exited:
		// Expected: exit proceeds now that the lock is free.
	case <-time.After(2 * time.Second):
		t.Fatal("expected exit to proceed once the in-flight mutation released the lock")
	}
}

// newReconcilerTestServer builds a Server with a temp-dir state file, an
// injectable spawnFunc, and the given autoSpawn project config, without
// starting any background goroutines.
func newReconcilerTestServer(t *testing.T, projects map[string]ProjectConfig, spawnFunc func(project, cwd string, env map[string]string) error) *Server {
	t.Helper()
	dir := t.TempDir()
	cfg := ServeConfig{Port: 0, Projects: projects}
	return newServer(filepath.Join(dir, "state.json"), cfg, spawnFunc)
}

func seedQueuedOrchestratorMessage(srv *Server, to string) {
	srv.mu.Lock()
	srv.state.Messages = append(srv.state.Messages, &Message{
		ID: "msg-seed", To: to, Text: "Task assigned: X (Tasks/X.md)",
		Status: "queued", CreatedAt: time.Now(),
	})
	srv.mu.Unlock()
}

// TestReconcileSpawnsOnce_SpawnsWhenQueuedAndNoAliveAgent verifies the
// reconciler retries a spawn for an autoSpawn project that has a queued
// orchestrator message but no alive agent — the case where an earlier
// webhook-triggered spawn failed (or launchd hadn't finished restarting),
// and the assignment dedup then suppresses any retry via normal routing.
func TestReconcileSpawnsOnce_SpawnsWhenQueuedAndNoAliveAgent(t *testing.T) {
	var callCount int
	var gotSlug, gotCwd string
	srv := newReconcilerTestServer(t,
		map[string]ProjectConfig{"myapp": {AutoSpawn: true, Cwd: "/repos/myapp"}},
		func(project, cwd string, env map[string]string) error {
			callCount++
			gotSlug, gotCwd = project, cwd
			return nil
		})
	seedQueuedOrchestratorMessage(srv, "orchestrator-myapp")

	srv.reconcileSpawnsOnce()

	if callCount != 1 {
		t.Fatalf("expected spawnFunc called once, got %d", callCount)
	}
	if gotSlug != "myapp" || gotCwd != "/repos/myapp" {
		t.Errorf("spawnFunc called with (%q, %q), want (%q, %q)", gotSlug, gotCwd, "myapp", "/repos/myapp")
	}
}

// TestReconcileSpawnsOnce_SkipsWhenAliveAgentExists verifies no spawn is
// attempted when an alive agent already covers the project, even with a
// queued message pending.
func TestReconcileSpawnsOnce_SkipsWhenAliveAgentExists(t *testing.T) {
	var callCount int
	srv := newReconcilerTestServer(t,
		map[string]ProjectConfig{"myapp": {AutoSpawn: true, Cwd: "/repos/myapp"}},
		func(project, cwd string, env map[string]string) error {
			callCount++
			return nil
		})
	seedQueuedOrchestratorMessage(srv, "orchestrator-myapp")

	srv.mu.Lock()
	srv.state.Agents["orchestrator-myapp"] = &Agent{
		Name: "orchestrator-myapp", Project: "myapp", LastSeenAt: time.Now(), Accepting: true,
	}
	srv.mu.Unlock()

	srv.reconcileSpawnsOnce()

	if callCount != 0 {
		t.Errorf("expected spawnFunc not called when an alive agent exists, got %d calls", callCount)
	}
}

// TestReconcileSpawnsOnce_SpawnsWhenOnlyDrainingAgentAlive verifies the
// generations rule: a draining-only agent does NOT count as "alive" for
// spawn-suppression purposes — the reconciler must still fire so a fresh
// generation can pick up the stranded queued work.
func TestReconcileSpawnsOnce_SpawnsWhenOnlyDrainingAgentAlive(t *testing.T) {
	var callCount int
	srv := newReconcilerTestServer(t,
		map[string]ProjectConfig{"myapp": {AutoSpawn: true, Cwd: "/repos/myapp"}},
		func(project, cwd string, env map[string]string) error {
			callCount++
			return nil
		})
	seedQueuedOrchestratorMessage(srv, "orchestrator-myapp")

	srv.mu.Lock()
	srv.state.Agents["orchestrator-myapp-g1"] = &Agent{
		Name: "orchestrator-myapp-g1", Project: "myapp", LastSeenAt: time.Now(), Accepting: false,
	}
	srv.mu.Unlock()

	srv.reconcileSpawnsOnce()

	if callCount != 1 {
		t.Errorf("expected spawnFunc called once when only a draining agent is alive, got %d calls", callCount)
	}
}

// TestReconcileSpawnsOnce_SkipsWhenNoQueuedMessages verifies no spawn is
// attempted for an autoSpawn project with nothing queued for its
// orchestrator.
func TestReconcileSpawnsOnce_SkipsWhenNoQueuedMessages(t *testing.T) {
	var callCount int
	srv := newReconcilerTestServer(t,
		map[string]ProjectConfig{"myapp": {AutoSpawn: true, Cwd: "/repos/myapp"}},
		func(project, cwd string, env map[string]string) error {
			callCount++
			return nil
		})

	srv.reconcileSpawnsOnce()

	if callCount != 0 {
		t.Errorf("expected spawnFunc not called with no queued messages, got %d calls", callCount)
	}
}

// TestReconcileSpawnsOnce_SkipsNonAutoSpawnProjects verifies a project
// without autoSpawn is never a spawn candidate, even with queued work and no
// alive agent.
func TestReconcileSpawnsOnce_SkipsNonAutoSpawnProjects(t *testing.T) {
	var callCount int
	srv := newReconcilerTestServer(t,
		map[string]ProjectConfig{"myapp": {AutoSpawn: false, Cwd: "/repos/myapp"}},
		func(project, cwd string, env map[string]string) error {
			callCount++
			return nil
		})
	seedQueuedOrchestratorMessage(srv, "orchestrator-myapp")

	srv.reconcileSpawnsOnce()

	if callCount != 0 {
		t.Errorf("expected spawnFunc not called for a non-autoSpawn project, got %d calls", callCount)
	}
}

// --- Feed + reply (SPEC-ui-v2.md #3) ---

// TestBuildFeed_MergesMessagesAcksAndActivityNewestFirst verifies the
// three source kinds interleave correctly by time, and that an acked
// message produces TWO rows (the send AND the ack, at their own
// timestamps) rather than one merged row.
func TestBuildFeed_MergesMessagesAcksAndActivityNewestFirst(t *testing.T) {
	t0 := mustTime(t, "2026-08-17T12:00:00Z")
	sentAt := t0
	ackedAt := t0.Add(2 * time.Minute)
	activityAt := t0.Add(1 * time.Minute)

	messages := []Message{
		{
			From: "orchestrator-myapp-g1", To: "worker-1", TaskPath: "Tasks/A.md",
			Text: "please do X", Status: "acked", CreatedAt: sentAt, AckedAt: &ackedAt, Response: "done",
		},
	}
	activity := []ActivityEntry{
		{Agent: "orchestrator-myapp-g1", At: activityAt, Text: "spawned worker-1", TaskPath: "Tasks/A.md"},
	}

	feed := buildFeed(messages, activity)

	if len(feed) != 3 {
		t.Fatalf("expected 3 feed rows (send + activity + ack), got %d: %+v", len(feed), feed)
	}
	// Newest first: acked (t0+2m), activity (t0+1m), message (t0).
	if feed[0].Kind != "acked" || feed[0].Text != "done" || feed[0].TaskPath != "Tasks/A.md" {
		t.Errorf("expected newest row to be the ack, got %+v", feed[0])
	}
	if feed[1].Kind != "activity" || feed[1].Text != "spawned worker-1" {
		t.Errorf("expected middle row to be the activity entry, got %+v", feed[1])
	}
	if feed[2].Kind != "message" || feed[2].Text != "please do X" || feed[2].Agent != "orchestrator-myapp-g1" || feed[2].To != "worker-1" {
		t.Errorf("expected oldest row to be the send, got %+v", feed[2])
	}
}

// TestBuildFeed_QueuedMessageProducesNoAckRow verifies a not-yet-acked
// message contributes exactly one row (the send), not a phantom ack.
func TestBuildFeed_QueuedMessageProducesNoAckRow(t *testing.T) {
	messages := []Message{
		{From: "a", To: "b", Text: "hi", Status: "queued", CreatedAt: time.Now()},
	}
	feed := buildFeed(messages, nil)
	if len(feed) != 1 || feed[0].Kind != "message" {
		t.Fatalf("expected exactly 1 message row for a queued (unacked) message, got %+v", feed)
	}
}

// TestBuildFeed_CapsAtFeedCap verifies the merged timeline is capped, not
// unbounded.
func TestBuildFeed_CapsAtFeedCap(t *testing.T) {
	now := time.Now()
	var activity []ActivityEntry
	for i := 0; i < feedCap+20; i++ {
		activity = append(activity, ActivityEntry{Agent: "a", At: now.Add(time.Duration(i) * time.Second), Text: "e"})
	}
	feed := buildFeed(nil, activity)
	if len(feed) != feedCap {
		t.Fatalf("expected feed capped at %d, got %d", feedCap, len(feed))
	}
	// Capped list must be the NEWEST feedCap, not an arbitrary slice.
	wantNewest := now.Add(time.Duration(feedCap+19) * time.Second)
	if !feed[0].At.Equal(wantNewest) {
		t.Errorf("expected the newest entry to survive the cap, got %v want %v", feed[0].At, wantNewest)
	}
}

// TestHandleFeed_ServesMergedFeedOverHTTP is a thin end-to-end check that
// the HTTP handler actually wires buildFeed to real server state.
func TestHandleFeed_ServesMergedFeedOverHTTP(t *testing.T) {
	srv, ts := newTestServer(t)
	srv.mu.Lock()
	srv.appendActivityLocked("agent-x", "did a thing", "Tasks/A.md")
	srv.mu.Unlock()

	resp, err := http.Get(ts.URL + "/feed")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	var body struct {
		Feed []feedEntry `json:"feed"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if len(body.Feed) != 1 || body.Feed[0].Text != "did a thing" || body.Feed[0].TaskPath != "Tasks/A.md" {
		t.Fatalf("expected the activity entry to appear in /feed, got %+v", body.Feed)
	}
}

// TestHandleReply_WritesClaudeLineToReplyBlockAndRoutes is the strongest
// verification available without a live TaskNotes webhook: it exercises
// handleReply's PUT, then feeds the EXACT resulting details string through
// routeTaskNotification (the real webhook handler) to confirm the written
// line actually routes — proving "no new delivery mechanism" end to end,
// not just that a PUT happened.
func TestHandleReply_WritesClaudeLineToReplyBlockAndRoutes(t *testing.T) {
	const path = "Tasks/NeedsInput.md"
	existingDetails := applyNoteBodyEdit("Original description.", func(nb *noteBody) {
		nb.Ask = "Merge or reply?"
	})

	var putBody map[string]any
	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.Method {
		case http.MethodGet:
			json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"path": path, "details": existingDetails}})
		case http.MethodPut:
			json.NewDecoder(r.Body).Decode(&putBody)
			json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"path": path}})
		}
	}))
	defer apiSrv.Close()

	srv, ts := newTestServer(t)
	srv.tnClient = NewClient(Config{URL: apiSrv.URL})
	register(t, ts, "agent-needsinput", "proj-ni")

	reqBody, _ := json.Marshal(map[string]string{"taskPath": path, "text": "looks good, merge it"})
	resp, err := http.Post(ts.URL+"/reply", "application/json", bytes.NewReader(reqBody))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	newDetails, _ := putBody["details"].(string)
	if !strings.Contains(newDetails, "@claude: looks good, merge it") {
		t.Fatalf("expected an @claude: line with the reply text in the PUT'd details, got:\n%s", newDetails)
	}
	if !strings.Contains(newDetails, "Original description.") {
		t.Errorf("expected the description preserved, got:\n%s", newDetails)
	}

	// The real test: does the daemon's OWN webhook handler, given exactly
	// what got PUT'd, actually route this as an @claude: reply? This is
	// what "routes through the existing webhook path" means in practice.
	srv.routeTaskNotification(webhookTask{
		Path: path, Title: "NeedsInput", Status: "needs-input",
		Projects: []string{"proj-ni"}, Details: newDetails,
	})
	msgs := queuedMessagesTo(srv, "agent-needsinput")
	if len(msgs) != 1 || msgs[0].Text != "looks good, merge it" {
		t.Fatalf("expected the UI reply to route to the owning agent via the normal webhook path, got %+v", msgs)
	}
}

// TestHandleReply_StripsRedundantAtClaudePrefix verifies a caller-supplied
// "@claude:" prefix isn't doubled into "@claude: @claude: ...".
func TestHandleReply_StripsRedundantAtClaudePrefix(t *testing.T) {
	const path = "Tasks/X.md"
	var putBody map[string]any
	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.Method {
		case http.MethodGet:
			json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"path": path, "details": ""}})
		case http.MethodPut:
			json.NewDecoder(r.Body).Decode(&putBody)
			json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"path": path}})
		}
	}))
	defer apiSrv.Close()

	srv, ts := newTestServer(t)
	srv.tnClient = NewClient(Config{URL: apiSrv.URL})

	reqBody, _ := json.Marshal(map[string]string{"taskPath": path, "text": "@claude: already prefixed"})
	resp, err := http.Post(ts.URL+"/reply", "application/json", bytes.NewReader(reqBody))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	newDetails, _ := putBody["details"].(string)
	if strings.Contains(newDetails, "@claude: @claude:") {
		t.Fatalf("expected the redundant prefix stripped, got:\n%s", newDetails)
	}
	if !strings.Contains(newDetails, "@claude: already prefixed") {
		t.Fatalf("expected exactly one @claude: prefix preserved, got:\n%s", newDetails)
	}
}

// TestHandleReply_MultiLineText_RoutesCompleteText is the 2026-08-25
// truncation-incident real fix, verified end to end through the actual
// webhook path: extractClaudeLines used to only ever capture the first
// line after "@claude:" (claudeLineRe is line-anchored, and wrapCallout
// renders a multi-line reply as separate quoted lines), so a multi-line
// reply's second paragraph onward was silently dropped from the ROUTED
// message even though the note itself stayed complete — confirmed live
// against real production messages before this fix. extractClaudeDirectives
// (serve.go) now parses the note's structured blocks first, so the full
// multi-line reply is what gets dispatched, not just its first line.
func TestHandleReply_MultiLineText_RoutesCompleteText(t *testing.T) {
	const path = "Tasks/MultiLine.md"
	var putBody map[string]any
	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.Method {
		case http.MethodGet:
			json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"path": path, "details": ""}})
		case http.MethodPut:
			json.NewDecoder(r.Body).Decode(&putBody)
			json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"path": path}})
		}
	}))
	defer apiSrv.Close()

	srv, ts := newTestServer(t)
	srv.tnClient = NewClient(Config{URL: apiSrv.URL})
	register(t, ts, "agent-multiline", "proj-ml")

	multiline := "First paragraph, the part that used to be all that arrived.\n\nSecond paragraph, previously lost entirely.\n\nThird paragraph, also previously lost."
	reqBody, _ := json.Marshal(map[string]string{"taskPath": path, "text": multiline})
	resp, err := http.Post(ts.URL+"/reply", "application/json", bytes.NewReader(reqBody))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	newDetails, _ := putBody["details"].(string)
	if !strings.Contains(newDetails, "@claude: First paragraph") {
		t.Fatalf("expected the reply written plainly (no pointer/split needed anymore), got:\n%s", newDetails)
	}

	// The real test: route it through the actual webhook path exactly as
	// TestHandleReply_WritesClaudeLineToReplyBlockAndRoutes does, and
	// confirm the DISPATCHED message contains the complete text, not just
	// the first line.
	srv.routeTaskNotification(webhookTask{
		Path: path, Title: "MultiLine", Status: "open",
		Projects: []string{"proj-ml"}, Details: newDetails,
	})
	msgs := queuedMessagesTo(srv, "agent-multiline")
	if len(msgs) != 1 {
		t.Fatalf("expected exactly 1 routed message, got %+v", msgs)
	}
	if msgs[0].Text != multiline {
		t.Errorf("expected the routed message to match the original multi-line text exactly, got %q", msgs[0].Text)
	}
}

// TestHandleReply_MissingFieldsRejected verifies both taskPath and text
// are required.
func TestHandleReply_MissingFieldsRejected(t *testing.T) {
	srv, ts := newTestServer(t)
	srv.tnClient = NewClient(Config{URL: "http://unused.invalid"})

	cases := []map[string]string{
		{"taskPath": "", "text": "hi"},
		{"taskPath": "Tasks/X.md", "text": ""},
	}
	for _, c := range cases {
		reqBody, _ := json.Marshal(c)
		resp, err := http.Post(ts.URL+"/reply", "application/json", bytes.NewReader(reqBody))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("expected 400 for %+v, got %d", c, resp.StatusCode)
		}
	}
}

// TestHandleReply_NoTnClientConfigured verifies a clear 503 rather than a
// nil-pointer panic when no TaskNotes client is configured (matches the
// nil-check convention used throughout serve.go for s.tnClient).
func TestHandleReply_NoTnClientConfigured(t *testing.T) {
	srv, ts := newTestServer(t)
	srv.tnClient = nil

	reqBody, _ := json.Marshal(map[string]string{"taskPath": "Tasks/X.md", "text": "hi"})
	resp, err := http.Post(ts.URL+"/reply", "application/json", bytes.NewReader(reqBody))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("expected 503 with no tnClient configured, got %d", resp.StatusCode)
	}
}

package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeTaskNotesGetPutAPI is a minimal TaskNotes API stand-in: GET returns
// whatever details is currently held (mutated by PUT), so a test can send
// a second ack and observe whether the note-surfacing logic re-reads its
// own prior write (the dedup path this whole feature depends on).
func fakeTaskNotesGetPutAPI(t *testing.T, path, initialDetails string) (*httptest.Server, func() string) {
	t.Helper()
	details := initialDetails
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.Method {
		case http.MethodGet:
			json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"path": path, "details": details}})
		case http.MethodPut:
			var body map[string]any
			json.NewDecoder(r.Body).Decode(&body)
			if d, ok := body["details"].(string); ok {
				details = d
			}
			json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"path": path, "details": details}})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, func() string { return details }
}

// TestAckResponse_MirroredIntoTaskHistory verifies the core of the fix: an
// ack carrying a response AND a taskPath surfaces that response in the
// task's own history, so the user sees the answer without cross-referencing
// the bridge inbox.
func TestAckResponse_MirroredIntoTaskHistory(t *testing.T) {
	const path = "Tasks/Question.md"
	apiSrv, current := fakeTaskNotesGetPutAPI(t, path, "Original description.")

	srv, ts := newTestServer(t)
	srv.tnClient = NewClient(Config{URL: apiSrv.URL})
	register(t, ts, "agent-answerer", "proj-a")

	sendResp := mustPost(t, ts.URL+"/messages", map[string]any{
		"to": "agent-answerer", "taskPath": path, "text": "@claude: where's the config file?",
	})
	var sent struct {
		Message Message `json:"message"`
	}
	decodeInto(t, sendResp, &sent)
	sendResp.Body.Close()

	ackResp := mustPost(t, ts.URL+"/ack", map[string]any{
		"name": "agent-answerer", "msgId": sent.Message.ID, "response": "It's at config/app.yaml",
	})
	ackResp.Body.Close()
	if ackResp.StatusCode != http.StatusOK {
		t.Fatalf("ack: expected 200, got %d", ackResp.StatusCode)
	}

	details := current()
	if !strings.Contains(details, "It's at config/app.yaml") {
		t.Fatalf("expected the ack response to be noted in task history, got details:\n%s", details)
	}
	if !strings.Contains(details, "agent-answerer (answering)") {
		t.Fatalf("expected the note to attribute the answering agent, got details:\n%s", details)
	}
}

// TestAckResponse_SecondIdenticalAckNoDuplicate verifies the dedup guard:
// acking the same response text twice (e.g. an agent that also ran `tn
// note` with identical text, or a retried ack) must not double-note it.
func TestAckResponse_SecondIdenticalAckNoDuplicate(t *testing.T) {
	const path = "Tasks/Question2.md"
	apiSrv, current := fakeTaskNotesGetPutAPI(t, path, "Original description.")

	srv, ts := newTestServer(t)
	srv.tnClient = NewClient(Config{URL: apiSrv.URL})
	register(t, ts, "agent-answerer2", "proj-a")

	send := func(text string) string {
		resp := mustPost(t, ts.URL+"/messages", map[string]any{
			"to": "agent-answerer2", "taskPath": path, "text": text,
		})
		var sent struct {
			Message Message `json:"message"`
		}
		decodeInto(t, resp, &sent)
		resp.Body.Close()
		return sent.Message.ID
	}

	id1 := send("@claude: q1")
	ackResp := mustPost(t, ts.URL+"/ack", map[string]any{"name": "agent-answerer2", "msgId": id1, "response": "The answer is 42"})
	ackResp.Body.Close()

	countAfterFirst := strings.Count(current(), "The answer is 42")
	if countAfterFirst != 1 {
		t.Fatalf("expected 1 occurrence after first ack, got %d:\n%s", countAfterFirst, current())
	}

	id2 := send("@claude: q1 again")
	ackResp2 := mustPost(t, ts.URL+"/ack", map[string]any{"name": "agent-answerer2", "msgId": id2, "response": "The answer is 42"})
	ackResp2.Body.Close()

	countAfterSecond := strings.Count(current(), "The answer is 42")
	if countAfterSecond != 1 {
		t.Fatalf("expected still 1 occurrence after identical second ack (no duplicate), got %d:\n%s", countAfterSecond, current())
	}
}

// TestAckResponse_NoTaskPathNoWrite verifies an ack with a response but no
// taskPath (agent-to-agent chat, not task-scoped) never touches the
// TaskNotes API and never errors.
func TestAckResponse_NoTaskPathNoWrite(t *testing.T) {
	var apiHit bool
	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		apiHit = true
		w.WriteHeader(http.StatusOK)
	}))
	defer apiSrv.Close()

	srv, ts := newTestServer(t)
	srv.tnClient = NewClient(Config{URL: apiSrv.URL})
	register(t, ts, "agent-notask", "proj-a")

	sendResp := mustPost(t, ts.URL+"/messages", map[string]any{"to": "agent-notask", "text": "hey"})
	var sent struct {
		Message Message `json:"message"`
	}
	decodeInto(t, sendResp, &sent)
	sendResp.Body.Close()

	ackResp := mustPost(t, ts.URL+"/ack", map[string]any{"name": "agent-notask", "msgId": sent.Message.ID, "response": "sup"})
	ackResp.Body.Close()
	if ackResp.StatusCode != http.StatusOK {
		t.Fatalf("ack: expected 200, got %d", ackResp.StatusCode)
	}
	if apiHit {
		t.Fatalf("expected no TaskNotes API call for a taskPath-less ack")
	}
}

// TestAckResponse_EmptyResponseNoWrite verifies a plain ack (no response
// text) never touches the TaskNotes API — nothing to surface.
func TestAckResponse_EmptyResponseNoWrite(t *testing.T) {
	var apiHit bool
	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		apiHit = true
		w.WriteHeader(http.StatusOK)
	}))
	defer apiSrv.Close()

	srv, ts := newTestServer(t)
	srv.tnClient = NewClient(Config{URL: apiSrv.URL})
	register(t, ts, "agent-noresp", "proj-a")

	sendResp := mustPost(t, ts.URL+"/messages", map[string]any{"to": "agent-noresp", "taskPath": "Tasks/Silent.md", "text": "hey"})
	var sent struct {
		Message Message `json:"message"`
	}
	decodeInto(t, sendResp, &sent)
	sendResp.Body.Close()

	ackResp := mustPost(t, ts.URL+"/ack", map[string]any{"name": "agent-noresp", "msgId": sent.Message.ID})
	ackResp.Body.Close()
	if ackResp.StatusCode != http.StatusOK {
		t.Fatalf("ack: expected 200, got %d", ackResp.StatusCode)
	}
	if apiHit {
		t.Fatalf("expected no TaskNotes API call for a response-less ack")
	}
}

package tn

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
)

// fakeWebhookAPI serves GET/POST/DELETE /api/webhooks(/:id) against an
// in-memory list, so tryRegisterWebhook can be exercised against realistic
// request/response shapes without a real TaskNotes instance.
type fakeWebhookAPI struct {
	entries    []webhookEntry
	nextID     int
	deleteHits []string
	createHits [][]string // one []string per CreateWebhook call, the events list
}

func newFakeWebhookAPI(t *testing.T, initial []webhookEntry) (*fakeWebhookAPI, *httptest.Server) {
	t.Helper()
	f := &fakeWebhookAPI{entries: initial, nextID: 1}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/webhooks":
			json.NewEncoder(w).Encode(map[string]any{
				"success": true,
				"data":    map[string]any{"webhooks": f.entries, "total": len(f.entries)},
			})
		case r.Method == http.MethodPost && r.URL.Path == "/api/webhooks":
			var body struct {
				URL    string   `json:"url"`
				Events []string `json:"events"`
			}
			json.NewDecoder(r.Body).Decode(&body)
			f.createHits = append(f.createHits, body.Events)
			id := "wh-" + strconv.Itoa(f.nextID)
			f.nextID++
			f.entries = append(f.entries, webhookEntry{ID: id, URL: body.URL, Events: body.Events})
			json.NewEncoder(w).Encode(map[string]any{"success": true, "data": map[string]any{"id": id}})
		case r.Method == http.MethodDelete && len(r.URL.Path) > len("/api/webhooks/"):
			id := r.URL.Path[len("/api/webhooks/"):]
			f.deleteHits = append(f.deleteHits, id)
			kept := f.entries[:0]
			for _, e := range f.entries {
				if e.ID != id {
					kept = append(kept, e)
				}
			}
			f.entries = kept
			json.NewEncoder(w).Encode(map[string]any{"success": true, "data": map[string]any{}})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return f, srv
}

// TestTryRegisterWebhook_CreatesWithAllRequiredEventsWhenNoneRegistered
// verifies a fresh registration requests every entry in
// requiredWebhookEvents — specifically including "task.completed", the
// event the live daemon was missing (see the guard fix in
// handleTaskNotesWebhook).
func TestTryRegisterWebhook_CreatesWithAllRequiredEventsWhenNoneRegistered(t *testing.T) {
	f, apiSrv := newFakeWebhookAPI(t, nil)
	srv, _ := newTestServer(t)
	client := NewClient(Config{URL: apiSrv.URL})

	ok := srv.tryRegisterWebhook(client, "http://localhost:8391/webhooks/tasknotes")
	if !ok {
		t.Fatal("expected tryRegisterWebhook to succeed")
	}
	if len(f.createHits) != 1 {
		t.Fatalf("expected exactly 1 CreateWebhook call, got %d", len(f.createHits))
	}
	for _, want := range requiredWebhookEvents {
		found := false
		for _, got := range f.createHits[0] {
			if got == want {
				found = true
			}
		}
		if !found {
			t.Errorf("expected the created webhook to request event %q, got %v", want, f.createHits[0])
		}
	}
}

// TestTryRegisterWebhook_AlreadyCorrectIsNoOp verifies an existing
// registration whose events already cover requiredWebhookEvents is left
// alone — no delete, no re-create.
func TestTryRegisterWebhook_AlreadyCorrectIsNoOp(t *testing.T) {
	const selfURL = "http://localhost:8391/webhooks/tasknotes"
	f, apiSrv := newFakeWebhookAPI(t, []webhookEntry{
		{ID: "wh-1", URL: selfURL, Events: []string{"task.created", "task.updated", "task.completed"}},
	})
	srv, _ := newTestServer(t)
	client := NewClient(Config{URL: apiSrv.URL})

	ok := srv.tryRegisterWebhook(client, selfURL)
	if !ok {
		t.Fatal("expected tryRegisterWebhook to succeed")
	}
	if len(f.createHits) != 0 || len(f.deleteHits) != 0 {
		t.Errorf("expected no create/delete calls for an already-correct registration, got create=%d delete=%d", len(f.createHits), len(f.deleteHits))
	}
}

// TestTryRegisterWebhook_StaleEventsReplaced verifies the actual fix for
// the live gap: an existing registration for our URL that's missing
// task.completed (e.g. registered by an older daemon build) is DELETED and
// RE-CREATED with the full required event list — not left in place, since
// TaskNotes has no update endpoint.
func TestTryRegisterWebhook_StaleEventsReplaced(t *testing.T) {
	const selfURL = "http://localhost:8391/webhooks/tasknotes"
	f, apiSrv := newFakeWebhookAPI(t, []webhookEntry{
		{ID: "wh-old", URL: selfURL, Events: []string{"task.created", "task.updated"}}, // missing task.completed
	})
	srv, _ := newTestServer(t)
	client := NewClient(Config{URL: apiSrv.URL})

	ok := srv.tryRegisterWebhook(client, selfURL)
	if !ok {
		t.Fatal("expected tryRegisterWebhook to succeed")
	}
	if len(f.deleteHits) != 1 || f.deleteHits[0] != "wh-old" {
		t.Fatalf("expected wh-old deleted, got deleteHits=%v", f.deleteHits)
	}
	if len(f.createHits) != 1 {
		t.Fatalf("expected exactly 1 re-create call, got %d", len(f.createHits))
	}
	hasCompleted := false
	for _, e := range f.createHits[0] {
		if e == "task.completed" {
			hasCompleted = true
		}
	}
	if !hasCompleted {
		t.Errorf("expected the re-created webhook to include task.completed, got %v", f.createHits[0])
	}
}

// TestHandleTaskNotesWebhook_TaskCompletedEventRoutes verifies the other
// half of the fix: the handler itself must accept a "task.completed"
// event (not only task.created/task.updated) and route it exactly like
// any other observation — this is what actually closes the live gap
// (registering for the event is useless if the handler then ignores it).
func TestHandleTaskNotesWebhook_TaskCompletedEventRoutes(t *testing.T) {
	srv, ts := newTestServer(t)
	register(t, ts, "agent-completed", "proj-c")

	// TaskNotes fires task.completed with only {"task": ...} — no
	// "previous" key, unlike task.updated.
	payload := map[string]any{
		"event": "task.completed",
		"data": map[string]any{
			"task": map[string]any{
				"path": "Tasks/Done.md", "title": "Done", "status": "done",
				"projects": []string{"proj-c"}, "details": "@claude: it's finished",
			},
		},
	}
	resp := mustPost(t, ts.URL+"/webhooks/tasknotes", payload)
	resp.Body.Close()

	msgs := queuedMessagesTo(srv, "agent-completed")
	if len(msgs) != 1 || msgs[0].Text != "it's finished" {
		t.Fatalf("expected the task.completed event to route the @claude line, got %+v", msgs)
	}
}

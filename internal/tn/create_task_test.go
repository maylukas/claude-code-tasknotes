package tn

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

// fakeCreateTaskAPI serves POST /api/tasks, handing out sequentially
// numbered paths so tests can distinguish "created once" from "created
// twice" by counting calls.
type fakeCreateTaskAPI struct {
	calls int
}

func newFakeCreateTaskAPI(t *testing.T) (*fakeCreateTaskAPI, *httptest.Server) {
	t.Helper()
	f := &fakeCreateTaskAPI{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/tasks" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		f.calls++
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		title, _ := body["title"].(string)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{
			"path": "Tasks/Created-" + title + "-" + strconv.Itoa(f.calls) + ".md", "title": title,
		}})
	}))
	t.Cleanup(srv.Close)
	return f, srv
}

// TestHandleCreateTask_RepeatKeyReturnsSameTaskOnlyOneCreate verifies the
// core fix: a repeat POST /tasks/create with the SAME idempotencyKey
// returns the ORIGINAL task's path, and TaskNotes is only ever called
// once — a `tn create` retry after a client-side timeout must be
// indistinguishable from a first-time success.
func TestHandleCreateTask_RepeatKeyReturnsSameTaskOnlyOneCreate(t *testing.T) {
	fake, apiSrv := newFakeCreateTaskAPI(t)
	srv, ts := newTestServer(t)
	srv.tnClient = NewClient(Config{URL: apiSrv.URL})

	body1 := map[string]any{"idempotencyKey": "key-1", "fields": map[string]any{"title": "Same"}}
	resp1 := mustPost(t, ts.URL+"/tasks/create", body1)
	var out1 struct {
		Task Task `json:"task"`
	}
	decodeInto(t, resp1, &out1)

	resp2 := mustPost(t, ts.URL+"/tasks/create", body1)
	var out2 struct {
		Task Task `json:"task"`
	}
	decodeInto(t, resp2, &out2)

	if fake.calls != 1 {
		t.Fatalf("expected exactly 1 TaskNotes create call, got %d", fake.calls)
	}
	if out1.Task.Path != out2.Task.Path {
		t.Errorf("expected the same path on repeat, got %q vs %q", out1.Task.Path, out2.Task.Path)
	}
	if out1.Task.Path == "" {
		t.Error("expected a non-empty path")
	}
}

// TestHandleCreateTask_DifferentKeyCreatesSecondTask verifies a genuinely
// different idempotency key is treated as a separate, real create.
func TestHandleCreateTask_DifferentKeyCreatesSecondTask(t *testing.T) {
	fake, apiSrv := newFakeCreateTaskAPI(t)
	srv, ts := newTestServer(t)
	srv.tnClient = NewClient(Config{URL: apiSrv.URL})

	resp1 := mustPost(t, ts.URL+"/tasks/create", map[string]any{"idempotencyKey": "key-a", "fields": map[string]any{"title": "A"}})
	resp1.Body.Close()
	resp2 := mustPost(t, ts.URL+"/tasks/create", map[string]any{"idempotencyKey": "key-b", "fields": map[string]any{"title": "B"}})
	resp2.Body.Close()

	if fake.calls != 2 {
		t.Errorf("expected 2 TaskNotes create calls for 2 different keys, got %d", fake.calls)
	}
}

// TestHandleCreateTask_KeyExpiryAllowsGenuineLaterRecreate verifies an
// idempotency record older than createIdempotencyTTL is treated as
// expired — a deliberate later re-creation of something with the same
// title/project/details must not be permanently blocked.
func TestHandleCreateTask_KeyExpiryAllowsGenuineLaterRecreate(t *testing.T) {
	fake, apiSrv := newFakeCreateTaskAPI(t)
	srv, ts := newTestServer(t)
	srv.tnClient = NewClient(Config{URL: apiSrv.URL})

	resp1 := mustPost(t, ts.URL+"/tasks/create", map[string]any{"idempotencyKey": "key-expiring", "fields": map[string]any{"title": "X"}})
	resp1.Body.Close()

	// Manually age the recorded entry past the TTL.
	srv.mu.Lock()
	rec := srv.state.CreatedTasks["key-expiring"]
	rec.CreatedAt = time.Now().Add(-createIdempotencyTTL - time.Minute)
	srv.state.CreatedTasks["key-expiring"] = rec
	srv.mu.Unlock()

	resp2 := mustPost(t, ts.URL+"/tasks/create", map[string]any{"idempotencyKey": "key-expiring", "fields": map[string]any{"title": "X"}})
	resp2.Body.Close()

	if fake.calls != 2 {
		t.Errorf("expected a genuine second create after the key expired, got %d TaskNotes calls", fake.calls)
	}
}

// TestCmdCreate_RoutesThroughBridgeWhenReachableAndDedupsOnRetry is the
// real end-to-end deployment path: `tn create` (via cmdCreate) with a live
// bridge reachable routes through it rather than the direct-to-TaskNotes
// fallback, and a second identical invocation (simulating a client retry
// after a timeout) is deduped — only 1 TaskNotes create call total, both
// invocations print the same path. The direct client passed to cmdCreate
// points at a server that fails any request it receives, proving the
// fallback path is never taken when the bridge is up.
func TestCmdCreate_RoutesThroughBridgeWhenReachableAndDedupsOnRetry(t *testing.T) {
	fake, apiSrv := newFakeCreateTaskAPI(t)
	srv, bridgeTs := newTestServer(t)
	srv.tnClient = NewClient(Config{URL: apiSrv.URL})
	t.Setenv("TN_BRIDGE_URL", bridgeTs.URL)

	failSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("fallback direct-to-TaskNotes client must not be used when the bridge is reachable")
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer failSrv.Close()
	directClient := newTestClient(failSrv, "")

	var err1, err2 error
	out := captureStdout(t, func() {
		err1 = cmdCreate(directClient, []string{"--title", "Retryable Task", "--claude-project", "myapp"})
		err2 = cmdCreate(directClient, []string{"--title", "Retryable Task", "--claude-project", "myapp"})
	})

	if err1 != nil || err2 != nil {
		t.Fatalf("expected both calls to succeed, got err1=%v err2=%v", err1, err2)
	}
	if fake.calls != 1 {
		t.Errorf("expected exactly 1 TaskNotes create call across both invocations (deduped), got %d", fake.calls)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 2 || lines[0] != lines[1] {
		t.Errorf("expected both invocations to print the SAME path, got %q", lines)
	}
}

// TestCreateTaskIdempotent_AmbiguousBridgeErrorDoesNotFallBack verifies
// the fallback-safety rule: the bridge IS reachable but returns an error
// (simulating the daemon's own call to TaskNotes failing) — this must NOT
// trigger a second attempt via the direct-to-TaskNotes client. Falling
// back on an ambiguous failure would risk creating the exact duplicate
// this feature exists to prevent (the daemon-side create may have actually
// landed despite erroring back to the CLI). Only a crisp "bridge isn't
// running at all" (connection refused) may fall back.
func TestCreateTaskIdempotent_AmbiguousBridgeErrorDoesNotFallBack(t *testing.T) {
	bridgeSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "simulated daemon-side TaskNotes failure", http.StatusBadGateway)
	}))
	defer bridgeSrv.Close()
	t.Setenv("TN_BRIDGE_URL", bridgeSrv.URL)

	var fallbackCalled bool
	directSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fallbackCalled = true
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"data":{"path":"Tasks/ShouldNotExist.md","title":"ShouldNotExist"}}`))
	}))
	defer directSrv.Close()
	directClient := newTestClient(directSrv, "")

	_, err := createTaskIdempotent("some-key", map[string]any{"title": "X"}, directClient)

	if err == nil {
		t.Fatal("expected an error from the ambiguous bridge failure, got nil")
	}
	if fallbackCalled {
		t.Error("expected NO fallback to the direct client on an ambiguous (non-connection-refused) bridge error")
	}
}

// TestHandleCreateTask_NoKeyMeansNoDedup verifies existing behavior is
// unchanged when no idempotencyKey is sent (older binaries / direct
// callers): every call creates a new task, no dedup at all.
func TestHandleCreateTask_NoKeyMeansNoDedup(t *testing.T) {
	fake, apiSrv := newFakeCreateTaskAPI(t)
	srv, ts := newTestServer(t)
	srv.tnClient = NewClient(Config{URL: apiSrv.URL})

	resp1 := mustPost(t, ts.URL+"/tasks/create", map[string]any{"fields": map[string]any{"title": "NoKey"}})
	resp1.Body.Close()
	resp2 := mustPost(t, ts.URL+"/tasks/create", map[string]any{"fields": map[string]any{"title": "NoKey"}})
	resp2.Body.Close()

	if fake.calls != 2 {
		t.Errorf("expected no dedup at all with no idempotencyKey, got %d TaskNotes calls (want 2)", fake.calls)
	}
}

// --- Ambiguous-failure lookup-before-retry (residual gap closed) ---

// ambiguousCreateThenLookupAPI simulates a TaskNotes instance where the
// FIRST POST /api/tasks call fails ambiguously (connection hijacked and
// closed mid-response — not a clean HTTP error, not connection-refused;
// the client genuinely can't tell whether it landed), any SUBSEQUENT
// POST /api/tasks call succeeds normally, and POST /api/tasks/query
// returns whatever queryResult is configured, but only once queryCalls
// has reached appearAfterQueries — before that, it returns nothing, no
// matter what queryResult holds. appearAfterQueries=0 (the zero value)
// means "visible from the first query", matching the original behavior
// for tests that don't care about the flush delay.
type ambiguousCreateThenLookupAPI struct {
	createCalls        int
	queryCalls         int
	queryResult        []map[string]any
	appearAfterQueries int
}

func newAmbiguousCreateThenLookupAPI(t *testing.T, queryResult []map[string]any) (*ambiguousCreateThenLookupAPI, *httptest.Server) {
	t.Helper()
	return newAmbiguousCreateThenLookupAPIWithDelay(t, queryResult, 0)
}

func newAmbiguousCreateThenLookupAPIWithDelay(t *testing.T, queryResult []map[string]any, appearAfterQueries int) (*ambiguousCreateThenLookupAPI, *httptest.Server) {
	t.Helper()
	f := &ambiguousCreateThenLookupAPI{queryResult: queryResult, appearAfterQueries: appearAfterQueries}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/tasks":
			f.createCalls++
			if f.createCalls == 1 {
				// Ambiguous failure: accept the connection, then drop it
				// without writing any response — the client sees an error
				// that is neither a clean HTTP status nor connection-refused.
				hj, ok := w.(http.Hijacker)
				if !ok {
					t.Fatal("ResponseWriter does not support hijacking")
				}
				conn, _, err := hj.Hijack()
				if err != nil {
					t.Fatalf("hijack failed: %v", err)
				}
				conn.Close()
				return
			}
			var body map[string]any
			json.NewDecoder(r.Body).Decode(&body)
			title, _ := body["title"].(string)
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{
				"path": "Tasks/Retried-" + title + ".md", "title": title,
			}})
		case r.Method == http.MethodPost && r.URL.Path == "/api/tasks/query":
			f.queryCalls++
			result := f.queryResult
			if f.queryCalls <= f.appearAfterQueries {
				result = nil // write hasn't "flushed" yet — nothing to find
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{
				"tasks": result, "total": len(result), "filtered": len(result),
			}})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return f, srv
}

// shrinkCreateAmbiguityBackoffForTest replaces the real (tens-of-seconds)
// backoff schedule with a short one for the duration of the test, and
// restores it after — so tests exercise the real retry-loop logic without
// actually sleeping for 40 seconds.
func shrinkCreateAmbiguityBackoffForTest(t *testing.T, schedule []time.Duration) {
	t.Helper()
	orig := createAmbiguityBackoffSchedule
	createAmbiguityBackoffSchedule = schedule
	t.Cleanup(func() { createAmbiguityBackoffSchedule = orig })
}

// TestHandleCreateTask_AmbiguousFailureWithTaskPresentReturnsExisting
// verifies the core of the residual-gap fix: an ambiguous create failure
// (timeout/dropped connection) followed by a lookup that FINDS a matching
// recent task returns that task's path as success — no second create
// attempt — and records the idempotency mapping so a further retry also
// short-circuits without even reaching TaskNotes.
func TestHandleCreateTask_AmbiguousFailureWithTaskPresentReturnsExisting(t *testing.T) {
	recentlyCreated := time.Now().Add(-2 * time.Minute).Format(time.RFC3339Nano)
	fake, apiSrv := newAmbiguousCreateThenLookupAPI(t, []map[string]any{
		{"path": "Tasks/AlreadyLanded.md", "title": "Landed Anyway", "dateCreated": recentlyCreated,
			"customProperties": map[string]any{"claude-project": "myapp"}},
	})
	srv, ts := newTestServer(t)
	srv.tnClient = NewClient(Config{URL: apiSrv.URL})

	body := map[string]any{
		"idempotencyKey": "amb-key-1",
		"fields": map[string]any{
			"title": "Landed Anyway", "customProperties": map[string]any{"claude-project": "myapp"},
		},
	}
	resp := mustPost(t, ts.URL+"/tasks/create", body)
	var out struct {
		Task Task `json:"task"`
	}
	decodeInto(t, resp, &out)

	if fake.createCalls != 1 {
		t.Errorf("expected exactly 1 create attempt (the ambiguous one; no retry once a match was found), got %d", fake.createCalls)
	}
	if fake.queryCalls != 1 {
		t.Errorf("expected exactly 1 lookup query, got %d", fake.queryCalls)
	}
	if out.Task.Path != "Tasks/AlreadyLanded.md" {
		t.Errorf("expected the existing task's path returned, got %q", out.Task.Path)
	}

	srv.mu.Lock()
	rec, ok := srv.state.CreatedTasks["amb-key-1"]
	srv.mu.Unlock()
	if !ok || rec.Path != "Tasks/AlreadyLanded.md" {
		t.Errorf("expected the idempotency mapping recorded to the found task, got %+v (ok=%v)", rec, ok)
	}
}

// TestHandleCreateTask_AmbiguousFailureRaceWriteAppearsAfterDelay is the
// regression test for the actual production incident: the write is
// genuinely queued, not failed, and only becomes visible to a query after
// a couple of query cycles (simulating the real stall where the flush
// took well over a minute). The old code queried exactly once, found
// nothing, and blindly retried the create — landing both the original
// (delayed) write and the retry as a duplicate. The fix must keep
// re-querying (not re-creating) until the write shows up: exactly ONE
// create call, multiple lookup queries, and the existing (delayed) task
// returned rather than a freshly retried one.
func TestHandleCreateTask_AmbiguousFailureRaceWriteAppearsAfterDelay(t *testing.T) {
	shrinkCreateAmbiguityBackoffForTest(t, []time.Duration{time.Millisecond, time.Millisecond, time.Millisecond})
	recentlyCreated := time.Now().Add(-30 * time.Second).Format(time.RFC3339Nano)
	// appearAfterQueries=2: the first two lookups find nothing (write
	// still queued), the third and any later one finds it (flushed).
	fake, apiSrv := newAmbiguousCreateThenLookupAPIWithDelay(t, []map[string]any{
		{"path": "Tasks/FlushedLate.md", "title": "Flushed Late", "dateCreated": recentlyCreated,
			"customProperties": map[string]any{"claude-project": "myapp"}},
	}, 2)
	srv, ts := newTestServer(t)
	srv.tnClient = NewClient(Config{URL: apiSrv.URL})

	body := map[string]any{
		"idempotencyKey": "amb-delay-1",
		"fields": map[string]any{
			"title": "Flushed Late", "customProperties": map[string]any{"claude-project": "myapp"},
		},
	}
	resp := mustPost(t, ts.URL+"/tasks/create", body)
	var out struct {
		Task Task `json:"task"`
	}
	decodeInto(t, resp, &out)

	if fake.createCalls != 1 {
		t.Errorf("expected exactly 1 create attempt (must not retry the create while the write is merely delayed), got %d", fake.createCalls)
	}
	if fake.queryCalls < 3 {
		t.Errorf("expected the backoff to keep re-querying until the write appeared (>=3 queries), got %d", fake.queryCalls)
	}
	if out.Task.Path != "Tasks/FlushedLate.md" {
		t.Errorf("expected the delayed-but-real write's path returned (no duplicate create), got %q", out.Task.Path)
	}
}

// TestHandleCreateTask_AmbiguousFailureExhaustsBackoffReportsAmbiguityNoRetry
// covers the other half of the fix: if the entire backoff window elapses
// and the lookup still finds nothing, the handler must NOT fall back to
// blindly retrying the create (that guess is exactly what produced
// duplicates) — it reports the ambiguity to the caller instead, with a
// non-200 status distinct from both success and a clean/definite failure.
func TestHandleCreateTask_AmbiguousFailureExhaustsBackoffReportsAmbiguityNoRetry(t *testing.T) {
	shrinkCreateAmbiguityBackoffForTest(t, []time.Duration{time.Millisecond, time.Millisecond, time.Millisecond})
	fake, apiSrv := newAmbiguousCreateThenLookupAPI(t, nil) // never finds anything
	srv, ts := newTestServer(t)
	srv.tnClient = NewClient(Config{URL: apiSrv.URL})

	body := map[string]any{"fields": map[string]any{"title": "Never Landed"}}
	resp := mustPost(t, ts.URL+"/tasks/create", body)
	var out struct {
		Ambiguous bool   `json:"ambiguous"`
		Title     string `json:"title"`
	}
	decodeInto(t, resp, &out)

	if fake.createCalls != 1 {
		t.Errorf("expected exactly 1 create attempt (no blind retry once the backoff window is exhausted), got %d", fake.createCalls)
	}
	if fake.queryCalls != 4 { // 1 immediate + 3 backoff steps
		t.Errorf("expected 4 lookup queries (1 immediate + 3 backoff retries), got %d", fake.queryCalls)
	}
	if resp.StatusCode != http.StatusConflict {
		t.Errorf("expected 409 Conflict reporting the ambiguity, got %d", resp.StatusCode)
	}
	if !out.Ambiguous || out.Title != "Never Landed" {
		t.Errorf("expected an honest ambiguous:true report naming the title, got %+v", out)
	}
}

// TestHandleCreateTask_Clean4xxSurfacesErrorNoLookupNoRetry verifies a
// clean HTTP error response (TaskNotes definitively rejected the create,
// e.g. a validation failure) is surfaced immediately — no lookup query,
// no retry, since there's no ambiguity about whether it landed.
func TestHandleCreateTask_Clean4xxSurfacesErrorNoLookupNoRetry(t *testing.T) {
	var createCalls, queryCalls int
	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/tasks":
			createCalls++
			http.Error(w, `{"error":"title is required"}`, http.StatusBadRequest)
		case r.Method == http.MethodPost && r.URL.Path == "/api/tasks/query":
			queryCalls++
			w.Write([]byte(`{"data":{"tasks":[],"total":0,"filtered":0}}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer apiSrv.Close()
	srv, ts := newTestServer(t)
	srv.tnClient = NewClient(Config{URL: apiSrv.URL})

	resp := mustPost(t, ts.URL+"/tasks/create", map[string]any{"fields": map[string]any{}})
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusOK {
		t.Fatal("expected a non-200 response surfacing the clean error")
	}
	if createCalls != 1 {
		t.Errorf("expected exactly 1 create attempt (no retry on a clean error), got %d", createCalls)
	}
	if queryCalls != 0 {
		t.Errorf("expected NO lookup query for a clean (unambiguous) failure, got %d", queryCalls)
	}
}

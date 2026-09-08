package tn

import (
	"bytes"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func newTestClient(srv *httptest.Server, token string) *Client {
	cfg := Config{URL: srv.URL, Token: token}
	return NewClient(cfg)
}

// TestAuthHeader verifies the Authorization header is sent when a token is
// configured, and is absent when it is empty.
func TestAuthHeader(t *testing.T) {
	var gotAuth string
	var sawHeader bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth, sawHeader = r.Header.Get("Authorization"), r.Header.Get("Authorization") != ""
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"status":"ok"}`))
	}))
	defer srv.Close()

	// With token.
	c := newTestClient(srv, "secret-token")
	if _, err := c.Health(); err != nil {
		t.Fatalf("Health with token: %v", err)
	}
	if !sawHeader {
		t.Fatal("expected Authorization header to be sent when token is configured")
	}
	if gotAuth != "Bearer secret-token" {
		t.Fatalf("expected 'Bearer secret-token', got %q", gotAuth)
	}

	// Without token.
	c2 := newTestClient(srv, "")
	if _, err := c2.Health(); err != nil {
		t.Fatalf("Health without token: %v", err)
	}
	if sawHeader {
		t.Fatal("expected no Authorization header when token is empty")
	}
}

// TestBuildListQuery checks the FilterQuery JSON body shape for default,
// --all, and filter-flag variants of `tn list`.
func TestBuildListQuery(t *testing.T) {
	t.Run("default", func(t *testing.T) {
		fg := buildListQuery("", "", "", "", false, false)
		if len(fg.Children) != 2 {
			t.Fatalf("expected 2 default conditions, got %d: %+v", len(fg.Children), fg.Children)
		}
		if fg.Children[0].Property != "archived" || fg.Children[0].Operator != "is-not-checked" {
			t.Errorf("unexpected first condition: %+v", fg.Children[0])
		}
		if fg.Children[1].Property != "status.isCompleted" || fg.Children[1].Operator != "is-not-checked" {
			t.Errorf("unexpected second condition: %+v", fg.Children[1])
		}
		if fg.Conjunction != "and" || fg.Type != "group" || fg.SortKey != "due" || fg.SortDirection != "asc" {
			t.Errorf("unexpected group shape: %+v", fg)
		}
	})

	t.Run("all", func(t *testing.T) {
		fg := buildListQuery("", "", "", "", true, false)
		if len(fg.Children) != 0 {
			t.Fatalf("expected --all to drop default conditions, got %+v", fg.Children)
		}
		// Marshal-level, not just struct-level: a nil []FilterCondition and
		// an empty-but-non-nil one are both len==0, but marshal to "null"
		// and "[]" respectively — and the real TaskNotes API treats a null
		// children array as "match nothing" rather than "no constraints"
		// (verified live: null -> 0 tasks, [] -> 62 tasks, identical query
		// otherwise). A struct-level check alone would have missed this.
		b, err := json.Marshal(fg)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(b), `"children":null`) {
			t.Errorf("buildListQuery(--all) marshaled children as null, want []: %s", b)
		}
		if !strings.Contains(string(b), `"children":[]`) {
			t.Errorf("expected marshaled children to be [], got: %s", b)
		}
	})

	t.Run("filters", func(t *testing.T) {
		fg := buildListQuery("open", "ProjA", "urgent", "home", false, false)
		if len(fg.Children) != 6 {
			t.Fatalf("expected 2 default + 4 filter conditions, got %d: %+v", len(fg.Children), fg.Children)
		}
		want := []FilterCondition{
			{Type: "condition", ID: "c1", Property: "status", Operator: "is", Value: "open"},
			{Type: "condition", ID: "c2", Property: "projects", Operator: "contains", Value: "ProjA"},
			{Type: "condition", ID: "c3", Property: "tags", Operator: "contains", Value: "urgent"},
			{Type: "condition", ID: "c4", Property: "contexts", Operator: "contains", Value: "home"},
		}
		got := fg.Children[2:]
		for i, w := range want {
			if got[i] != w {
				t.Errorf("condition %d: got %+v, want %+v", i, got[i], w)
			}
		}
	})

	t.Run("startable", func(t *testing.T) {
		fg := buildListQuery("", "", "", "", false, true)
		if len(fg.Children) != 3 {
			t.Fatalf("expected 2 default + 1 startable condition, got %d: %+v", len(fg.Children), fg.Children)
		}
		want := FilterCondition{Type: "condition", ID: "not-blocked", Property: "dependencies.isBlocked", Operator: "is-not-checked"}
		if fg.Children[2] != want {
			t.Errorf("expected startable condition %+v, got %+v", want, fg.Children[2])
		}
	})

	t.Run("json shape round trip", func(t *testing.T) {
		fg := buildListQuery("open", "", "", "", false, false)
		b, err := json.Marshal(fg)
		if err != nil {
			t.Fatal(err)
		}
		var m map[string]any
		if err := json.Unmarshal(b, &m); err != nil {
			t.Fatal(err)
		}
		for _, key := range []string{"type", "id", "conjunction", "children", "sortKey", "sortDirection", "groupKey"} {
			if _, ok := m[key]; !ok {
				t.Errorf("expected key %q in marshaled FilterQuery, got %v", key, m)
			}
		}
	})
}

// TestQueryBuildersNeverMarshalNullChildren guards every FilterQuery
// builder in the codebase against the nil-slice-marshals-to-null trap that
// broke `tn list --all` (buildListQuery used `var children []FilterCondition`,
// which stays nil and marshals to "children":null when no filter flags are
// set — and the live TaskNotes API treats a null children array as "match
// nothing", not "no constraints", unlike an empty array). Struct-level
// len()==0 checks don't distinguish nil from empty-non-nil, so this asserts
// on the actual marshaled JSON, for every builder and every nested group
// within it (a nested OR group with nil children would be just as broken).
func TestQueryBuildersNeverMarshalNullChildren(t *testing.T) {
	assertNoNullChildren := func(t *testing.T, name string, v any) {
		t.Helper()
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("%s: marshal failed: %v", name, err)
		}
		if strings.Contains(string(b), `"children":null`) {
			t.Errorf("%s: marshaled with \"children\":null (should be [] or omitted): %s", name, b)
		}
	}

	assertNoNullChildren(t, "buildListQuery(default)", buildListQuery("", "", "", "", false, false))
	assertNoNullChildren(t, "buildListQuery(--all)", buildListQuery("", "", "", "", true, false))
	assertNoNullChildren(t, "buildListQuery(--all --startable)", buildListQuery("", "", "", "", true, true))
	assertNoNullChildren(t, "buildStatusTaskCountsQuery", buildStatusTaskCountsQuery())
	assertNoNullChildren(t, "buildDueTasksQuery", buildDueTasksQuery("2026-01-01"))
	assertNoNullChildren(t, "buildUnblockedDependentsQuery", buildUnblockedDependentsQuery())
	assertNoNullChildren(t, "buildMRWatchQuery", buildMRWatchQuery())
	assertNoNullChildren(t, "buildRecentlyModifiedQuery", buildRecentlyModifiedQuery("2026-01-01"))
	assertNoNullChildren(t, "buildStartableOpenTasksQuery", buildStartableOpenTasksQuery())
	assertNoNullChildren(t, "buildNeedsActionQuery", buildNeedsActionQuery())
}

// TestUsageDocumentsAllFlags statically scans every cmdXxx function's
// flag.NewFlagSet registrations (via go/ast, not a hand-maintained parallel
// list — a second hardcoded list would drift exactly like the usage string
// did) and fails if any registered flag name never appears in the usage
// string. This is the regression guard for `tn list --startable` existing
// and working but being absent from `tn help`, which let a headless
// orchestrator conclude the flag didn't exist.
func TestUsageDocumentsAllFlags(t *testing.T) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("could not determine test file path via runtime.Caller")
	}
	dir := filepath.Dir(thisFile)

	fset := token.NewFileSet()
	flagNames := map[string]bool{}

	for _, name := range []string{"main.go", "bridge.go"} {
		path := filepath.Join(dir, name)
		if _, err := os.Stat(path); err != nil {
			continue
		}
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatalf("parsing %s: %v", name, err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			// Only calls on the "fs" flag.FlagSet variable every cmdXxx
			// function uses — avoids false matches on unrelated .Var etc.
			recv, ok := sel.X.(*ast.Ident)
			if !ok || recv.Name != "fs" {
				return true
			}
			var nameArgIdx int
			switch sel.Sel.Name {
			case "String", "Bool", "Int":
				nameArgIdx = 0
			case "Var":
				nameArgIdx = 1 // fs.Var(&dest, "name", "usage")
			default:
				return true
			}
			if len(call.Args) <= nameArgIdx {
				return true
			}
			lit, ok := call.Args[nameArgIdx].(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			v, err := strconv.Unquote(lit.Value)
			if err != nil {
				return true
			}
			flagNames[v] = true
			return true
		})
	}

	if len(flagNames) == 0 {
		t.Fatal("AST scan found zero registered flags — test setup is broken, not the CLI")
	}

	for name := range flagNames {
		needle := "--" + name
		if !strings.Contains(usage, needle) {
			t.Errorf("usage text is missing %q: a flag is registered via flag.NewFlagSet but not documented in `tn help`", needle)
		}
	}
}

// TestListSendsQuery verifies `tn list`'s request goes to POST
// /api/tasks/query with the built filter as the body.
func TestListSendsQuery(t *testing.T) {
	var gotMethod, gotPath string
	var gotBody FilterGroup
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"data":{"tasks":[{"path":"a.md","title":"A"}],"total":1,"filtered":1}}`))
	}))
	defer srv.Close()

	c := newTestClient(srv, "")
	fg := buildListQuery("open", "", "", "", false, false)
	tasks, err := c.QueryTasks(fg)
	if err != nil {
		t.Fatal(err)
	}
	if gotMethod != http.MethodPost || gotPath != "/api/tasks/query" {
		t.Fatalf("expected POST /api/tasks/query, got %s %s", gotMethod, gotPath)
	}
	if len(gotBody.Children) != 3 {
		t.Fatalf("expected 3 conditions to be sent, got %+v", gotBody.Children)
	}
	if len(tasks) != 1 || tasks[0].Path != "a.md" {
		t.Fatalf("unexpected tasks: %+v", tasks)
	}
}

// TestFormatHistoryEntry verifies the "**timestamp — by:** text" entry
// format (SPEC-note-layout.md) — the successor to the old flat-append
// appendNote, now used to build ONE history entry that applyNoteBodyEdit
// prepends into a history block rather than concatenating onto details.
func TestFormatHistoryEntry(t *testing.T) {
	now := time.Date(2026, 8, 17, 14, 30, 0, 0, time.Local)
	got := formatHistoryEntry("claude", "did a thing", now)
	want := "**2026-08-17 14:30 — claude:** did a thing"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// TestAddNoteRoundTrip verifies the GET-then-PUT round trip performed by
// `tn note`: the server first returns a task with some details, then the
// client PUTs back the correctly appended details.
func TestAddNoteRoundTrip(t *testing.T) {
	const path = "Notes/Task.md"
	var getCount, putCount int
	var putDetails string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.Method {
		case http.MethodGet:
			getCount++
			w.Write([]byte(`{"data":{"path":"Notes/Task.md","title":"T","details":"Original details."}}`))
		case http.MethodPut:
			putCount++
			var body map[string]any
			json.NewDecoder(r.Body).Decode(&body)
			putDetails, _ = body["details"].(string)
			resp, _ := json.Marshal(map[string]any{
				"data": map[string]any{"path": "Notes/Task.md", "title": "T", "details": putDetails},
			})
			w.Write(resp)
		default:
			t.Fatalf("unexpected method %s", r.Method)
		}
	}))
	defer srv.Close()

	c := newTestClient(srv, "")
	task, err := c.AddNote(path, "checked the logs", "")
	if err != nil {
		t.Fatal(err)
	}
	if getCount != 1 || putCount != 1 {
		t.Fatalf("expected 1 GET and 1 PUT, got GET=%d PUT=%d", getCount, putCount)
	}
	if want := "Original details.\n\n"; putDetails[:len(want)] != want {
		t.Errorf("expected the description preserved before the history block, got %q", putDetails)
	}
	if !strings.Contains(putDetails, "<!-- tn:history -->") || !strings.Contains(putDetails, "History (1)") {
		t.Errorf("expected a history block with a count of 1, got %q", putDetails)
	}
	if !strings.Contains(putDetails, "claude:** checked the logs") {
		t.Errorf("expected default 'claude' author and the note text, got %q", putDetails)
	}
	_ = task
}

// TestPathEscaping verifies a task path containing slashes and spaces is
// correctly percent-encoded in the request URL, and that the server-side
// decoded path matches the original.
func TestPathEscaping(t *testing.T) {
	const original = "TaskNotes/My Folder/Foo Bar.md"

	var decodedPath string
	var rawURLPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// r.URL.Path is already decoded by net/http.
		decodedPath = r.URL.Path
		rawURLPath = r.URL.EscapedPath()
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"data":{"path":"TaskNotes/My Folder/Foo Bar.md","title":"Foo Bar"}}`))
	}))
	defer srv.Close()

	c := newTestClient(srv, "")
	task, err := c.GetTask(original)
	if err != nil {
		t.Fatal(err)
	}

	wantDecoded := "/api/tasks/" + original
	if decodedPath != wantDecoded {
		t.Errorf("server saw decoded path %q, want %q", decodedPath, wantDecoded)
	}

	// The raw (escaped) URL must not contain literal slashes/spaces beyond
	// the fixed "/api/tasks/" prefix, proving PathEscape was used.
	escapedSuffix := rawURLPath[len("/api/tasks/"):]
	if want := url.PathEscape(original); escapedSuffix != want {
		t.Errorf("escaped path suffix = %q, want %q", escapedSuffix, want)
	}

	if task.Title != "Foo Bar" {
		t.Errorf("unexpected task: %+v", task)
	}
}

// TestConnRefused checks the friendly error is returned when the API is
// unreachable.
func TestConnRefused(t *testing.T) {
	c := NewClient(Config{URL: "http://127.0.0.1:1", Token: ""})
	_, err := c.Health()
	if err == nil {
		t.Fatal("expected an error connecting to a closed port")
	}
}

// TestHTTPError checks non-2xx responses are surfaced as *httpError.
func TestHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(`{"error":"not found"}`))
	}))
	defer srv.Close()

	c := newTestClient(srv, "")
	_, err := c.GetTask("missing.md")
	if err == nil {
		t.Fatal("expected an error")
	}
	he, ok := err.(*httpError)
	if !ok {
		t.Fatalf("expected *httpError, got %T: %v", err, err)
	}
	if he.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", he.Code)
	}
}

// TestBuildBlockedByField verifies each repeated --blocked-by value becomes
// one {"uid": <value verbatim>, "reltype": "FINISHTOSTART"} entry, and that
// values (plain paths or explicit wikilinks) are passed through unchanged.
func TestBuildBlockedByField(t *testing.T) {
	got := buildBlockedByField([]string{"Tasks/A.md", "[[Tasks/B]]"})
	want := []map[string]any{
		{"uid": "Tasks/A.md", "reltype": "FINISHTOSTART"},
		{"uid": "[[Tasks/B]]", "reltype": "FINISHTOSTART"},
	}
	if len(got) != len(want) {
		t.Fatalf("expected %d entries, got %d: %+v", len(want), len(got), got)
	}
	for i, w := range want {
		if got[i]["uid"] != w["uid"] || got[i]["reltype"] != w["reltype"] {
			t.Errorf("entry %d: got %+v, want %+v", i, got[i], w)
		}
	}

	if got := buildBlockedByField(nil); len(got) != 0 {
		t.Errorf("expected an empty (non-nil) slice for no values, got %+v", got)
	}
}

// TestCmdCreateSendsBlockedBy verifies `tn create --blocked-by ...` puts
// the blockedBy array in the POST /api/tasks request body.
func TestCmdCreateSendsBlockedBy(t *testing.T) {
	t.Setenv("TN_BRIDGE_URL", "http://127.0.0.1:1") // force the bridge-unreachable fallback path deterministically
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"data":{"path":"Tasks/New.md","title":"New"}}`))
	}))
	defer srv.Close()

	c := newTestClient(srv, "")
	err := cmdCreate(c, []string{
		"--title", "New", "--blocked-by", "Tasks/A.md", "--blocked-by", "[[Tasks/B]]",
	})
	if err != nil {
		t.Fatal(err)
	}

	blockedBy, ok := gotBody["blockedBy"].([]any)
	if !ok || len(blockedBy) != 2 {
		t.Fatalf("expected a 2-entry blockedBy array in the request body, got %+v", gotBody["blockedBy"])
	}
	first := blockedBy[0].(map[string]any)
	if first["uid"] != "Tasks/A.md" || first["reltype"] != "FINISHTOSTART" {
		t.Errorf("unexpected first entry: %+v", first)
	}
}

// TestCmdUpdateSendsBlockedBy verifies `tn update <path> --blocked-by ...`
// puts the blockedBy array in the PUT /api/tasks/:id request body, and that
// omitting the flag entirely leaves it out (partial-update semantics).
func TestCmdUpdateSendsBlockedBy(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"data":{"path":"Tasks/Existing.md","title":"Existing"}}`))
	}))
	defer srv.Close()

	c := newTestClient(srv, "")
	err := cmdUpdate(c, []string{"Tasks/Existing.md", "--blocked-by", "Tasks/A.md"})
	if err != nil {
		t.Fatal(err)
	}

	blockedBy, ok := gotBody["blockedBy"].([]any)
	if !ok || len(blockedBy) != 1 {
		t.Fatalf("expected a 1-entry blockedBy array, got %+v", gotBody["blockedBy"])
	}
	entry := blockedBy[0].(map[string]any)
	if entry["uid"] != "Tasks/A.md" || entry["reltype"] != "FINISHTOSTART" {
		t.Errorf("unexpected entry: %+v", entry)
	}
	if _, ok := gotBody["status"]; ok {
		t.Errorf("expected no status field when --status wasn't passed, got %+v", gotBody)
	}
}

// TestCmdCreateMergesCustomProperties verifies `tn create --claude-project
// ... --mr ...` puts both keys into ONE customProperties object, rather
// than the second flag clobbering the first.
func TestCmdCreateMergesCustomProperties(t *testing.T) {
	t.Setenv("TN_BRIDGE_URL", "http://127.0.0.1:1") // force the bridge-unreachable fallback path deterministically
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"data":{"path":"Tasks/New.md","title":"New"}}`))
	}))
	defer srv.Close()

	c := newTestClient(srv, "")
	err := cmdCreate(c, []string{
		"--title", "New", "--claude-project", "myapp", "--mr", "https://example.com/mr/1",
	})
	if err != nil {
		t.Fatal(err)
	}

	cp, ok := gotBody["customProperties"].(map[string]any)
	if !ok {
		t.Fatalf("expected a customProperties object, got %+v", gotBody["customProperties"])
	}
	if cp["claude-project"] != "myapp" || cp["mr"] != "https://example.com/mr/1" {
		t.Errorf("expected both keys merged into one object, got %+v", cp)
	}
}

// TestCmdUpdateMergesCustomProperties mirrors the create case for `tn
// update`: both --claude-project and --mr land in ONE customProperties
// object in the PUT body.
func TestCmdUpdateMergesCustomProperties(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"data":{"path":"Tasks/Existing.md","title":"Existing"}}`))
	}))
	defer srv.Close()

	c := newTestClient(srv, "")
	err := cmdUpdate(c, []string{
		"Tasks/Existing.md", "--claude-project", "myapp", "--mr", "https://example.com/mr/2",
	})
	if err != nil {
		t.Fatal(err)
	}

	cp, ok := gotBody["customProperties"].(map[string]any)
	if !ok {
		t.Fatalf("expected a customProperties object, got %+v", gotBody["customProperties"])
	}
	if cp["claude-project"] != "myapp" || cp["mr"] != "https://example.com/mr/2" {
		t.Errorf("expected both keys merged into one object, got %+v", cp)
	}
}

// TestCmdUpdate_DetailsPreservesHistoryAndOtherManagedBlocks is the
// regression test for a HIGH-priority data-loss bug (reported by g34005,
// reproduced by team-lead, 2026-08-26): `tn update <path> --details TEXT`
// sent the flag's raw text as a LITERAL wholesale replacement for the
// task's entire `details` field — UpdateTask's partial-update semantics
// replace the field outright, not merge it — silently destroying the
// managed history block (and, unverified until this test, the ask/brief
// blocks too) on every call. The task that surfaced this in production
// held 102 history entries across 187,869 bytes, built over many retired
// generations; one --details write would have destroyed all of it
// irrecoverably. Deliberately a REALISTIC, dense fixture (many real
// history entries via cmdNote, not one or two synthetic strings) — see
// the Evidence-line-regex lesson (SPEC-serve.md) on why a synthetic
// fixture would not have caught this class of bug: a small fixture makes
// "did the whole block survive" indistinguishable from "did the one
// entry I happened to check survive."
func TestCmdUpdate_DetailsPreservesHistoryAndOtherManagedBlocks(t *testing.T) {
	ts := statefulTaskServer(t, "Tasks/Dense.md", "Dense")
	c := newTestClient(ts, "")

	if err := cmdAsk(c, []string{"Tasks/Dense.md", "What should the fallback behavior be?"}); err != nil {
		t.Fatalf("seeding ask block failed: %v", err)
	}
	if err := cmdBrief(c, []string{"Tasks/Dense.md", "Currently blocked on VM quota."}); err != nil {
		t.Fatalf("seeding brief block failed: %v", err)
	}
	const wantEntries = 25
	for i := 0; i < wantEntries; i++ {
		text := fmt.Sprintf("Generation g%d: investigated failure mode #%d, ruled out the obvious cause, filed a follow-up.", i, i)
		if err := cmdNote(c, []string{"Tasks/Dense.md", text}); err != nil {
			t.Fatalf("seeding history entry %d failed: %v", i, err)
		}
	}

	before, err := c.GetTask("Tasks/Dense.md")
	if err != nil {
		t.Fatal(err)
	}
	nbBefore, ok := parseNoteBody(before.Details)
	if !ok {
		t.Fatalf("expected a structured body before the update, got %q", before.Details)
	}
	if len(nbBefore.History) != wantEntries {
		t.Fatalf("fixture setup failed: expected %d history entries before the update, got %d", wantEntries, len(nbBefore.History))
	}
	if !strings.Contains(before.Details, "tn:history") {
		t.Fatalf("fixture setup failed: expected a history marker before the update")
	}
	beforeLen := len(before.Details)

	if err := cmdUpdate(c, []string{"Tasks/Dense.md", "--details", "Root cause finally identified: stale lockfile."}); err != nil {
		t.Fatalf("cmdUpdate --details failed: %v", err)
	}

	after, err := c.GetTask("Tasks/Dense.md")
	if err != nil {
		t.Fatal(err)
	}
	nbAfter, ok := parseNoteBody(after.Details)
	if !ok {
		t.Fatalf("expected a structured body after the update, got %q", after.Details)
	}

	if len(nbAfter.History) != wantEntries {
		t.Errorf("HISTORY DESTROYED: expected all %d entries to survive --details, got %d (before had %d bytes, after has %d bytes)",
			wantEntries, len(nbAfter.History), beforeLen, len(after.Details))
	}
	if !strings.Contains(after.Details, "tn:history") {
		t.Error("HISTORY MARKER DESTROYED: expected <!-- tn:history --> to survive --details")
	}
	if nbAfter.Ask != nbBefore.Ask {
		t.Errorf("ASK BLOCK DESTROYED: expected %q preserved, got %q", nbBefore.Ask, nbAfter.Ask)
	}
	if nbAfter.Brief != nbBefore.Brief {
		t.Errorf("BRIEF BLOCK DESTROYED: expected %q preserved, got %q", nbBefore.Brief, nbAfter.Brief)
	}
	if nbAfter.Description != "Root cause finally identified: stale lockfile." {
		t.Errorf("expected the --details text applied to Description, got %q", nbAfter.Description)
	}
}

// TestCmdUpdate_DetailsBareTaskStillReplacesNormally verifies the fix
// doesn't overcorrect: a task with no managed blocks at all (never
// noted/asked/briefed — just a plain description, the common case for a
// freshly created task) still gets --details applied as a normal
// replacement, no refusal, no history to protect because none exists.
func TestCmdUpdate_DetailsBareTaskStillReplacesNormally(t *testing.T) {
	ts := statefulTaskServer(t, "Tasks/Bare.md", "Bare")
	c := newTestClient(ts, "")

	if err := cmdUpdate(c, []string{"Tasks/Bare.md", "--details", "Initial plain description."}); err != nil {
		t.Fatalf("initial update failed: %v", err)
	}
	if err := cmdUpdate(c, []string{"Tasks/Bare.md", "--details", "Replaced plain description."}); err != nil {
		t.Fatalf("second update failed: %v", err)
	}

	task, err := c.GetTask("Tasks/Bare.md")
	if err != nil {
		t.Fatal(err)
	}
	nb, ok := parseNoteBody(task.Details)
	if ok {
		// A bare description with no anchors legitimately parses as
		// unstructured (ok=false) — either shape is fine here, only the
		// CONTENT matters for this test.
		if nb.Description != "Replaced plain description." {
			t.Errorf("expected the description replaced, got %q", nb.Description)
		}
	} else if strings.TrimSpace(task.Details) != "Replaced plain description." {
		t.Errorf("expected the description replaced, got %q", task.Details)
	}
}

// TestLooksLikeItHasHistoryContent is an isolated, honest test of the
// belt-and-braces DETECTOR cmdUpdate's --details refusal path relies on
// — deliberately NOT routed through cmdUpdate/migrateLegacyBody, because
// migrateLegacyBody's own bail-out (returning ok=false) is documented as
// STRUCTURALLY UNREACHABLE through its real code path (see
// TestParseOrMigrateNoteBody_BailOutContract in note_layout_test.go) —
// constructing a fixture that organically reaches cmdUpdate's refusal
// branch would mean either lying about what the test proves or shipping
// a test that silently exercises the SAFE path while claiming to prove
// the refusal fires. The refusal branch itself stays in cmdUpdate as
// cheap insurance against a future change to migrateLegacyBody that
// reintroduces a real bail-out; this test only pins down that its
// trigger condition (looksLikeItHasHistoryContent) correctly recognizes
// the two shapes it's meant to catch.
func TestLooksLikeItHasHistoryContent(t *testing.T) {
	cases := []struct {
		name    string
		details string
		want    bool
	}{
		{"real history anchor", "some text <!-- tn:history --> more text", true},
		{"legacy entry header", "**2026-08-20 10:00 — bridge:** An old note entry.", true},
		{"plain description, no markers", "Just a plain description with no structure at all.", false},
		{"empty", "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := looksLikeItHasHistoryContent(c.details); got != c.want {
				t.Errorf("looksLikeItHasHistoryContent(%q) = %v, want %v", c.details, got, c.want)
			}
		})
	}
}

// TestCmdUpdate_ReplaceHistoryFlagAllowsItAndWarns verifies the explicit
// opt-out: --replace-history lets --details fall back to the old literal
// replacement, but must print exactly how many history entries it just
// removed — silent data loss must never recur even through an
// intentional escape hatch.
func TestCmdUpdate_ReplaceHistoryFlagAllowsItAndWarns(t *testing.T) {
	ts := statefulTaskServer(t, "Tasks/Replace.md", "Replace")
	c := newTestClient(ts, "")

	for i := 0; i < 3; i++ {
		if err := cmdNote(c, []string{"Tasks/Replace.md", fmt.Sprintf("Entry %d", i)}); err != nil {
			t.Fatalf("seeding history entry %d failed: %v", i, err)
		}
	}

	out := captureStdout(t, func() {
		if err := cmdUpdate(c, []string{"Tasks/Replace.md", "--details", "Fresh start.", "--replace-history"}); err != nil {
			t.Fatalf("cmdUpdate with --replace-history failed: %v", err)
		}
	})

	if !strings.Contains(out, "warning: this write removed 3 history entries") {
		t.Errorf("expected the exact removal-count warning, got: %q", out)
	}

	task, err := c.GetTask("Tasks/Replace.md")
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(task.Details) != "Fresh start." {
		t.Errorf("expected the literal replacement to have gone through, got %q", task.Details)
	}
}

// TestCmdUpdateMrOnly verifies `tn update <path> --mr URL` alone produces a
// customProperties object containing just "mr" (no stray "claude-project"
// key or empty-string clobbering).
func TestCmdUpdateMrOnly(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"data":{"path":"Tasks/Existing.md","title":"Existing"}}`))
	}))
	defer srv.Close()

	c := newTestClient(srv, "")
	err := cmdUpdate(c, []string{"Tasks/Existing.md", "--mr", "https://example.com/mr/3"})
	if err != nil {
		t.Fatal(err)
	}

	cp, ok := gotBody["customProperties"].(map[string]any)
	if !ok {
		t.Fatalf("expected a customProperties object, got %+v", gotBody["customProperties"])
	}
	if len(cp) != 1 || cp["mr"] != "https://example.com/mr/3" {
		t.Errorf("expected only mr in customProperties, got %+v", cp)
	}
}

// TestCmdCreate_MrSeedsLinksBlockFirst verifies `tn create --mr` seeds the
// new task's links block with an "MR" entry (SPEC-note-layout.md addendum),
// alongside any --details text as the description.
func TestCmdCreate_MrSeedsLinksBlockFirst(t *testing.T) {
	t.Setenv("TN_BRIDGE_URL", "http://127.0.0.1:1") // force the bridge-unreachable fallback path deterministically
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"data":{"path":"Tasks/New.md","title":"New"}}`))
	}))
	defer srv.Close()

	c := newTestClient(srv, "")
	err := cmdCreate(c, []string{
		"--title", "New", "--mr", "https://gitlab.com/x/y/-/merge_requests/42", "--details", "Some description.",
	})
	if err != nil {
		t.Fatal(err)
	}

	details, _ := gotBody["details"].(string)
	nb, ok := parseNoteBody(details)
	if !ok {
		t.Fatalf("expected a structured body, got %q", details)
	}
	if nb.Links != "- MR: https://gitlab.com/x/y/-/merge_requests/42" {
		t.Errorf("expected the MR link as the only entry, got %q", nb.Links)
	}
	if nb.Description != "Some description." {
		t.Errorf("expected the --details text preserved as description, got %q", nb.Description)
	}
}

// TestCmdUpdate_MrSeedsLinksBlockFirst verifies `tn update --mr` puts the
// MR link FIRST in the links block, preserving an existing manual entry
// added earlier via `tn link`.
func TestCmdUpdate_MrSeedsLinksBlockFirst(t *testing.T) {
	ts := statefulTaskServer(t, "Tasks/Existing.md", "Existing")
	c := newTestClient(ts, "")

	if err := cmdLink(c, []string{"Tasks/Existing.md", "https://example.com/shot.png", "--label", "Artifact"}); err != nil {
		t.Fatalf("seeding the artifact link failed: %v", err)
	}

	if err := cmdUpdate(c, []string{"Tasks/Existing.md", "--mr", "https://gitlab.com/x/y/-/merge_requests/7"}); err != nil {
		t.Fatalf("update --mr failed: %v", err)
	}

	task, err := c.GetTask("Tasks/Existing.md")
	if err != nil {
		t.Fatal(err)
	}
	nb, ok := parseNoteBody(task.Details)
	if !ok {
		t.Fatalf("expected structured parse, got %q", task.Details)
	}
	want := "- MR: https://gitlab.com/x/y/-/merge_requests/7\n- Artifact: https://example.com/shot.png"
	if nb.Links != want {
		t.Fatalf("expected MR first, artifact preserved:\ngot:  %q\nwant: %q", nb.Links, want)
	}
}

// TestCmdUpdate_MrWithExplicitDetailsUsesGivenText verifies --mr combined
// with an explicit --details in the SAME call applies the links transform
// to the user's own --details text, never silently discarding it in favor
// of a stale GET.
func TestCmdUpdate_MrWithExplicitDetailsUsesGivenText(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"data":{"path":"Tasks/Existing.md","title":"Existing"}}`))
	}))
	defer srv.Close()

	c := newTestClient(srv, "")
	err := cmdUpdate(c, []string{
		"Tasks/Existing.md", "--details", "New description.", "--mr", "https://gitlab.com/x/y/-/merge_requests/8",
	})
	if err != nil {
		t.Fatal(err)
	}

	details, _ := gotBody["details"].(string)
	nb, ok := parseNoteBody(details)
	if !ok {
		t.Fatalf("expected structured body, got %q", details)
	}
	if nb.Description != "New description." {
		t.Errorf("expected the explicit --details text preserved, got %q", nb.Description)
	}
	if nb.Links != "- MR: https://gitlab.com/x/y/-/merge_requests/8" {
		t.Errorf("expected the MR link, got %q", nb.Links)
	}
}

// TestCmdCreate_IntegrationBranchFlag verifies --integration-branch lands
// in customProperties, combined with --mr in the SAME merge (mind the
// existing merge helper — both keys must land in one customProperties
// object, not two separate PUT/POST fields).
func TestCmdCreate_IntegrationBranchFlag(t *testing.T) {
	t.Setenv("TN_BRIDGE_URL", "http://127.0.0.1:1") // force the bridge-unreachable fallback path deterministically
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"data":{"path":"Tasks/New.md","title":"New"}}`))
	}))
	defer srv.Close()

	c := newTestClient(srv, "")
	err := cmdCreate(c, []string{
		"--title", "New", "--integration-branch", "feat/e2e-recorder", "--mr", "https://example.com/mr/1",
	})
	if err != nil {
		t.Fatal(err)
	}

	cp, ok := gotBody["customProperties"].(map[string]any)
	if !ok {
		t.Fatalf("expected a customProperties object, got %+v", gotBody["customProperties"])
	}
	if cp["integration-branch"] != "feat/e2e-recorder" || cp["mr"] != "https://example.com/mr/1" {
		t.Errorf("expected both keys merged into one object, got %+v", cp)
	}
}

// TestCmdUpdate_IntegrationBranchFlag mirrors the create case for `tn
// update`, combined with --mr in one customProperties object.
func TestCmdUpdate_IntegrationBranchFlag(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"data":{"path":"Tasks/Existing.md","title":"Existing"}}`))
	}))
	defer srv.Close()

	c := newTestClient(srv, "")
	err := cmdUpdate(c, []string{
		"Tasks/Existing.md", "--integration-branch", "feat/e2e-recorder", "--mr", "https://example.com/mr/2",
	})
	if err != nil {
		t.Fatal(err)
	}

	cp, ok := gotBody["customProperties"].(map[string]any)
	if !ok {
		t.Fatalf("expected a customProperties object, got %+v", gotBody["customProperties"])
	}
	if cp["integration-branch"] != "feat/e2e-recorder" || cp["mr"] != "https://example.com/mr/2" {
		t.Errorf("expected both keys merged into one object, got %+v", cp)
	}
}

// fakeParentAndCreateAPI simulates the two calls `tn create --from` makes:
// GET the parent task (parentFound controls a 404 vs a real response,
// carrying parentCustomProps), then POST /api/tasks for the actual create,
// capturing that POST body.
func fakeParentAndCreateAPI(t *testing.T, parentPath string, parentCustomProps map[string]string, parentFound bool) (*httptest.Server, *map[string]any, *bool) {
	t.Helper()
	gotBody := map[string]any{}
	createCalled := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet {
			if !parentFound {
				w.WriteHeader(http.StatusNotFound)
				json.NewEncoder(w).Encode(map[string]any{"error": "not found"})
				return
			}
			json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{
				"path": parentPath, "title": "Parent", "customProperties": parentCustomProps,
			}})
			return
		}
		createCalled = true
		json.NewDecoder(r.Body).Decode(&gotBody)
		w.Write([]byte(`{"data":{"path":"Tasks/Child.md","title":"Child"}}`))
	}))
	t.Cleanup(srv.Close)
	return srv, &gotBody, &createCalled
}

// TestCmdCreate_FromInheritsBothFields verifies `tn create --from <parent>`
// with no explicit --claude-project/--integration-branch copies both from
// the parent's own customProperties (never a repo-note or other source).
func TestCmdCreate_FromInheritsBothFields(t *testing.T) {
	t.Setenv("TN_BRIDGE_URL", "http://127.0.0.1:1") // force the bridge-unreachable fallback path deterministically
	srv, gotBody, _ := fakeParentAndCreateAPI(t, "Tasks/Parent.md",
		map[string]string{"claude-project": "myapp", "integration-branch": "feat/e2e-recorder"}, true)
	c := newTestClient(srv, "")

	err := cmdCreate(c, []string{"--title", "Child", "--from", "Tasks/Parent.md"})
	if err != nil {
		t.Fatal(err)
	}

	cp, ok := (*gotBody)["customProperties"].(map[string]any)
	if !ok {
		t.Fatalf("expected a customProperties object, got %+v", (*gotBody)["customProperties"])
	}
	if cp["claude-project"] != "myapp" || cp["integration-branch"] != "feat/e2e-recorder" {
		t.Errorf("expected both fields inherited from the parent, got %+v", cp)
	}
}

// TestCmdCreate_FromExplicitFlagsOverrideInherited verifies explicit
// --claude-project/--integration-branch on the SAME command line win over
// whatever --from would have inherited.
func TestCmdCreate_FromExplicitFlagsOverrideInherited(t *testing.T) {
	t.Setenv("TN_BRIDGE_URL", "http://127.0.0.1:1") // force the bridge-unreachable fallback path deterministically
	srv, gotBody, _ := fakeParentAndCreateAPI(t, "Tasks/Parent.md",
		map[string]string{"claude-project": "myapp", "integration-branch": "feat/e2e-recorder"}, true)
	c := newTestClient(srv, "")

	err := cmdCreate(c, []string{
		"--title", "Child", "--from", "Tasks/Parent.md",
		"--claude-project", "different-project", "--integration-branch", "feat/other-branch",
	})
	if err != nil {
		t.Fatal(err)
	}

	cp, ok := (*gotBody)["customProperties"].(map[string]any)
	if !ok {
		t.Fatalf("expected a customProperties object, got %+v", (*gotBody)["customProperties"])
	}
	if cp["claude-project"] != "different-project" || cp["integration-branch"] != "feat/other-branch" {
		t.Errorf("expected the explicit flags to win over inherited values, got %+v", cp)
	}
}

// TestCmdCreate_IntegrationRoleFlag verifies --integration-role owner
// lands in customProperties, combined with --integration-branch in the
// same merge.
func TestCmdCreate_IntegrationRoleFlag(t *testing.T) {
	t.Setenv("TN_BRIDGE_URL", "http://127.0.0.1:1") // force the bridge-unreachable fallback path deterministically
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"data":{"path":"Tasks/New.md","title":"New"}}`))
	}))
	defer srv.Close()

	c := newTestClient(srv, "")
	err := cmdCreate(c, []string{
		"--title", "New", "--integration-branch", "feat/e2e-recorder", "--integration-role", "owner",
	})
	if err != nil {
		t.Fatal(err)
	}

	cp, ok := gotBody["customProperties"].(map[string]any)
	if !ok {
		t.Fatalf("expected a customProperties object, got %+v", gotBody["customProperties"])
	}
	if cp["integration-branch"] != "feat/e2e-recorder" || cp["integration-role"] != "owner" {
		t.Errorf("expected both keys merged into one object, got %+v", cp)
	}
}

// TestCmdUpdate_IntegrationRoleFlag mirrors the create case for `tn
// update`.
func TestCmdUpdate_IntegrationRoleFlag(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"data":{"path":"Tasks/Existing.md","title":"Existing"}}`))
	}))
	defer srv.Close()

	c := newTestClient(srv, "")
	err := cmdUpdate(c, []string{"Tasks/Existing.md", "--integration-role", "owner"})
	if err != nil {
		t.Fatal(err)
	}

	cp, ok := gotBody["customProperties"].(map[string]any)
	if !ok {
		t.Fatalf("expected a customProperties object, got %+v", gotBody["customProperties"])
	}
	if len(cp) != 1 || cp["integration-role"] != "owner" {
		t.Errorf("expected only integration-role in customProperties, got %+v", cp)
	}
}

// TestCmdCreate_JiraFlags verifies --jira and --jira-nc both land in
// customProperties, merged with the other custom-property flags.
func TestCmdCreate_JiraFlags(t *testing.T) {
	t.Setenv("TN_BRIDGE_URL", "http://127.0.0.1:1") // force the bridge-unreachable fallback path deterministically
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"data":{"path":"Tasks/New.md","title":"New"}}`))
	}))
	defer srv.Close()

	c := newTestClient(srv, "")
	err := cmdCreate(c, []string{"--title", "New", "--jira", "CX-1,CX-2", "--jira-nc", "NC-1"})
	if err != nil {
		t.Fatal(err)
	}

	cp, ok := gotBody["customProperties"].(map[string]any)
	if !ok {
		t.Fatalf("expected a customProperties object, got %+v", gotBody["customProperties"])
	}
	if cp["jira"] != "CX-1,CX-2" || cp["jira-nc"] != "NC-1" {
		t.Errorf("expected both jira fields set, got %+v", cp)
	}
}

// TestCmdUpdate_JiraFlags mirrors the create case for `tn update`.
func TestCmdUpdate_JiraFlags(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"data":{"path":"Tasks/Existing.md","title":"Existing"}}`))
	}))
	defer srv.Close()

	c := newTestClient(srv, "")
	err := cmdUpdate(c, []string{"Tasks/Existing.md", "--jira", "CX-3", "--jira-nc", "NC-2"})
	if err != nil {
		t.Fatal(err)
	}

	cp, ok := gotBody["customProperties"].(map[string]any)
	if !ok {
		t.Fatalf("expected a customProperties object, got %+v", gotBody["customProperties"])
	}
	if cp["jira"] != "CX-3" || cp["jira-nc"] != "NC-2" {
		t.Errorf("expected both jira fields set, got %+v", cp)
	}
}

// TestCmdCreate_FromDoesNotCopyIntegrationRole verifies `tn create --from`
// inherits integration-branch (and claude-project) but NEVER
// integration-role, even when the parent is itself the workstream owner —
// a child task must never silently inherit ownership of the parent's
// consolidated MR.
func TestCmdCreate_FromDoesNotCopyIntegrationRole(t *testing.T) {
	t.Setenv("TN_BRIDGE_URL", "http://127.0.0.1:1") // force the bridge-unreachable fallback path deterministically
	srv, gotBody, _ := fakeParentAndCreateAPI(t, "Tasks/Parent.md",
		map[string]string{
			"claude-project": "myapp", "integration-branch": "feat/e2e-recorder", "integration-role": "owner",
		}, true)
	c := newTestClient(srv, "")

	err := cmdCreate(c, []string{"--title", "Child", "--from", "Tasks/Parent.md"})
	if err != nil {
		t.Fatal(err)
	}

	cp, ok := (*gotBody)["customProperties"].(map[string]any)
	if !ok {
		t.Fatalf("expected a customProperties object, got %+v", (*gotBody)["customProperties"])
	}
	if cp["integration-branch"] != "feat/e2e-recorder" {
		t.Errorf("expected integration-branch inherited, got %+v", cp)
	}
	if _, hasRole := cp["integration-role"]; hasRole {
		t.Errorf("expected integration-role NEVER inherited via --from, got %+v", cp)
	}
}

// TestCmdCreate_FromMissingParentErrorsNoTaskCreated verifies an
// unreadable/missing parent fails loudly and never calls POST /api/tasks —
// no task is silently created without its intended inheritance.
func TestCmdCreate_FromMissingParentErrorsNoTaskCreated(t *testing.T) {
	srv, _, createCalled := fakeParentAndCreateAPI(t, "Tasks/Ghost.md", nil, false)
	c := newTestClient(srv, "")

	err := cmdCreate(c, []string{"--title", "Child", "--from", "Tasks/Ghost.md"})
	if err == nil {
		t.Fatal("expected an error for an unreadable parent, got nil")
	}
	if *createCalled {
		t.Error("expected POST /api/tasks to never be called when the parent fetch fails")
	}
}

// fakeNoteAPI simulates enough of the TaskNotes API for cmdNote's
// GET-then-PUT AddNote flow, capturing the PUT body's "details" field.
func fakeNoteAPI(t *testing.T) (*httptest.Server, *map[string]any) {
	t.Helper()
	gotBody := map[string]any{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet {
			w.Write([]byte(`{"data":{"path":"Tasks/Note.md","title":"Note","details":""}}`))
			return
		}
		json.NewDecoder(r.Body).Decode(&gotBody)
		w.Write([]byte(`{"data":{"path":"Tasks/Note.md","title":"Note"}}`))
	}))
	t.Cleanup(srv.Close)
	return srv, &gotBody
}

// TestCmdNote_MissingTextErrors verifies `tn note <path>` with no text
// argument at all (including a bare --by with nothing after it) fails
// loudly rather than silently storing something wrong — this is the exact
// live incident: `tn note <path> --by X` used to store the literal string
// "--by" as the note body.
func TestCmdNote_MissingTextErrors(t *testing.T) {
	srv, gotBody := fakeNoteAPI(t)
	c := newTestClient(srv, "")

	err := cmdNote(c, []string{"Tasks/Note.md", "--by", "X"})
	if err == nil {
		t.Fatal("expected an error for a missing text argument, got nil")
	}
	if len(*gotBody) != 0 {
		t.Errorf("expected no API call to have been made, got body %+v", *gotBody)
	}
}

// TestCmdNote_FlagLikeTextErrors verifies a positional text argument that
// starts with "--" is rejected rather than silently stored.
func TestCmdNote_FlagLikeTextErrors(t *testing.T) {
	srv, gotBody := fakeNoteAPI(t)
	c := newTestClient(srv, "")

	err := cmdNote(c, []string{"Tasks/Note.md", "--something"})
	if err == nil {
		t.Fatal("expected an error for a flag-like text argument, got nil")
	}
	if len(*gotBody) != 0 {
		t.Errorf("expected no API call to have been made, got body %+v", *gotBody)
	}
}

// TestCmdNote_ByFlagWorksInEitherOrder verifies `tn note <path> <text>
// --by NAME` and `tn note <path> --by NAME <text>` both succeed and store
// the same text — the --by flag must not care which side of the text
// argument it's on.
func TestCmdNote_ByFlagWorksInEitherOrder(t *testing.T) {
	t.Run("text then flag", func(t *testing.T) {
		srv, gotBody := fakeNoteAPI(t)
		c := newTestClient(srv, "")
		if err := cmdNote(c, []string{"Tasks/Note.md", "actual text", "--by", "reviewer"}); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		details, _ := (*gotBody)["details"].(string)
		if !strings.Contains(details, "actual text") {
			t.Errorf("expected the note body to contain the text, got %+v", *gotBody)
		}
	})
	t.Run("flag then text", func(t *testing.T) {
		srv, gotBody := fakeNoteAPI(t)
		c := newTestClient(srv, "")
		if err := cmdNote(c, []string{"Tasks/Note.md", "--by", "reviewer", "actual text"}); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		details, _ := (*gotBody)["details"].(string)
		if !strings.Contains(details, "actual text") {
			t.Errorf("expected the note body to contain the text, got %+v", *gotBody)
		}
	})
}

// TestCmdBrief_ReplacesBlock verifies `tn brief` replaces (not appends to)
// the brief block on a second call.
func TestCmdBrief_ReplacesBlock(t *testing.T) {
	srv, gotBody := fakeNoteAPI(t)
	c := newTestClient(srv, "")

	if err := cmdBrief(c, []string{"Tasks/Note.md", "Fix verified on VM4."}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	details, _ := (*gotBody)["details"].(string)
	if !strings.Contains(details, "tn:brief") || !strings.Contains(details, "Fix verified on VM4.") {
		t.Errorf("expected a brief block with the text, got %+v", *gotBody)
	}
	if _, hasStatus := (*gotBody)["status"]; hasStatus {
		t.Errorf("expected tn brief to never touch status, got %+v", *gotBody)
	}
}

// TestCmdAsk_SetsNeedsInputStatus verifies `tn ask <path> <text>` writes
// the ask block AND sets status needs-input in the same call.
func TestCmdAsk_SetsNeedsInputStatus(t *testing.T) {
	srv, gotBody := fakeNoteAPI(t)
	c := newTestClient(srv, "")

	if err := cmdAsk(c, []string{"Tasks/Note.md", "Merge MR !973 or reply."}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	details, _ := (*gotBody)["details"].(string)
	if !strings.Contains(details, "tn:ask") || !strings.Contains(details, "Merge MR !973 or reply.") {
		t.Errorf("expected an ask block with the text, got %+v", *gotBody)
	}
	if (*gotBody)["status"] != "needs-input" {
		t.Errorf("expected status needs-input, got %+v", *gotBody)
	}
}

// TestCmdAsk_ClearRemovesBlockWithoutTouchingStatus verifies `tn ask
// --clear` removes the ask block and does NOT set any status field.
func TestCmdAsk_ClearRemovesBlockWithoutTouchingStatus(t *testing.T) {
	srv, gotBody := fakeNoteAPI(t)
	c := newTestClient(srv, "")

	if err := cmdAsk(c, []string{"--clear", "Tasks/Note.md"}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	details, _ := (*gotBody)["details"].(string)
	if strings.Contains(details, "tn:ask") {
		t.Errorf("expected no ask block, got %+v", *gotBody)
	}
	if _, hasStatus := (*gotBody)["status"]; hasStatus {
		t.Errorf("expected tn ask --clear to never touch status, got %+v", *gotBody)
	}
}

// TestCmdAsk_MissingTextErrors verifies `tn ask <path>` with no text (and
// no --clear) fails loudly rather than silently writing an empty ask
// block.
func TestCmdAsk_MissingTextErrors(t *testing.T) {
	srv, gotBody := fakeNoteAPI(t)
	c := newTestClient(srv, "")

	if err := cmdAsk(c, []string{"Tasks/Note.md"}); err == nil {
		t.Error("expected an error for a missing text argument, got nil")
	}
	if len(*gotBody) != 0 {
		t.Errorf("expected no API call to have been made, got body %+v", *gotBody)
	}
}

// TestCmdAsk_WritesReplyBlockAndPreservesUserReply verifies `tn ask` seeds
// the reply block with its default template alongside the ask block (see
// SPEC-note-layout.md's addendum), but a follow-up `tn ask` never clobbers
// a reply the user has already started typing.
func TestCmdAsk_WritesReplyBlockAndPreservesUserReply(t *testing.T) {
	ts := statefulTaskServer(t, "Tasks/Ask.md", "Ask")
	c := newTestClient(ts, "")

	if err := cmdAsk(c, []string{"Tasks/Ask.md", "Merge or reply?"}); err != nil {
		t.Fatalf("first ask failed: %v", err)
	}
	task, err := c.GetTask("Tasks/Ask.md")
	if err != nil {
		t.Fatal(err)
	}
	nb, ok := parseNoteBody(task.Details)
	if !ok || nb.Reply != defaultReplyBody {
		t.Fatalf("expected the default reply template seeded, ok=%v reply=%q", ok, nb.Reply)
	}

	// Simulate the user replying directly in Obsidian (not via our CLI).
	if _, err := c.UpdateTask("Tasks/Ask.md", map[string]any{
		"details": applyNoteBodyEdit(task.Details, func(nb *noteBody) {
			nb.Reply = "@claude: still working on it"
		}),
	}); err != nil {
		t.Fatal(err)
	}

	// A follow-up question must not overwrite the in-progress reply.
	if err := cmdAsk(c, []string{"Tasks/Ask.md", "Also check X."}); err != nil {
		t.Fatalf("second ask failed: %v", err)
	}
	task, err = c.GetTask("Tasks/Ask.md")
	if err != nil {
		t.Fatal(err)
	}
	nb, ok = parseNoteBody(task.Details)
	if !ok || nb.Reply != "@claude: still working on it" {
		t.Errorf("expected the user's in-progress reply preserved, ok=%v reply=%q", ok, nb.Reply)
	}
	if nb.Ask != "Also check X." {
		t.Errorf("expected the new ask text set, got %q", nb.Ask)
	}
}

// TestCmdAsk_ClearLeavesReplyBlock verifies `tn ask --clear` removes only
// the ask block; the reply block (the user may reply on any task) is left
// untouched.
func TestCmdAsk_ClearLeavesReplyBlock(t *testing.T) {
	ts := statefulTaskServer(t, "Tasks/Clear.md", "Clear")
	c := newTestClient(ts, "")

	if err := cmdAsk(c, []string{"Tasks/Clear.md", "Merge or reply?"}); err != nil {
		t.Fatalf("ask failed: %v", err)
	}
	if err := cmdAsk(c, []string{"--clear", "Tasks/Clear.md"}); err != nil {
		t.Fatalf("ask --clear failed: %v", err)
	}

	task, err := c.GetTask("Tasks/Clear.md")
	if err != nil {
		t.Fatal(err)
	}
	nb, ok := parseNoteBody(task.Details)
	if !ok {
		t.Fatalf("expected structured parse, got %q", task.Details)
	}
	if nb.Ask != "" {
		t.Errorf("expected ask cleared, got %q", nb.Ask)
	}
	if nb.Reply != defaultReplyBody {
		t.Errorf("expected the reply block left in place, got %q", nb.Reply)
	}
}

// TestCmdLink_AddsAndDedupesByURL verifies `tn link` appends entries and
// that a repeat call for the same URL updates its label in place rather
// than adding a duplicate line.
func TestCmdLink_AddsAndDedupesByURL(t *testing.T) {
	ts := statefulTaskServer(t, "Tasks/Link.md", "Link")
	c := newTestClient(ts, "")

	if err := cmdLink(c, []string{"Tasks/Link.md", "https://gitlab.com/x/y/-/merge_requests/1", "--label", "MR"}); err != nil {
		t.Fatalf("first link failed: %v", err)
	}
	if err := cmdLink(c, []string{"Tasks/Link.md", "https://example.com/shot.png", "--label", "Artifact"}); err != nil {
		t.Fatalf("second link failed: %v", err)
	}

	task, err := c.GetTask("Tasks/Link.md")
	if err != nil {
		t.Fatal(err)
	}
	nb, ok := parseNoteBody(task.Details)
	if !ok {
		t.Fatalf("expected structured parse, got %q", task.Details)
	}
	want := "- MR: https://gitlab.com/x/y/-/merge_requests/1\n- Artifact: https://example.com/shot.png"
	if nb.Links != want {
		t.Fatalf("unexpected links block:\ngot:  %q\nwant: %q", nb.Links, want)
	}

	// Re-adding the first URL with a different label updates in place.
	if err := cmdLink(c, []string{"Tasks/Link.md", "https://gitlab.com/x/y/-/merge_requests/1", "--label", "Merge Request"}); err != nil {
		t.Fatalf("third link failed: %v", err)
	}
	task, err = c.GetTask("Tasks/Link.md")
	if err != nil {
		t.Fatal(err)
	}
	nb, ok = parseNoteBody(task.Details)
	if !ok {
		t.Fatalf("expected structured parse, got %q", task.Details)
	}
	if strings.Count(nb.Links, "merge_requests/1") != 1 {
		t.Errorf("expected exactly one entry for the deduped URL, got:\n%s", nb.Links)
	}
}

// TestCmdLink_MissingURLErrors verifies `tn link <path>` with no URL fails
// loudly rather than making an API call.
func TestCmdLink_MissingURLErrors(t *testing.T) {
	srv, gotBody := fakeNoteAPI(t)
	c := newTestClient(srv, "")

	if err := cmdLink(c, []string{"Tasks/Note.md"}); err == nil {
		t.Error("expected an error for a missing url argument, got nil")
	}
	if len(*gotBody) != 0 {
		t.Errorf("expected no API call to have been made, got body %+v", *gotBody)
	}
}

// statefulTaskServer is a fake TaskNotes API that actually persists details
// across requests (GET returns whatever the last PUT stored), unlike
// fakeNoteAPI's fixed single-response fixture. Needed to exercise a real
// multi-command round trip (e.g. `tn note` then `tn get`) rather than one
// request/response pair in isolation.
func statefulTaskServer(t *testing.T, path, title string) *httptest.Server {
	t.Helper()
	var mu sync.Mutex
	details := ""
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		mu.Lock()
		defer mu.Unlock()
		switch r.Method {
		case http.MethodGet:
			resp, _ := json.Marshal(map[string]any{
				"data": map[string]any{"path": path, "title": title, "details": details},
			})
			w.Write(resp)
		case http.MethodPut:
			var body map[string]any
			json.NewDecoder(r.Body).Decode(&body)
			if d, ok := body["details"].(string); ok {
				details = d
			}
			resp, _ := json.Marshal(map[string]any{
				"data": map[string]any{"path": path, "title": title, "details": details},
			})
			w.Write(resp)
		default:
			t.Fatalf("unexpected method %s", r.Method)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// captureStdout redirects os.Stdout for the duration of fn and returns
// everything written to it. Needed for cmdGet, which prints its
// human-readable output directly rather than returning a string.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	orig := os.Stdout
	os.Stdout = w
	fn()
	os.Stdout = orig
	w.Close()
	var buf bytes.Buffer
	io.Copy(&buf, r)
	return buf.String()
}

// TestCmdGet_ShowsRecentHistory is the regression guard for a real
// incident: `tn note` writes were landing correctly (verified live, twice),
// but `tn get`'s human-readable output omitted the history block entirely.
// An agent that appended a note and read the task back to confirm saw no
// trace of it and concluded its durable record was being silently lost —
// three "lost" notes were never lost, `tn get` just never showed them. This
// exercises the exact confirm-what-you-wrote loop agents rely on: `tn note`
// then `tn get`, against a server that actually persists details across
// requests (see statefulTaskServer).
func TestCmdGet_ShowsRecentHistory(t *testing.T) {
	const path = "Tasks/Round.md"
	srv := statefulTaskServer(t, path, "Round")
	c := newTestClient(srv, "")

	if err := cmdNote(c, []string{path, "checked the logs", "--by", "reviewer"}); err != nil {
		t.Fatalf("tn note failed: %v", err)
	}

	out := captureStdout(t, func() {
		if err := cmdGet(c, []string{path}); err != nil {
			t.Fatalf("tn get failed: %v", err)
		}
	})

	if !strings.Contains(out, "checked the logs") {
		t.Errorf("tn get output is missing the note just written — this is the exact 'silent loss' incident, got: %s", out)
	}
	if !strings.Contains(out, "reviewer:**") {
		t.Errorf("tn get output is missing the history entry's author, got: %s", out)
	}
	if !strings.Contains(out, "history (1)") {
		t.Errorf("expected a 'history (1):' header for a single entry, got: %s", out)
	}
}

// TestCmdGet_TruncatesHistoryWithHint verifies the default cap (5 most
// recent entries) plus a "N more" hint when truncated, and that
// --history 0 shows everything including the oldest entry.
func TestCmdGet_TruncatesHistoryWithHint(t *testing.T) {
	const path = "Tasks/Many.md"
	srv := statefulTaskServer(t, path, "Many")
	c := newTestClient(srv, "")

	for i := range 7 {
		if err := cmdNote(c, []string{path, fmt.Sprintf("entry %d", i)}); err != nil {
			t.Fatalf("tn note #%d failed: %v", i, err)
		}
	}

	t.Run("default caps at 5 with a hint", func(t *testing.T) {
		out := captureStdout(t, func() {
			if err := cmdGet(c, []string{path}); err != nil {
				t.Fatal(err)
			}
		})
		if !strings.Contains(out, "history (5 of 7)") {
			t.Errorf("expected a 5-of-7 header, got: %s", out)
		}
		if !strings.Contains(out, "2 more") {
			t.Errorf("expected a '2 more' hint, got: %s", out)
		}
		if strings.Contains(out, "entry 0") {
			t.Errorf("expected the oldest entry to be hidden by default, got: %s", out)
		}
	})

	t.Run("--history 0 shows all", func(t *testing.T) {
		out := captureStdout(t, func() {
			if err := cmdGet(c, []string{path, "--history", "0"}); err != nil {
				t.Fatal(err)
			}
		})
		if !strings.Contains(out, "entry 0") {
			t.Errorf("expected --history 0 to show every entry including the oldest, got: %s", out)
		}
		if strings.Contains(out, "more, see the note in Obsidian") {
			t.Errorf("expected no truncation hint when showing all, got: %s", out)
		}
	})
}

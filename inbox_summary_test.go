package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestSummarizeInboxText_ShortUnchanged verifies text under the cap is
// printed verbatim — no marker, no truncation — so short messages keep
// looking exactly as they always have.
func TestSummarizeInboxText_ShortUnchanged(t *testing.T) {
	short := "here's my answer"
	got := summarizeInboxText("msg-1", short)
	if got != short {
		t.Errorf("expected short text unchanged, got %q", got)
	}
}

// TestSummarizeInboxText_LongTruncatesWithMarker verifies text over the
// cap is cut to inboxSummaryMaxRunes runes and carries an explicit marker
// naming the exact follow-up command — the whole point being an agent
// never mistakes a cut line for the complete message.
func TestSummarizeInboxText_LongTruncatesWithMarker(t *testing.T) {
	long := strings.Repeat("a", 500)
	got := summarizeInboxText("msg-42", long)

	wantPrefix := strings.Repeat("a", inboxSummaryMaxRunes)
	if !strings.HasPrefix(got, wantPrefix) || strings.HasPrefix(got, wantPrefix+"a") {
		t.Errorf("expected exactly %d leading content runes, got %q", inboxSummaryMaxRunes, got)
	}
	if !strings.Contains(got, "run `tn msg msg-42`") {
		t.Errorf("expected the marker to name the exact id, got %q", got)
	}
	wantMore := len([]rune(long)) - inboxSummaryMaxRunes
	if !strings.Contains(got, "more chars") {
		t.Fatalf("expected a 'more chars' marker, got %q", got)
	}
	if !strings.Contains(got, fmt.Sprintf("%d", wantMore)) {
		t.Errorf("expected the remaining-char count %d in the marker, got %q", wantMore, got)
	}
}

// TestCmdInbox_LongMessageSummarizedInPlainOutput exercises the full CLI
// path: a message longer than the cap, fetched via `tn inbox` against a
// fake bridge, must produce a bounded line with the marker — never the
// raw 500-char text.
func TestCmdInbox_LongMessageSummarizedInPlainOutput(t *testing.T) {
	longText := strings.Repeat("word ", 100) // 500 chars
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"messages": []map[string]any{
				{"id": "msg-long", "from": "team-lead", "taskPath": "Tasks/X.md", "text": longText, "status": "delivered"},
			},
		})
	}))
	defer srv.Close()
	t.Setenv("TN_BRIDGE_URL", srv.URL)

	out := captureStdout(t, func() {
		if err := cmdInbox([]string{"--name", "agent-1"}); err != nil {
			t.Fatal(err)
		}
	})

	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 1 {
		t.Fatalf("expected exactly 1 line, got %d: %q", len(lines), out)
	}
	if len([]rune(lines[0])) >= len([]rune(longText)) {
		t.Fatalf("expected the line to be shorter than the raw message, got len %d vs raw %d", len([]rune(lines[0])), len([]rune(longText)))
	}
	if !strings.Contains(lines[0], "run `tn msg msg-long`") {
		t.Errorf("expected the marker in the printed line, got %q", lines[0])
	}
}

// TestCmdInbox_ShortMessageUnchanged verifies a message under the cap
// produces the exact same line shape as before this change — no marker,
// full text present.
func TestCmdInbox_ShortMessageUnchanged(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"messages": []map[string]any{
				{"id": "msg-short", "from": "team-lead", "taskPath": "Tasks/X.md", "text": "short reply", "status": "delivered"},
			},
		})
	}))
	defer srv.Close()
	t.Setenv("TN_BRIDGE_URL", srv.URL)

	out := captureStdout(t, func() {
		if err := cmdInbox([]string{"--name", "agent-1"}); err != nil {
			t.Fatal(err)
		}
	})

	want := "msg-short\tteam-lead\tTasks/X.md\tshort reply\n"
	if out != want {
		t.Errorf("expected %q, got %q", want, out)
	}
}

// TestCmdInbox_JSONUnaffectedByTruncation verifies --json stays complete
// regardless of message length — that path was never line-limited and
// must not become so.
func TestCmdInbox_JSONUnaffectedByTruncation(t *testing.T) {
	longText := strings.Repeat("z", 500)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"messages": []map[string]any{
				{"id": "msg-json", "from": "team-lead", "text": longText, "status": "delivered"},
			},
		})
	}))
	defer srv.Close()
	t.Setenv("TN_BRIDGE_URL", srv.URL)

	out := captureStdout(t, func() {
		if err := cmdInbox([]string{"--name", "agent-1", "--json"}); err != nil {
			t.Fatal(err)
		}
	})

	if !strings.Contains(out, longText) {
		t.Errorf("expected --json output to contain the full untruncated text")
	}
}

// TestCmdMsg_PrintsFullText verifies `tn msg <id>` returns the complete
// text — the companion command a truncated inbox line points to.
func TestCmdMsg_PrintsFullText(t *testing.T) {
	longText := strings.Repeat("full text here. ", 40)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/messages" || r.URL.Query().Get("id") != "msg-full" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"message": map[string]any{
				"id": "msg-full", "from": "team-lead", "taskPath": "Tasks/X.md",
				"text": longText, "status": "delivered", "response": "",
			},
		})
	}))
	defer srv.Close()
	t.Setenv("TN_BRIDGE_URL", srv.URL)

	out := captureStdout(t, func() {
		if err := cmdMsg([]string{"msg-full"}); err != nil {
			t.Fatal(err)
		}
	})

	if !strings.Contains(out, longText) {
		t.Errorf("expected tn msg output to contain the full text, got %q", out)
	}
	if !strings.Contains(out, "id: msg-full") {
		t.Errorf("expected the id in the output, got %q", out)
	}
}

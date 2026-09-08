package tn

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"
)

// TestDetectLiveStuckPrompt covers positive and negative live-prompt
// detection. A marker alone is no longer sufficient — a numbered option
// must also be present in the same window (the fix for scrollback
// false-positives: Claude prints resolved prompts verbatim as history, and
// a marker-only match there used to re-fire a dialog for something already
// answered).
func TestDetectLiveStuckPrompt(t *testing.T) {
	cases := []struct {
		name string
		pane string
		want bool
	}{
		{"proceed prompt with options", "some output\nDo you want to proceed?\n❯ 1. Yes\n2. No", true},
		{"requires confirmation with options", "Bash(rm -rf /tmp/x)\n  requires confirmation\n❯ 1. Yes\n2. No\nEsc to cancel", true},
		{"marker with no options at all — resolved scrollback, not live", "some tool call\nEsc to cancel", false},
		{"proceed marker with no options — same, not live", "Do you want to proceed?\nsome other output after it", false},
		{"normal output", "Running tests...\nAll tests passed\n$", false},
		{"empty", "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, got := detectLiveStuckPrompt(c.pane)
			if got != c.want {
				t.Errorf("detectLiveStuckPrompt(%q) = %v, want %v", c.pane, got, c.want)
			}
		})
	}
}

// TestDetectLiveStuckPrompt_OnlyScansRecentLines verifies a marker+options
// pair outside the last ~15 lines of the pane isn't detected — old
// scrollback, not a live prompt, even though both required pieces are
// technically present somewhere in the capture.
func TestDetectLiveStuckPrompt_OnlyScansRecentLines(t *testing.T) {
	pane := "Do you want to proceed?\n❯ 1. Yes\n2. No\n" + strings.Repeat("line\n", 50)
	if _, found := detectLiveStuckPrompt(pane); found {
		t.Error("expected a marker+options pair outside the last ~15 lines to not be detected")
	}
}

// TestSessionStatusDisplay_Precedence verifies rate-limited > gone >
// stuck > unackedBacklog > draining > alive. Rate-limited outranks even
// not-alive: a usage-limit wait stops the inbox poll by definition, so
// "gone" would hide the only fact that explains it.
func TestSessionStatusDisplay_Precedence(t *testing.T) {
	cases := []struct {
		alive, stuck, unackedBacklog, draining, rateLimited bool
		resets                                              string
		want                                                string
	}{
		{false, true, true, true, false, "", "⚫ gone"},
		{true, true, true, true, false, "", "⛔ stuck (permission prompt)"},
		{true, false, true, true, false, "", "🟡 unacked backlog (>30min)"},
		{true, false, false, true, false, "", "🌀 draining"},
		{true, false, false, false, false, "", "🟢 alive"},
		// Rate-limited wins over every other state, alive or not, and
		// suppresses the unacked-backlog badge (backlog is expected
		// while the session is parked).
		{false, false, true, false, true, "2:30pm (Europe/Berlin)", "⏳ usage limit (resets 2:30pm (Europe/Berlin))"},
		{true, true, true, true, true, "2:30pm", "⏳ usage limit (resets 2:30pm)"},
		{true, false, false, false, true, "", "⏳ usage limit"},
	}
	for _, c := range cases {
		got := sessionStatusDisplay(c.alive, c.stuck, c.unackedBacklog, c.draining, c.rateLimited, c.resets)
		if got != c.want {
			t.Errorf("sessionStatusDisplay(%v,%v,%v,%v,%v,%q) = %q, want %q",
				c.alive, c.stuck, c.unackedBacklog, c.draining, c.rateLimited, c.resets, got, c.want)
		}
	}
}

// TestCheckStuckSessionsOnce_StuckVsUnackedBacklogClassification exercises
// a full detector pass with two alive orchestrators: one with an old
// delivered-unacked message but a clean pane (unacked backlog, not stuck),
// one with a visible permission prompt (stuck). Only the stuck one should
// trigger the approval dialog.
func TestCheckStuckSessionsOnce_StuckVsUnackedBacklogClassification(t *testing.T) {
	srv := newDueScannerTestServer(t, map[string]ProjectConfig{})
	now := time.Now()

	srv.mu.Lock()
	srv.state.Agents["orchestrator-myapp"] = &Agent{Name: "orchestrator-myapp", Project: "myapp", LastSeenAt: now}
	srv.state.Agents["orchestrator-foo"] = &Agent{Name: "orchestrator-foo", Project: "foo", LastSeenAt: now}
	deliveredAt := now.Add(-40 * time.Minute)
	srv.state.Messages = append(srv.state.Messages, &Message{
		ID: "m1", To: "orchestrator-myapp", Status: "delivered", CreatedAt: deliveredAt, DeliveredAt: &deliveredAt,
	})
	srv.mu.Unlock()

	capture := func(session string) (string, error) {
		switch session {
		case "tn-myapp":
			return "no marker here, just normal output\n", nil
		case "tn-foo":
			return "Do you want to proceed?\n❯ 1. Yes\n2. No", nil
		}
		return "", fmt.Errorf("unexpected session %q", session)
	}
	var dialogCalls int
	dialog := func(title, text string, buttons []string) (string, error) {
		dialogCalls++
		return "Dismiss", nil
	}
	sendKeys := func(session, keys string) error {
		t.Fatal("sendKeys should not be called for a Dismiss response")
		return nil
	}

	// Two-tick confirmation (see updateStuckState): the same unchanging
	// pane needs two passes before a dialog signature counts as
	// genuinely stuck.
	srv.checkStuckSessionsOnce(capture, dialog, sendKeys)
	srv.checkStuckSessionsOnce(capture, dialog, sendKeys)

	info := srv.stuck.snapshot()
	if !info["orchestrator-myapp"].UnackedBacklog || info["orchestrator-myapp"].Stuck {
		t.Errorf("expected orchestrator-myapp to carry an unacked backlog (not stuck), got %+v", info["orchestrator-myapp"])
	}
	if !info["orchestrator-foo"].Stuck {
		t.Errorf("expected orchestrator-foo stuck, got %+v", info["orchestrator-foo"])
	}
	if dialogCalls != 1 {
		t.Errorf("expected the dialog shown exactly once (only for the stuck agent, only once confirmed), got %d", dialogCalls)
	}
}

// TestCheckStuckSessionsOnce_InformationalMessageDoesNotCountTowardUnackedBacklog
// is the 2026-08-25 fix: a message that asks nothing of its recipient
// (Message.Informational) must never count toward the unacked-backlog
// signal — a demonstrably working agent was flagged "stalled" (the field's
// name at the time) for ~3h off exactly this shape of message. Same
// fixture as the classification test above, except the message is marked
// Informational.
func TestCheckStuckSessionsOnce_InformationalMessageDoesNotCountTowardUnackedBacklog(t *testing.T) {
	srv := newDueScannerTestServer(t, map[string]ProjectConfig{})
	now := time.Now()

	srv.mu.Lock()
	srv.state.Agents["orchestrator-myapp"] = &Agent{Name: "orchestrator-myapp", Project: "myapp", LastSeenAt: now}
	deliveredAt := now.Add(-40 * time.Minute)
	srv.state.Messages = append(srv.state.Messages, &Message{
		ID: "m1", To: "orchestrator-myapp", Status: "delivered", CreatedAt: deliveredAt, DeliveredAt: &deliveredAt,
		Informational: true,
	})
	srv.mu.Unlock()

	capture := func(session string) (string, error) { return "no marker here, just normal output\n", nil }
	dialog := func(title, text string, buttons []string) (string, error) { return "Dismiss", nil }
	sendKeys := func(session, keys string) error { return nil }

	srv.checkStuckSessionsOnce(capture, dialog, sendKeys)
	srv.checkStuckSessionsOnce(capture, dialog, sendKeys)

	info := srv.stuck.snapshot()
	if info["orchestrator-myapp"].UnackedBacklog {
		t.Errorf("expected an all-Informational unacked backlog to NOT set UnackedBacklog, got %+v", info["orchestrator-myapp"])
	}
}

// TestOldestDeliveredUnackedAge_ExcludesInformational is the direct,
// function-level version of the fix above: a mix of a real (non-
// Informational) delivered-unacked message and an older Informational one
// must report the age of the REAL one only — the older Informational
// message must not win just because it's older.
func TestOldestDeliveredUnackedAge_ExcludesInformational(t *testing.T) {
	now := time.Now()
	oldInformational := now.Add(-3 * time.Hour)
	newerReal := now.Add(-10 * time.Minute)
	messages := []Message{
		{To: "agent-a", Status: "delivered", DeliveredAt: &oldInformational, Informational: true},
		{To: "agent-a", Status: "delivered", DeliveredAt: &newerReal},
	}
	got := oldestDeliveredUnackedAge("agent-a", messages, now)
	if got < 9*time.Minute || got > 11*time.Minute {
		t.Errorf("expected the age of the newer, non-Informational message (~10m), got %v", got)
	}
}

// TestOldestDeliveredUnackedAge_AllInformational_ReturnsZero verifies the
// zero case: if EVERY delivered-unacked message is Informational, there is
// no real backlog at all, not just a small one.
func TestOldestDeliveredUnackedAge_AllInformational_ReturnsZero(t *testing.T) {
	now := time.Now()
	old := now.Add(-3 * time.Hour)
	messages := []Message{
		{To: "agent-a", Status: "delivered", DeliveredAt: &old, Informational: true},
	}
	if got := oldestDeliveredUnackedAge("agent-a", messages, now); got != 0 {
		t.Errorf("expected 0 when every unacked message is Informational, got %v", got)
	}
}

// TestQueueMessageLocked_PropagatesInformational verifies the wiring from
// sendRequest.Informational to the persisted Message.Informational —
// dispatchMessage's only path for setting it.
func TestQueueMessageLocked_PropagatesInformational(t *testing.T) {
	srv := newDueScannerTestServer(t, map[string]ProjectConfig{})
	srv.mu.Lock()
	msg := srv.queueMessageLocked("agent-a", sendRequest{Text: "fyi only", Informational: true})
	srv.mu.Unlock()
	if !msg.Informational {
		t.Error("expected Informational to propagate from sendRequest to the persisted Message")
	}
}

// TestCheckStuckSessionsOnce_UsesReportedTmuxSession verifies the detector
// inspects any alive agent with a KNOWN tmux session — not just
// orchestrator-<slug>-named ones (SPEC-generations.md): a generation-named
// agent (orchestrator-myapp-g54321) that reported its own TmuxSession must
// be captured under that exact session name, not the legacy tn-<name>
// derivation.
func TestCheckStuckSessionsOnce_UsesReportedTmuxSession(t *testing.T) {
	srv := newDueScannerTestServer(t, map[string]ProjectConfig{})
	now := time.Now()

	srv.mu.Lock()
	srv.state.Agents["orchestrator-myapp-g54321"] = &Agent{
		Name: "orchestrator-myapp-g54321", Project: "myapp", LastSeenAt: now,
		TmuxSession: "tn-myapp-g54321",
	}
	srv.mu.Unlock()

	var capturedSession string
	capture := func(session string) (string, error) {
		capturedSession = session
		return "Do you want to proceed?\n❯ 1. Yes\n2. No", nil
	}
	dialog := func(title, text string, buttons []string) (string, error) { return "Dismiss", nil }
	sendKeys := func(session, keys string) error { return nil }

	srv.checkStuckSessionsOnce(capture, dialog, sendKeys)
	srv.checkStuckSessionsOnce(capture, dialog, sendKeys) // two-tick confirmation

	if capturedSession != "tn-myapp-g54321" {
		t.Errorf("expected capture on the agent's reported TmuxSession %q, got %q", "tn-myapp-g54321", capturedSession)
	}
	info := srv.stuck.snapshot()
	if !info["orchestrator-myapp-g54321"].Stuck {
		t.Errorf("expected the generation agent detected stuck, got %+v", info["orchestrator-myapp-g54321"])
	}
}

// TestTmuxSessionForAgent covers the resolution rules directly: an
// explicit TmuxSession always wins, a legacy orchestrator-<slug>-named
// agent without one falls back to tn-<slug>, and anything else has none.
func TestTmuxSessionForAgent(t *testing.T) {
	cases := []struct {
		name string
		a    Agent
		want string
		ok   bool
	}{
		{"explicit wins", Agent{Name: "orchestrator-myapp", TmuxSession: "tn-myapp-g1"}, "tn-myapp-g1", true},
		{"legacy fallback", Agent{Name: "orchestrator-myapp"}, "tn-myapp", true},
		{"non-orchestrator no fallback", Agent{Name: "claude/foo-e2e"}, "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := tmuxSessionForAgent(c.a)
			if got != c.want || ok != c.ok {
				t.Errorf("tmuxSessionForAgent(%+v) = (%q, %v), want (%q, %v)", c.a, got, ok, c.want, c.ok)
			}
		})
	}
}

// TestUpdateStuckState_NoRepromptWithin15Min verifies the dialog isn't
// re-shown for an agent that stays stuck within the 15-minute reprompt
// window, but is shown again once that window elapses.
func TestUpdateStuckState_NoRepromptWithin15Min(t *testing.T) {
	srv := newDueScannerTestServer(t, map[string]ProjectConfig{})
	var dialogCalls int
	dialog := func(title, text string, buttons []string) (string, error) {
		dialogCalls++
		return "Dismiss", nil
	}
	capture := func(session string) (string, error) { return "Do you want to proceed?", nil }
	sendKeys := func(session, keys string) error { return nil }

	now := time.Now()
	// Tick 1: candidate only (two-tick confirmation — see
	// updateStuckState) — same unchanging excerpt, not yet confirmed.
	srv.updateStuckState("orchestrator-myapp", "tn-myapp", true, false, "Do you want to proceed?", now, capture, dialog, sendKeys, false)
	// Tick 2: confirmed, dialog fires once (new episode).
	srv.updateStuckState("orchestrator-myapp", "tn-myapp", true, false, "Do you want to proceed?", now, capture, dialog, sendKeys, false)
	srv.updateStuckState("orchestrator-myapp", "tn-myapp", true, false, "Do you want to proceed?", now.Add(5*time.Minute), capture, dialog, sendKeys, false)

	if dialogCalls != 1 {
		t.Fatalf("expected exactly 1 dialog within 15min of the first, got %d", dialogCalls)
	}

	srv.updateStuckState("orchestrator-myapp", "tn-myapp", true, false, "Do you want to proceed?", now.Add(16*time.Minute), capture, dialog, sendKeys, false)
	if dialogCalls != 2 {
		t.Errorf("expected a re-prompt after the 15min window elapsed, got %d calls", dialogCalls)
	}
}

// TestUpdateStuckState_NoDialogWhenNotStuck verifies clearing (not stuck)
// never triggers the dialog, and going from stuck to clear then stuck again
// counts as a fresh transition (immediate re-prompt, not rate-limited).
func TestUpdateStuckState_NoDialogWhenNotStuck(t *testing.T) {
	srv := newDueScannerTestServer(t, map[string]ProjectConfig{})
	var dialogCalls int
	dialog := func(title, text string, buttons []string) (string, error) {
		dialogCalls++
		return "Dismiss", nil
	}
	capture := func(session string) (string, error) { return "", nil }
	sendKeys := func(session, keys string) error { return nil }

	now := time.Now()
	srv.updateStuckState("orchestrator-myapp", "tn-myapp", false, false, "", now, capture, dialog, sendKeys, false)
	if dialogCalls != 0 {
		t.Fatalf("expected no dialog when never stuck, got %d", dialogCalls)
	}
}

// TestSendApprovalIfStillStuck_SkipsSendWhenMarkerCleared verifies the
// re-check-before-send rule: if the marker is gone on re-capture (the user
// resolved it themselves in the meantime), no keys are sent and no
// activity is logged.
func TestSendApprovalIfStillStuck_SkipsSendWhenMarkerCleared(t *testing.T) {
	srv := newDueScannerTestServer(t, map[string]ProjectConfig{})
	var sendCalls int
	sendKeys := func(session, keys string) error {
		sendCalls++
		return nil
	}
	capture := func(session string) (string, error) { return "all clear now\n$", nil }

	srv.sendApprovalIfStillStuck("orchestrator-myapp", "tn-myapp", approvalAlwaysAllow, capture, sendKeys)

	if sendCalls != 0 {
		t.Errorf("expected no send when the marker cleared on re-check, got %d calls", sendCalls)
	}
	srv.mu.Lock()
	n := len(srv.state.Activity)
	srv.mu.Unlock()
	if n != 0 {
		t.Errorf("expected no activity entry logged when the send was skipped, got %d entries", n)
	}
}

// TestSendApprovalIfStillStuck_SendsWhenMarkerStillPresent verifies the
// happy path: marker still present on re-capture → keys sent, and an
// activity entry is appended for the agent's session page.
func TestSendApprovalIfStillStuck_SendsWhenMarkerStillPresent(t *testing.T) {
	srv := newDueScannerTestServer(t, map[string]ProjectConfig{})
	var sendSession, sendKeysArg string
	sendKeys := func(session, keys string) error {
		sendSession, sendKeysArg = session, keys
		return nil
	}
	capture := func(session string) (string, error) {
		return "Do you want to proceed?\n❯ 1. Yes\n2. Yes, and don't ask again this session\n3. No", nil
	}

	srv.sendApprovalIfStillStuck("orchestrator-myapp", "tn-myapp", approvalAlwaysAllow, capture, sendKeys)

	if sendSession != "tn-myapp" || sendKeysArg != "2" {
		t.Fatalf("expected send-keys to tn-myapp with %q, got session=%q keys=%q", "2", sendSession, sendKeysArg)
	}
	srv.mu.Lock()
	activity := srv.state.Activity
	srv.mu.Unlock()
	if len(activity) != 1 || activity[0].Agent != "orchestrator-myapp" {
		t.Fatalf("expected an activity entry for orchestrator-myapp, got %+v", activity)
	}
	if !strings.Contains(activity[0].Text, "Always allow") {
		t.Errorf("expected the activity text to mention the chosen option, got %q", activity[0].Text)
	}
}

// TestRenderDashboardMarkdown_StuckStatus verifies the Sessions table shows
// the stuck status for a flagged agent.
func TestRenderDashboardMarkdown_StuckStatus(t *testing.T) {
	now := mustTime(t, "2026-08-17T12:00:00Z")
	snap := dashboardSnapshot{
		Agents: []Agent{{Name: "orchestrator-myapp", Project: "myapp", LastSeenAt: now.Add(-5 * time.Second)}},
	}
	stuckByAgent := map[string]stuckInfo{
		"orchestrator-myapp": {Stuck: true, Excerpt: "Do you want to proceed?"},
	}
	md := renderDashboardMarkdown(snap, now, "", nil, false, stuckByAgent)
	if !strings.Contains(md, "| orchestrator-myapp | myapp | ⛔ stuck (permission prompt) |") {
		t.Errorf("expected stuck status in the Sessions row, got:\n%s", md)
	}
}

// TestRenderSessionPageMarkdown_StuckExcerpt verifies the fenced-code-block
// excerpt appears on a stuck agent's session page, and is absent otherwise.
func TestRenderSessionPageMarkdown_StuckExcerpt(t *testing.T) {
	now := mustTime(t, "2026-08-17T12:00:00Z")
	agent := Agent{Name: "orchestrator-myapp", Project: "myapp", LastSeenAt: now}

	md := renderSessionPageMarkdown(agent, true, nil, now, stuckInfo{Stuck: true, Excerpt: "Do you want to proceed?\n1. Yes"})
	if !strings.Contains(md, "## Stuck: permission prompt") {
		t.Errorf("expected a stuck section, got:\n%s", md)
	}
	if !strings.Contains(md, "```\nDo you want to proceed?\n1. Yes\n```") {
		t.Errorf("expected the fenced excerpt, got:\n%s", md)
	}

	mdClean := renderSessionPageMarkdown(agent, true, nil, now, stuckInfo{})
	if strings.Contains(mdClean, "## Stuck") {
		t.Errorf("expected no stuck section when not stuck, got:\n%s", mdClean)
	}
}

// --- Option-mapping fix: prompts come in more than one shape ---

const (
	twoOptionPane = "Do you want to proceed?\n❯ 1. Yes\n2. No"
	// don't-ask option at position 2.
	threeOptionPaneDontAskAt2 = "Do you want to proceed?\n❯ 1. Yes\n2. Yes, and don't ask again this session\n3. No"
	// same shape, but don't-ask reordered to position 3 — mapping must go by
	// text, never position.
	threeOptionPaneDontAskAt3 = "Do you want to proceed?\n❯ 1. Yes\n2. No, and tell Claude what to do differently\n3. Yes, and don't ask again this session"
)

// TestResolveStuckPromptMapping_TwoOptionPane verifies a bare Yes/No prompt
// maps "Allow once" and "Deny" — there is no "Always allow" equivalent to
// offer, but denial (the "No" option) is always a legitimate response.
func TestResolveStuckPromptMapping_TwoOptionPane(t *testing.T) {
	m := resolveStuckPromptMapping(twoOptionPane)
	if m.AllowOnceKey != "1" {
		t.Errorf("expected AllowOnceKey %q, got %q", "1", m.AllowOnceKey)
	}
	if m.DenyKey != "2" {
		t.Errorf("expected DenyKey %q, got %q", "2", m.DenyKey)
	}
	if m.AlwaysAllowKey != "" {
		t.Errorf("expected no AlwaysAllowKey on a 2-option prompt, got %q", m.AlwaysAllowKey)
	}
	buttons := stuckDialogButtons(m)
	want := []string{"Dismiss", "Deny", "Allow once"}
	if !equalStrSlices(buttons, want) {
		t.Errorf("buttons = %v, want %v", buttons, want)
	}
}

// TestResolveStuckPromptMapping_ThreeOptionPane_DontAskPosition verifies the
// don't-ask-again option is resolved by TEXT, not position — numbered 2 in
// one fixture and 3 in another must both map correctly, and the "No" option
// maps to Deny in both regardless of its own position.
func TestResolveStuckPromptMapping_ThreeOptionPane_DontAskPosition(t *testing.T) {
	cases := []struct {
		name           string
		pane           string
		allowOnceKey   string
		alwaysAllowKey string
		denyKey        string
	}{
		{"dont-ask at 2", threeOptionPaneDontAskAt2, "1", "2", "3"},
		{"dont-ask at 3", threeOptionPaneDontAskAt3, "1", "3", "2"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m := resolveStuckPromptMapping(c.pane)
			if m.AllowOnceKey != c.allowOnceKey {
				t.Errorf("AllowOnceKey = %q, want %q", m.AllowOnceKey, c.allowOnceKey)
			}
			if m.AlwaysAllowKey != c.alwaysAllowKey {
				t.Errorf("AlwaysAllowKey = %q, want %q", m.AlwaysAllowKey, c.alwaysAllowKey)
			}
			if m.DenyKey != c.denyKey {
				t.Errorf("DenyKey = %q, want %q", m.DenyKey, c.denyKey)
			}
			buttons := stuckDialogButtons(m)
			want := []string{"Dismiss", "Deny", "Allow once", "Always allow"}
			if !equalStrSlices(buttons, want) {
				t.Errorf("buttons = %v, want %v", buttons, want)
			}
		})
	}
}

// TestSendApprovalIfStillStuck_ReparsesFreshCapture verifies the option
// number sent comes from the RE-CAPTURED pane at send time, not any mapping
// computed earlier (e.g. when the dialog was first shown) — option numbers
// can legitimately differ between the two captures.
func TestSendApprovalIfStillStuck_ReparsesFreshCapture(t *testing.T) {
	srv := newDueScannerTestServer(t, map[string]ProjectConfig{})
	var sendKeysArg string
	sendKeys := func(session, keys string) error {
		sendKeysArg = keys
		return nil
	}
	// The fresh re-capture has don't-ask at position 3, not 2 — if the code
	// sent a stale/hardcoded key it would send "2" (No) instead of "3".
	capture := func(session string) (string, error) { return threeOptionPaneDontAskAt3, nil }

	srv.sendApprovalIfStillStuck("orchestrator-myapp", "tn-myapp", approvalAlwaysAllow, capture, sendKeys)

	if sendKeysArg != "3" {
		t.Errorf("expected the freshly re-parsed key %q, got %q", "3", sendKeysArg)
	}
}

// TestSendApprovalIfStillStuck_SkipsWhenOptionNotOfferedOnRecheck verifies
// that if the fresh re-capture no longer offers the requested option kind
// (e.g. it shrank to a 2-option Yes/No between dialog-show and send), no
// keys are sent rather than sending a wrong/stale number.
func TestSendApprovalIfStillStuck_SkipsWhenOptionNotOfferedOnRecheck(t *testing.T) {
	srv := newDueScannerTestServer(t, map[string]ProjectConfig{})
	var sendCalls int
	sendKeys := func(session, keys string) error {
		sendCalls++
		return nil
	}
	capture := func(session string) (string, error) { return twoOptionPane, nil }

	srv.sendApprovalIfStillStuck("orchestrator-myapp", "tn-myapp", approvalAlwaysAllow, capture, sendKeys)

	if sendCalls != 0 {
		t.Errorf("expected no send when the re-checked prompt has no Always-allow option, got %d calls", sendCalls)
	}
}

// TestCheckStuckSessionsOnce_ButtonsMatchPromptShape verifies a full
// detector pass builds the dialog's buttons from what each agent's own
// pane actually offers — a 2-option prompt never gets an "Always allow"
// button (the bug that could send "No" on a 2-option prompt).
func TestCheckStuckSessionsOnce_ButtonsMatchPromptShape(t *testing.T) {
	srv := newDueScannerTestServer(t, map[string]ProjectConfig{})
	now := time.Now()
	srv.mu.Lock()
	srv.state.Agents["orchestrator-two"] = &Agent{Name: "orchestrator-two", Project: "two", LastSeenAt: now}
	srv.state.Agents["orchestrator-three"] = &Agent{Name: "orchestrator-three", Project: "three", LastSeenAt: now}
	srv.mu.Unlock()

	capture := func(session string) (string, error) {
		switch session {
		case "tn-two":
			return twoOptionPane, nil
		case "tn-three":
			return threeOptionPaneDontAskAt2, nil
		}
		return "", fmt.Errorf("unexpected session %q", session)
	}
	buttonsByAgent := map[string][]string{}
	dialog := func(title, text string, buttons []string) (string, error) {
		if strings.Contains(title, "orchestrator-two") {
			buttonsByAgent["orchestrator-two"] = buttons
		} else {
			buttonsByAgent["orchestrator-three"] = buttons
		}
		return "Dismiss", nil
	}
	sendKeys := func(session, keys string) error { return nil }

	srv.checkStuckSessionsOnce(capture, dialog, sendKeys) // tick 1: candidate
	srv.checkStuckSessionsOnce(capture, dialog, sendKeys) // tick 2: confirmed

	if !equalStrSlices(buttonsByAgent["orchestrator-two"], []string{"Dismiss", "Deny", "Allow once"}) {
		t.Errorf("2-option agent buttons = %v, want [Dismiss Deny Allow once]", buttonsByAgent["orchestrator-two"])
	}
	if !equalStrSlices(buttonsByAgent["orchestrator-three"], []string{"Dismiss", "Deny", "Allow once", "Always allow"}) {
		t.Errorf("3-option agent buttons = %v, want [Dismiss Deny Allow once Always allow]", buttonsByAgent["orchestrator-three"])
	}
}

// TestCheckStuckSessionsOnce_DenyChoiceSendsNoOptionNumber verifies the new
// "Deny" button on a 2-option Yes/No prompt sends the "No" option's own
// parsed number — the exact live case this addendum fixes: the dialog
// previously had no way to express denial at all.
func TestCheckStuckSessionsOnce_DenyChoiceSendsNoOptionNumber(t *testing.T) {
	srv := newDueScannerTestServer(t, map[string]ProjectConfig{})
	now := time.Now()
	srv.mu.Lock()
	srv.state.Agents["orchestrator-myapp"] = &Agent{Name: "orchestrator-myapp", Project: "myapp", LastSeenAt: now}
	srv.mu.Unlock()

	capture := func(session string) (string, error) { return twoOptionPane, nil }
	var offeredButtons []string
	dialog := func(title, text string, buttons []string) (string, error) {
		offeredButtons = buttons
		return "Deny", nil
	}
	var sendSession, sendKeysArg string
	sendKeys := func(session, keys string) error {
		sendSession, sendKeysArg = session, keys
		return nil
	}

	srv.checkStuckSessionsOnce(capture, dialog, sendKeys) // tick 1: candidate
	srv.checkStuckSessionsOnce(capture, dialog, sendKeys) // tick 2: confirmed

	if !equalStrSlices(offeredButtons, []string{"Dismiss", "Deny", "Allow once"}) {
		t.Fatalf("expected buttons [Dismiss Deny Allow once] on a 2-option prompt, got %v", offeredButtons)
	}
	if sendSession != "tn-myapp" || sendKeysArg != "2" {
		t.Errorf("expected Deny to send the No option's number %q to tn-myapp, got session=%q keys=%q", "2", sendSession, sendKeysArg)
	}
}

func equalStrSlices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// --- Episode-hash dedup: a new prompt (different excerpt) always reprompts
// immediately, even within the 15-min reprompt window; the SAME prompt
// repeated stays rate-limited. ---

// TestUpdateStuckState_DifferentExcerptRepromptsImmediately verifies serial
// permission prompts (e.g. several subagents each triggering their own
// worktree-relocation prompt back-to-back) each get their own dialog, since
// each is a genuinely new episode by content, not just "still stuck".
func TestUpdateStuckState_DifferentExcerptRepromptsImmediately(t *testing.T) {
	srv := newDueScannerTestServer(t, map[string]ProjectConfig{})
	var dialogCalls int
	dialog := func(title, text string, buttons []string) (string, error) {
		dialogCalls++
		return "Dismiss", nil
	}
	capture := func(session string) (string, error) { return "", nil }
	sendKeys := func(session, keys string) error { return nil }

	now := time.Now()
	// Two-tick confirmation (updateStuckState) applies per distinct
	// excerpt: the first prompt needs two identical passes before it
	// confirms and fires...
	srv.updateStuckState("orchestrator-myapp", "tn-myapp", true, false, twoOptionPane, now, capture, dialog, sendKeys, false)
	srv.updateStuckState("orchestrator-myapp", "tn-myapp", true, false, twoOptionPane, now, capture, dialog, sendKeys, false)
	if dialogCalls != 1 {
		t.Fatalf("expected 1 dialog for the first episode, got %d", dialogCalls)
	}

	// ...and a DIFFERENT prompt one minute later is itself a new excerpt,
	// so it ALSO needs its own two ticks before confirming — but once
	// confirmed, fires immediately (new episode by hash), well within
	// the 15min reprompt window that would otherwise suppress it.
	srv.updateStuckState("orchestrator-myapp", "tn-myapp", true, false, threeOptionPaneDontAskAt2, now.Add(time.Minute), capture, dialog, sendKeys, false)
	srv.updateStuckState("orchestrator-myapp", "tn-myapp", true, false, threeOptionPaneDontAskAt2, now.Add(time.Minute), capture, dialog, sendKeys, false)
	if dialogCalls != 2 {
		t.Errorf("expected a second dialog for a different prompt within the window, got %d calls", dialogCalls)
	}
}

// --- Dialog content fix: show request context, not just the bare marker ---

// worktreeRelocationExcerpt is the fixture shape reported live: a tool-use
// header, a description of the action, a permission-root line, the bare
// marker question, then the numbered options.
const worktreeRelocationExcerpt = `Tool use: Bash
mkdir -p /some/other/root/worktree-foo
New working directory is outside the allowed permission roots.
Do you want to proceed?
❯ 1. Yes
2. Yes, and don't ask again this session
3. No`

// TestStuckPromptContext_IncludesContextExcludesOptions verifies the
// extracted context runs from the first non-empty line down through the
// marker line, includes the tool description and the permission-root line,
// and drops the numbered options entirely.
func TestStuckPromptContext_IncludesContextExcludesOptions(t *testing.T) {
	ctx := stuckPromptContext(worktreeRelocationExcerpt)

	for _, want := range []string{"Tool use: Bash", "outside the allowed permission roots", "Do you want to proceed?"} {
		if !strings.Contains(ctx, want) {
			t.Errorf("expected context to contain %q, got:\n%s", want, ctx)
		}
	}
	for _, unwanted := range []string{"1. Yes", "don't ask again", "3. No"} {
		if strings.Contains(ctx, unwanted) {
			t.Errorf("expected context to exclude option line %q, got:\n%s", unwanted, ctx)
		}
	}
}

// TestBuildStuckDialogText_EndsWithAttachHint verifies the dialog body
// contains the request context and ends with the tmux attach hint for the
// given session, not the given options.
func TestBuildStuckDialogText_EndsWithAttachHint(t *testing.T) {
	text := buildStuckDialogText(worktreeRelocationExcerpt, "tn-myapp")

	if !strings.Contains(text, "Tool use: Bash") || !strings.Contains(text, "outside the allowed permission roots") {
		t.Errorf("expected dialog text to include the request context, got:\n%s", text)
	}
	if strings.Contains(text, "1. Yes") || strings.Contains(text, "don't ask again") {
		t.Errorf("expected dialog text to exclude the numbered options, got:\n%s", text)
	}
	if !strings.HasSuffix(text, "(tmux attach -t tn-myapp to inspect)") {
		t.Errorf("expected dialog text to end with the attach hint, got:\n%s", text)
	}
}

// TestTruncateMiddle_LongExcerptTruncatedInMiddle verifies an over-long
// excerpt is capped at stuckDialogTextMaxLen with the middle cut out (both
// the start and the end — where the permission line lives — survive).
func TestTruncateMiddle_LongExcerptTruncatedInMiddle(t *testing.T) {
	long := strings.Repeat("x", 1000)
	got := truncateMiddle(long, stuckDialogTextMaxLen)

	if len([]rune(got)) != stuckDialogTextMaxLen {
		t.Fatalf("expected truncated length %d, got %d", stuckDialogTextMaxLen, len([]rune(got)))
	}
	if !strings.Contains(got, "…") {
		t.Errorf("expected an ellipsis marking the truncated middle, got %q", got)
	}
	if !strings.HasPrefix(got, "xxxx") || !strings.HasSuffix(got, "xxxx") {
		t.Errorf("expected both the start and end of the original text to survive, got %q", got)
	}
}

// TestBuildStuckDialogText_LongContextGetsTruncated verifies the
// end-to-end path: a request context long enough to exceed the cap comes
// out truncated in the dialog text (still ending with the attach hint).
func TestBuildStuckDialogText_LongContextGetsTruncated(t *testing.T) {
	longExcerpt := "Tool use: Bash\n" + strings.Repeat("some long output line\n", 60) + "Do you want to proceed?\n❯ 1. Yes\n2. No"
	text := buildStuckDialogText(longExcerpt, "tn-myapp")

	hint := "\n\n(tmux attach -t tn-myapp to inspect)"
	if !strings.HasSuffix(text, hint) {
		t.Fatalf("expected dialog text to end with the attach hint, got:\n%s", text)
	}
	body := strings.TrimSuffix(text, hint)
	if len([]rune(body)) != stuckDialogTextMaxLen {
		t.Errorf("expected the context body capped at %d runes, got %d", stuckDialogTextMaxLen, len([]rune(body)))
	}
	if !strings.Contains(body, "…") {
		t.Errorf("expected the over-long context to be middle-truncated, got:\n%s", body)
	}
}

// --- Excerpt-window fix: the command block was dropping out of the
// displayed excerpt entirely (2026-08-26 live incident, raised by the
// user: "I would like to see the command I'm allowing" — the tray/dialog
// showed the marker+options but not the command sitting above them,
// because stuckExcerptLines (15) only ever captured the tail). ---

// stuckPromptDeepBlockFixture reproduces the reported shape: a tool-use
// header naming the subagent, the actual command, a classifier reason
// block, then padding representing the extra real-terminal lines a wrapped
// long command/reason produces (confirmed live: a 40-line capture showed
// the whole block where 15 did not) — deep enough that the header falls
// outside the last 15 lines but stays inside the last 40.
const stuckPromptDeepBlockFixture = `Bash command · from the svc-9023-redeploy agent
    | curl -s -m 3 http://localhost:9023/health 2>&1; echo; echo "confirming
    | still stale, still up, before next retry"
    Reconfirm service still running (independent check) before next kill retry
Auto mode classifier requires confirmation for this command.
3 consecutive actions were blocked. Please review the transcript before continuing.
Latest blocked action: Blocked by classifier
padding line 1
padding line 2
padding line 3
padding line 4
padding line 5
padding line 6
padding line 7
padding line 8
padding line 9
padding line 10
padding line 11
padding line 12
padding line 13
padding line 14
Do you want to proceed?
❯ 1. Yes
2. Yes, and don't ask again for: curl *
3. No`

// TestUpdateStuckState_ExcerptIncludesCommandBlockBeyondNarrowWindow is the
// regression test for the actual incident: the header/command/reason sit
// beyond stuckExcerptLines (verified below — the narrow window alone does
// NOT reach them) but within stuckContextScanLines, so the DISPLAYED
// excerpt must include them even though live-prompt DETECTION (which stays
// on the narrow window, unchanged) never sees them at all.
func TestUpdateStuckState_ExcerptIncludesCommandBlockBeyondNarrowWindow(t *testing.T) {
	if strings.Contains(lastNLines(stuckPromptDeepBlockFixture, stuckExcerptLines), "svc-9023-redeploy") {
		t.Fatal("fixture is broken: the header must NOT already be inside the narrow detection window, or this test proves nothing")
	}

	srv, _ := newTestServer(t)
	t.Setenv("TN_NO_DIALOG", "1")
	capture := func(session string) (string, error) { return stuckPromptDeepBlockFixture, nil }
	dialog := func(title, text string, buttons []string) (string, error) { return "", nil }
	sendKeys := func(session, keys string) error { return nil }
	now := time.Now()

	// Two identical ticks: the ordinary fastConfirm path every other
	// pane-scraping test in this file uses — no hook involved.
	srv.updateStuckState("orchestrator-myapp", "tn-myapp", true, false, stuckPromptDeepBlockFixture, now, capture, dialog, sendKeys, false)
	srv.updateStuckState("orchestrator-myapp", "tn-myapp", true, false, stuckPromptDeepBlockFixture, now, capture, dialog, sendKeys, false)

	info := srv.stuck.snapshot()["orchestrator-myapp"]
	if !info.Stuck {
		t.Fatalf("expected Stuck true, got %+v", info)
	}
	if !strings.Contains(info.Excerpt, "svc-9023-redeploy") {
		t.Errorf("expected the excerpt to include the subagent attribution, got:\n%s", info.Excerpt)
	}
	if !strings.Contains(info.Excerpt, "curl -s -m 3 http://localhost:9023/health") {
		t.Errorf("expected the excerpt to include the actual command, got:\n%s", info.Excerpt)
	}
	if !strings.Contains(info.Excerpt, "Blocked by classifier") {
		t.Errorf("expected the excerpt to include the classifier reason, got:\n%s", info.Excerpt)
	}
}

// TestBuildDisplayExcerpt_HookCommandPrependedWhenSignatureMatches verifies
// the more-reliable hook-sourced command (immune to any scan-window
// truncation) is prepended when it belongs to the CURRENT prompt episode.
func TestBuildDisplayExcerpt_HookCommandPrependedWhenSignatureMatches(t *testing.T) {
	pane := "Do you want to proceed?\n❯ 1. Yes\n2. No"
	got := buildDisplayExcerpt(pane, "curl -s -m 3 http://localhost:9023/health", "Bash", "sig-a", "sig-a")

	if !strings.HasPrefix(got, "[hook] Bash: curl -s -m 3 http://localhost:9023/health") {
		t.Errorf("expected the hook command prepended, got:\n%s", got)
	}
	if !strings.Contains(got, "Do you want to proceed?") {
		t.Errorf("expected the pane text still present alongside it, got:\n%s", got)
	}
}

// TestBuildDisplayExcerpt_MismatchedSignatureIgnoresHookCommand verifies a
// hook command cached for a DIFFERENT (resolved) episode is never shown
// against a later, unrelated one.
func TestBuildDisplayExcerpt_MismatchedSignatureIgnoresHookCommand(t *testing.T) {
	pane := "Do you want to proceed?\n❯ 1. Yes\n2. No"
	got := buildDisplayExcerpt(pane, "rm -rf /some/stale/episode", "Bash", "sig-old", "sig-new")

	if strings.Contains(got, "rm -rf /some/stale/episode") {
		t.Errorf("expected a stale-episode hook command to be ignored, got:\n%s", got)
	}
}

// TestUpdateStuckState_SameExcerptStaysRateLimited verifies the SAME
// prompt repeated across ticks still only dialogs once within the 15min
// window (this is the existing dedup behavior, now keyed on excerpt hash
// instead of stuck/not-stuck transition).
func TestUpdateStuckState_SameExcerptStaysRateLimited(t *testing.T) {
	srv := newDueScannerTestServer(t, map[string]ProjectConfig{})
	var dialogCalls int
	dialog := func(title, text string, buttons []string) (string, error) {
		dialogCalls++
		return "Dismiss", nil
	}
	capture := func(session string) (string, error) { return "", nil }
	sendKeys := func(session, keys string) error { return nil }

	now := time.Now()
	srv.updateStuckState("orchestrator-myapp", "tn-myapp", true, false, twoOptionPane, now, capture, dialog, sendKeys, false)
	srv.updateStuckState("orchestrator-myapp", "tn-myapp", true, false, twoOptionPane, now.Add(time.Minute), capture, dialog, sendKeys, false)

	if dialogCalls != 1 {
		t.Errorf("expected the same-content prompt to stay deduped within the window, got %d calls", dialogCalls)
	}
}

// TestUpdateStuckState_RepaintingPaneSameOptionsNoNewDialog verifies the
// FINAL shape (fast-path confirmation + sticky display, see
// updateStuckState): a pane repainting (different raw text — elapsed
// counter, spinner, streaming output) while the marker+options stay
// identical must NOT flicker Stuck false, must NOT log a "cleared"
// transition, and must NOT fire a second dialog for what's semantically
// the same episode. Confirmation itself (fastConfirm/backstopConfirm)
// still genuinely requires its own evidence — this test only verifies
// what gets DISPLAYED and LOGGED once already confirmed stays stable
// through a same-signature repaint, which is what makes the underlying
// "any change costs a confirmation tick" rule (see updateStuckState's
// doc comment) safe to ship without spamming the user's feed.
func TestUpdateStuckState_RepaintingPaneSameOptionsNoNewDialog(t *testing.T) {
	srv := newDueScannerTestServer(t, map[string]ProjectConfig{})
	var dialogCalls int
	dialog := func(title, text string, buttons []string) (string, error) {
		dialogCalls++
		return "Dismiss", nil
	}
	sendKeys := func(session, keys string) error { return nil }

	paneTick1 := "elapsed: 12s\nDo you want to proceed?\n❯ 1. Yes\n2. No"
	paneTick2 := "elapsed: 2m14s\nDo you want to proceed?\n❯ 1. Yes\n2. No" // same marker+options, different text
	capture := func(session string) (string, error) { return paneTick2, nil }

	now := time.Now()
	srv.updateStuckState("orchestrator-myapp", "tn-myapp", true, false, paneTick1, now, capture, dialog, sendKeys, false)
	srv.updateStuckState("orchestrator-myapp", "tn-myapp", true, false, paneTick1, now, capture, dialog, sendKeys, false)
	if dialogCalls != 1 {
		t.Fatalf("expected 1 dialog for the first tick, got %d", dialogCalls)
	}

	// One minute later, well within the 15min window: the pane repainted
	// (different elapsed time), same marker+options. Fresh confirmation
	// (fastConfirm) can't fire on this tick's raw-text mismatch, but the
	// DISPLAYED state stays stuck — sticky, since the signature is the
	// same one that was already confirmed.
	srv.updateStuckState("orchestrator-myapp", "tn-myapp", true, false, paneTick2, now.Add(time.Minute), capture, dialog, sendKeys, false)
	if info := srv.stuck.snapshot()["orchestrator-myapp"]; !info.Stuck {
		t.Errorf("expected Stuck to stay true through a same-signature repaint (sticky display), got %+v", info)
	}
	if dialogCalls != 1 {
		t.Errorf("expected no new dialog for a repainted pane with identical options, got %d calls total", dialogCalls)
	}

	// A further tick of the same repainted text: still no new dialog —
	// the sticky display carries it, and LastPromptedHash was never
	// cleared, so even a fresh fastConfirm here (raw text now matches
	// twice in a row) recognizes it as the SAME episode, not a new one.
	srv.updateStuckState("orchestrator-myapp", "tn-myapp", true, false, paneTick2, now.Add(2*time.Minute), capture, dialog, sendKeys, false)
	if dialogCalls != 1 {
		t.Errorf("expected still no second dialog for the same episode, got %d calls total", dialogCalls)
	}
}

// TestUpdateStuckState_ChangedOptionsStillRepromptsImmediately verifies
// the fix's other edge: if the OPTIONS genuinely change (not just cosmetic
// repaint) on the same marker, that's still treated as a new episode and
// reprompts immediately — the signature must be sensitive to option
// changes, not just always suppress reprompts within the window.
func TestUpdateStuckState_ChangedOptionsStillRepromptsImmediately(t *testing.T) {
	srv := newDueScannerTestServer(t, map[string]ProjectConfig{})
	var dialogCalls int
	dialog := func(title, text string, buttons []string) (string, error) {
		dialogCalls++
		return "Dismiss", nil
	}
	capture := func(session string) (string, error) { return "", nil }
	sendKeys := func(session, keys string) error { return nil }

	pane2Opt := "Do you want to proceed?\n❯ 1. Yes\n2. No"
	pane3Opt := "Do you want to proceed?\n❯ 1. Yes\n2. Yes, and don't ask again this session\n3. No"

	now := time.Now()
	srv.updateStuckState("orchestrator-myapp", "tn-myapp", true, false, pane2Opt, now, capture, dialog, sendKeys, false)
	srv.updateStuckState("orchestrator-myapp", "tn-myapp", true, false, pane2Opt, now, capture, dialog, sendKeys, false)
	if dialogCalls != 1 {
		t.Fatalf("expected 1 dialog for the first episode, got %d", dialogCalls)
	}
	srv.updateStuckState("orchestrator-myapp", "tn-myapp", true, false, pane3Opt, now.Add(time.Minute), capture, dialog, sendKeys, false)
	srv.updateStuckState("orchestrator-myapp", "tn-myapp", true, false, pane3Opt, now.Add(time.Minute), capture, dialog, sendKeys, false)
	if dialogCalls != 2 {
		t.Errorf("expected a reprompt when the options genuinely changed, got %d calls", dialogCalls)
	}
}

// TestCheckStuckSessionsOnce_DialogFiredImpliesStuckPromptsReports is the
// reconciliation check: the live incident showed /status.stuckPrompts
// reading 0 while an approval dialog was actively firing for an agent.
// This verifies they can never disagree within a single detector pass —
// st.Stuck is set (which feeds stuckPrompts via stuckTracker.snapshot)
// BEFORE the dialog is ever called, in the same locked section, so an
// agent the dialog just fired for is guaranteed already present as stuck
// in the snapshot.
func TestCheckStuckSessionsOnce_DialogFiredImpliesStuckPromptsReports(t *testing.T) {
	srv := newDueScannerTestServer(t, map[string]ProjectConfig{})
	now := time.Now()
	srv.mu.Lock()
	srv.state.Agents["orchestrator-myapp"] = &Agent{Name: "orchestrator-myapp", Project: "myapp", LastSeenAt: now}
	srv.mu.Unlock()

	capture := func(session string) (string, error) { return twoOptionPane, nil }
	var dialogFired bool
	dialog := func(title, text string, buttons []string) (string, error) {
		dialogFired = true
		return "Dismiss", nil
	}
	sendKeys := func(session, keys string) error { return nil }

	srv.checkStuckSessionsOnce(capture, dialog, sendKeys) // tick 1: candidate
	srv.checkStuckSessionsOnce(capture, dialog, sendKeys) // tick 2: confirmed

	if !dialogFired {
		t.Fatal("expected the dialog to fire for a fresh live prompt")
	}
	info := srv.stuck.snapshot()
	if !info["orchestrator-myapp"].Stuck {
		t.Error("expected stuckByAgent (feeding /status.stuckPrompts) to already report Stuck=true in the SAME pass the dialog fired")
	}
}

// TestDialogFuncForEnv_TNNoDialogSuppressesDialogButNotDetection verifies
// the TN_NO_DIALOG=1 escape hatch: the osascript dialog is never invoked,
// but Stuck/UnackedBacklog state (and therefore /status.stuckPrompts, the
// dashboard flag, and the session-page excerpt, which all read from the
// same stuckTracker) is computed and recorded exactly as normal.
func TestDialogFuncForEnv_TNNoDialogSuppressesDialogButNotDetection(t *testing.T) {
	t.Setenv("TN_NO_DIALOG", "1")
	dialog := dialogFuncForEnv()

	// Verify by IDENTITY before ever calling it: if dialogFuncForEnv ever
	// regressed to returning dialogReal despite TN_NO_DIALOG=1, calling it
	// below would actually invoke osascript on whatever machine runs this
	// suite — exactly the live incident this fix closes. Failing here
	// first means that never happens even under a broken implementation.
	got := reflect.ValueOf(dialog).Pointer()
	want := reflect.ValueOf(dialogSuppressed).Pointer()
	if got != want {
		t.Fatal("expected dialogFuncForEnv() to return dialogSuppressed when TN_NO_DIALOG=1 — refusing to call it, since it may be dialogReal")
	}

	srv := newDueScannerTestServer(t, map[string]ProjectConfig{})
	now := time.Now()
	srv.mu.Lock()
	srv.state.Agents["orchestrator-myapp"] = &Agent{Name: "orchestrator-myapp", Project: "myapp", LastSeenAt: now}
	srv.mu.Unlock()

	capture := func(session string) (string, error) { return twoOptionPane, nil }
	sendKeys := func(session, keys string) error { return nil }

	srv.checkStuckSessionsOnce(capture, dialog, sendKeys) // tick 1: candidate
	srv.checkStuckSessionsOnce(capture, dialog, sendKeys) // tick 2: confirmed

	info := srv.stuck.snapshot()
	if !info["orchestrator-myapp"].Stuck {
		t.Error("expected Stuck=true recorded even with the dialog suppressed")
	}
	if info["orchestrator-myapp"].Excerpt == "" {
		t.Error("expected the excerpt still populated even with the dialog suppressed")
	}
}

// TestDialogFuncForEnv_DefaultReturnsRealDialog verifies TN_NO_DIALOG unset
// (or anything other than "1") keeps the real osascript dialog wired up —
// the suppression is opt-in via env, matching every other TN_NO_* flag's
// convention in this codebase.
func TestDialogFuncForEnv_DefaultReturnsRealDialog(t *testing.T) {
	t.Setenv("TN_NO_DIALOG", "")
	got := reflect.ValueOf(dialogFuncForEnv()).Pointer()
	want := reflect.ValueOf(dialogReal).Pointer()
	if got != want {
		t.Error("expected dialogFuncForEnv() to return dialogReal when TN_NO_DIALOG is unset")
	}
}

// --- Live incident: does the stuck flag actually clear? ---
//
// Reported live: a user approved orchestrator-myapp-g84221's prompt, tmux
// showed a clean working pane with no prompt anywhere on screen, yet
// /status kept reporting stuckPrompts:1 for it. These three tests drive
// checkStuckSessionsOnce through the exact sequence team-lead asked for —
// fixture with a live prompt -> stuck true; next pass with a clean pane ->
// stuck false, excerpt gone; a third pass with a NEW prompt -> stuck true
// again with the alert path re-armed — rather than trusting a single
// fixture or a live eyeball check (the live daemon's stuck state is
// entirely in-memory and gets wiped by any restart, so re-observing the
// original incident after a rebuild proves nothing either way).

// TestStuckStateSequence_ClearsOnCleanPaneThenReArmsOnNewPrompt is the
// basic three-pass sequence with a genuinely different second prompt.
func TestStuckStateSequence_ClearsOnCleanPaneThenReArmsOnNewPrompt(t *testing.T) {
	srv := newDueScannerTestServer(t, map[string]ProjectConfig{})
	now := time.Now()
	srv.mu.Lock()
	srv.state.Agents["orchestrator-myapp-g1"] = &Agent{Name: "orchestrator-myapp-g1", Project: "myapp", LastSeenAt: now}
	srv.mu.Unlock()

	var pane string
	capture := func(session string) (string, error) { return pane, nil }
	var dialogCalls int
	dialog := func(title, text string, buttons []string) (string, error) {
		dialogCalls++
		return "Dismiss", nil
	}
	sendKeys := func(session, keys string) error { return nil }

	// Pass 1: a live prompt. Two identical ticks — two-tick confirmation
	// (updateStuckState) — before it's treated as genuinely stuck.
	pane = "Do you want to proceed?\n❯ 1. Yes\n2. No"
	srv.checkStuckSessionsOnce(capture, dialog, sendKeys)
	srv.checkStuckSessionsOnce(capture, dialog, sendKeys)
	info := srv.stuck.snapshot()
	if !info["orchestrator-myapp-g1"].Stuck || info["orchestrator-myapp-g1"].Excerpt == "" {
		t.Fatalf("pass 1: expected stuck=true with an excerpt, got %+v", info["orchestrator-myapp-g1"])
	}
	if dialogCalls != 1 {
		t.Fatalf("pass 1: expected the dialog shown once, got %d calls", dialogCalls)
	}

	// Pass 2: the prompt is answered — a clean pane, nothing stuck-looking.
	pane = "some normal Claude output, no prompt here\n"
	srv.checkStuckSessionsOnce(capture, dialog, sendKeys)
	info = srv.stuck.snapshot()
	if info["orchestrator-myapp-g1"].Stuck {
		t.Fatalf("pass 2: expected stuck=false on a clean pane, got %+v", info["orchestrator-myapp-g1"])
	}
	if info["orchestrator-myapp-g1"].Excerpt != "" {
		t.Errorf("pass 2: expected the excerpt cleared, got %q", info["orchestrator-myapp-g1"].Excerpt)
	}
	if dialogCalls != 1 {
		t.Fatalf("pass 2: expected no new dialog on a clean pane, got %d calls", dialogCalls)
	}

	// Pass 3: a genuinely NEW, differently-shaped prompt — the alert path
	// must re-arm, not stay silenced by the first episode's history. Two
	// identical ticks again for its own two-tick confirmation.
	pane = "requires confirmation\n❯ 1. Allow\n2. Deny\n3. Always allow"
	srv.checkStuckSessionsOnce(capture, dialog, sendKeys)
	srv.checkStuckSessionsOnce(capture, dialog, sendKeys)
	info = srv.stuck.snapshot()
	if !info["orchestrator-myapp-g1"].Stuck || info["orchestrator-myapp-g1"].Excerpt == "" {
		t.Fatalf("pass 3: expected stuck=true again with an excerpt, got %+v", info["orchestrator-myapp-g1"])
	}
	if dialogCalls != 2 {
		t.Errorf("pass 3: expected the alert path to re-arm (a 2nd dialog) for a new episode, got %d calls total", dialogCalls)
	}
}

// TestStuckStateSequence_IdenticallyShapedRecurrenceStillReArms is the
// sharper edge case: the SAME kind of prompt (identical marker + option
// shape, e.g. a recurring worktree-relocation prompt — see
// stuckSessionState.LastPromptedHash's own doc comment for this exact
// scenario) recurs AFTER being resolved. If LastPromptedHash isn't reset
// when the agent clears, this looks like "still the same un-prompted
// episode" and the dialog silently never fires again — exactly the
// alerting-disarm failure mode team-lead flagged (tray-alert.sh dedupes
// on the episode signature, so a stale signature permanently muffles the
// next real occurrence of that same shape).
func TestStuckStateSequence_IdenticallyShapedRecurrenceStillReArms(t *testing.T) {
	srv := newDueScannerTestServer(t, map[string]ProjectConfig{})
	now := time.Now()
	srv.mu.Lock()
	srv.state.Agents["orchestrator-myapp-g1"] = &Agent{Name: "orchestrator-myapp-g1", Project: "myapp", LastSeenAt: now}
	srv.mu.Unlock()

	const promptPane = "Do you want to proceed?\n❯ 1. Yes\n2. No"
	var pane string
	capture := func(session string) (string, error) { return pane, nil }
	var dialogCalls int
	dialog := func(title, text string, buttons []string) (string, error) {
		dialogCalls++
		return "Dismiss", nil
	}
	sendKeys := func(session, keys string) error { return nil }

	pane = promptPane
	srv.checkStuckSessionsOnce(capture, dialog, sendKeys) // tick: candidate
	srv.checkStuckSessionsOnce(capture, dialog, sendKeys) // tick: confirmed
	if dialogCalls != 1 {
		t.Fatalf("pass 1: expected the dialog shown once, got %d calls", dialogCalls)
	}

	pane = "clean, no prompt\n"
	srv.checkStuckSessionsOnce(capture, dialog, sendKeys)
	if srv.stuck.snapshot()["orchestrator-myapp-g1"].Stuck {
		t.Fatal("pass 2: expected stuck=false on a clean pane")
	}

	// Pass 3: the SAME shape of prompt recurs — a genuinely new episode
	// (the agent was unstuck in between), not a continuation of the
	// first. Its own two-tick confirmation again (the clean pass reset
	// the pending-candidate tracking).
	pane = promptPane
	srv.checkStuckSessionsOnce(capture, dialog, sendKeys)
	srv.checkStuckSessionsOnce(capture, dialog, sendKeys)
	if !srv.stuck.snapshot()["orchestrator-myapp-g1"].Stuck {
		t.Fatal("pass 3: expected stuck=true again")
	}
	if dialogCalls != 2 {
		t.Errorf("pass 3: expected the alert to re-fire for a NEW episode of an identically-shaped prompt (got %d calls total) — "+
			"a stale LastPromptedHash from the resolved first episode would silently suppress this", dialogCalls)
	}
}

// --- Liveness gate removal: capture unconditionally, alert on
// DeregisteredAt instead (see checkStuckSessionsOnce's doc comment) ---

// TestCheckStuckSessionsOnce_StalePollingButRegistered_StillAlerts is the
// regression test for the actual bug: an agent that's registered (never
// deregistered) but has stopped polling its inbox — indistinguishable
// from "not alive" by the OLD gate (agentAliveAt/aliveWindow), and
// EXACTLY what an agent blocked on a permission dialog looks like, since
// it can't run its own polling loop while blocked — must still be
// captured and alerted on. Before this fix, the old `!agentAliveAt`
// top-of-loop gate would have skipped this agent entirely and this test
// would have failed.
func TestCheckStuckSessionsOnce_StalePollingButRegistered_StillAlerts(t *testing.T) {
	srv := newDueScannerTestServer(t, map[string]ProjectConfig{})
	srv.mu.Lock()
	srv.state.Agents["orchestrator-myapp-g1"] = &Agent{
		Name: "orchestrator-myapp-g1", Project: "myapp",
		// Stale well past aliveWindow, never deregistered — the "blocked
		// on a dialog, hasn't polled" shape.
		LastSeenAt: time.Now().Add(-10 * time.Minute),
	}
	srv.mu.Unlock()

	capture := func(session string) (string, error) {
		return "Do you want to proceed?\n❯ 1. Yes\n2. No", nil
	}
	var dialogCalls int
	dialog := func(title, text string, buttons []string) (string, error) {
		dialogCalls++
		return "Dismiss", nil
	}
	sendKeys := func(session, keys string) error { return nil }

	srv.checkStuckSessionsOnce(capture, dialog, sendKeys) // tick 1: candidate
	srv.checkStuckSessionsOnce(capture, dialog, sendKeys) // tick 2: confirmed

	info := srv.stuck.snapshot()["orchestrator-myapp-g1"]
	if !info.Stuck || info.Excerpt == "" {
		t.Fatalf("expected the stale-but-registered agent to be recorded stuck with an excerpt, got %+v", info)
	}
	if dialogCalls != 1 {
		t.Errorf("expected the dialog to fire for it, got %d calls", dialogCalls)
	}

	statusResp := buildStatusResponse(dashboardSnapshot{Agents: []Agent{*srv.state.Agents["orchestrator-myapp-g1"]}},
		srv.stuck.snapshot(), nil, nil, nil, map[string]ProjectConfig{"myapp": {AutoSpawn: true}},
		fixedRepoSettings(defaultRepoSettings()), time.Now(), 8391, false, nil, time.Time{}, 0, "", nil, nil, nil)
	found := false
	for _, sp := range statusResp.StuckPrompts {
		if sp.Agent == "orchestrator-myapp-g1" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected orchestrator-myapp-g1 in /status.stuckPrompts, got %+v", statusResp.StuckPrompts)
	}
}

// TestCheckStuckSessionsOnce_DeregisteredAgent_NeverAlertsEvenWithAPrompt
// verifies the OTHER half: a deregistered agent's pane may still be
// captured (its tmux session is deliberately never killed — forensic
// scrollback), but it must never alert and must never re-alert on
// repeated ticks, even though the pane keeps showing an old prompt.
func TestCheckStuckSessionsOnce_DeregisteredAgent_NeverAlertsEvenWithAPrompt(t *testing.T) {
	srv := newDueScannerTestServer(t, map[string]ProjectConfig{})
	deregisteredAt := time.Now().Add(-time.Hour)
	srv.mu.Lock()
	srv.state.Agents["orchestrator-myapp-g0"] = &Agent{
		Name: "orchestrator-myapp-g0", Project: "myapp",
		LastSeenAt: deregisteredAt, DeregisteredAt: &deregisteredAt,
	}
	srv.mu.Unlock()

	var captureCalls int
	capture := func(session string) (string, error) {
		captureCalls++
		// Still shows an old, never-answered prompt — genuine zombie pane.
		return "Do you want to proceed?\n❯ 1. Yes\n2. No", nil
	}
	var dialogCalls int
	dialog := func(title, text string, buttons []string) (string, error) {
		dialogCalls++
		return "Dismiss", nil
	}
	sendKeys := func(session, keys string) error { return nil }

	srv.checkStuckSessionsOnce(capture, dialog, sendKeys)
	srv.checkStuckSessionsOnce(capture, dialog, sendKeys)
	srv.checkStuckSessionsOnce(capture, dialog, sendKeys)

	if captureCalls != 3 {
		t.Errorf("expected the pane still captured every pass (cheap, keeps the scrollback path exercised), got %d calls", captureCalls)
	}
	if dialogCalls != 0 {
		t.Errorf("expected NO dialog ever, across 3 passes, for a deregistered agent, got %d calls", dialogCalls)
	}

	info := srv.stuck.snapshot()["orchestrator-myapp-g0"]
	if info.Stuck {
		t.Errorf("expected the tracker to never record Stuck=true for a deregistered agent, got %+v", info)
	}

	// Belt-and-suspenders: even if some other path had left a stale
	// Stuck=true in the tracker, /status must not surface it for a
	// deregistered agent (buildStatusResponse's own DeregisteredAt guard
	// on stuckOut).
	srv.mu.Lock()
	srv.stuck.mu.Lock()
	srv.stuck.byName["orchestrator-myapp-g0"] = &stuckSessionState{Stuck: true, Excerpt: "stale"}
	srv.stuck.mu.Unlock()
	srv.mu.Unlock()
	statusResp := buildStatusResponse(dashboardSnapshot{Agents: []Agent{*srv.state.Agents["orchestrator-myapp-g0"]}},
		srv.stuck.snapshot(), nil, nil, nil, map[string]ProjectConfig{"myapp": {AutoSpawn: true}},
		fixedRepoSettings(defaultRepoSettings()), time.Now(), 8391, false, nil, time.Time{}, 0, "", nil, nil, nil)
	for _, sp := range statusResp.StuckPrompts {
		if sp.Agent == "orchestrator-myapp-g0" {
			t.Errorf("expected a deregistered agent excluded from /status.stuckPrompts even with a forced-stale Stuck=true, got %+v", statusResp.StuckPrompts)
		}
	}
}

// --- Progress-diff detector: a dialog signature alone doesn't confirm
// stuck — it must be present AND unchanged across two consecutive ticks
// (see updateStuckState). Live-confirmed false positive this fixes:
// g53286 showed a resolved "Teach auto mode about your environment?"
// dialog's text still sitting in the scan window while a running
// subagent below it had a climbing token count and elapsed timer —
// positionally identical to a genuine block, distinguishable only by
// whether the window changed between ticks. ---

// TestCheckStuckSessionsOnce_SingleTickNeverConfirmsStuck locks in the
// two-tick requirement directly: one pass with a dialog signature must
// NOT confirm Stuck or fire the dialog, even though the OLD (pre-fix)
// single-tick detector would have. This is the one-tick cost team-lead
// explicitly accepted in exchange for not disarming tray-alert.sh on a
// stale-dialog false positive.
func TestCheckStuckSessionsOnce_SingleTickNeverConfirmsStuck(t *testing.T) {
	srv := newDueScannerTestServer(t, map[string]ProjectConfig{})
	srv.mu.Lock()
	srv.state.Agents["orchestrator-myapp"] = &Agent{Name: "orchestrator-myapp", Project: "myapp", LastSeenAt: time.Now()}
	srv.mu.Unlock()

	capture := func(session string) (string, error) { return twoOptionPane, nil }
	var dialogCalls int
	dialog := func(title, text string, buttons []string) (string, error) {
		dialogCalls++
		return "Dismiss", nil
	}
	sendKeys := func(session, keys string) error { return nil }

	srv.checkStuckSessionsOnce(capture, dialog, sendKeys)

	info := srv.stuck.snapshot()["orchestrator-myapp"]
	if info.Stuck {
		t.Errorf("expected Stuck=false after a single tick (two-tick confirmation required), got %+v", info)
	}
	if dialogCalls != 0 {
		t.Errorf("expected no dialog after a single tick, got %d calls", dialogCalls)
	}

	// A second identical tick DOES confirm it.
	srv.checkStuckSessionsOnce(capture, dialog, sendKeys)
	info = srv.stuck.snapshot()["orchestrator-myapp"]
	if !info.Stuck {
		t.Errorf("expected Stuck=true after a second identical tick, got %+v", info)
	}
	if dialogCalls != 1 {
		t.Errorf("expected exactly 1 dialog once confirmed, got %d calls", dialogCalls)
	}
}

// TestCheckStuckSessionsOnce_ProgressBetweenTicksNeverConfirms is the
// direct reproduction of the g53286 false positive: a dialog signature
// present on both ticks, but the scanned window changed between them
// (a climbing token count / elapsed timer below the stale dialog text,
// exactly what a running subagent produces) — must never confirm.
func TestCheckStuckSessionsOnce_ProgressBetweenTicksNeverConfirms(t *testing.T) {
	srv := newDueScannerTestServer(t, map[string]ProjectConfig{})
	srv.mu.Lock()
	srv.state.Agents["orchestrator-myapp"] = &Agent{Name: "orchestrator-myapp", Project: "myapp", LastSeenAt: time.Now()}
	srv.mu.Unlock()

	tick1 := "Teach auto mode about your environment?\n❯ 1. Yes\n2. Not now\n3. Don't show again\nEnter to confirm · Esc to cancel\n" +
		"❯ \n[CAVEMAN:ULTRA]\n⏵⏵ auto mode on · 1 monitor\n⏺ main\n◯ subagent  Locating file... 1m 41s · ↓ 61.4k tokens"
	tick2 := "Teach auto mode about your environment?\n❯ 1. Yes\n2. Not now\n3. Don't show again\nEnter to confirm · Esc to cancel\n" +
		"❯ \n[CAVEMAN:ULTRA]\n⏵⏵ auto mode on · 1 monitor\n⏺ main\n◯ subagent  Locating file... 1m 46s · ↓ 62.1k tokens" // token count / elapsed time climbed
	var tick int
	capture := func(session string) (string, error) {
		tick++
		if tick == 1 {
			return tick1, nil
		}
		return tick2, nil
	}
	var dialogCalls int
	dialog := func(title, text string, buttons []string) (string, error) {
		dialogCalls++
		return "Dismiss", nil
	}
	sendKeys := func(session, keys string) error { return nil }

	srv.checkStuckSessionsOnce(capture, dialog, sendKeys) // tick1: candidate
	srv.checkStuckSessionsOnce(capture, dialog, sendKeys) // tick2: signature still present, but window CHANGED

	info := srv.stuck.snapshot()["orchestrator-myapp"]
	if info.Stuck {
		t.Errorf("expected Stuck=false — the subagent's climbing token count/elapsed timer proves real progress, not a genuine block — got %+v", info)
	}
	if dialogCalls != 0 {
		t.Errorf("expected no dialog for a stale dialog beside real progress, got %d calls", dialogCalls)
	}
}

// --- Persistence backstop: the fast path's own blind spot, per
// team-lead's explicit shape — ORCHESTRATOR.md has agents own long-
// running background jobs, which keep emitting output while the
// foreground is frozen on a permission prompt. That produces a genuinely
// blocked agent whose pane STILL changes tick to tick (the fast path's
// exact-match requirement can never confirm it), so the backstop tracks
// the dialog SIGNATURE persisting across stuckPersistenceBackstopTicks
// consecutive ticks regardless of what else changed, and confirms once
// that streak is reached — late, never silent. Bias toward alerting when
// ambiguous: this deliberately means a stale-dialog-beside-real-progress
// pane (the false positive the fast path exists to reject) WILL still
// eventually alert if it persists the full backstop window — accepted
// per team-lead: a false positive costs an unnecessary notification, a
// false negative costs a hung agent nobody notices. ---

// syntheticBackgroundJobPane returns a pane with a STATIC dialog block
// (same marker+options every time — the agent genuinely blocked) plus a
// SEPARATELY updating region below it (tick-numbered output — a
// background job it owns still running). No real blocked Claude needed:
// the detector only ever sees captured text.
func syntheticBackgroundJobPane(tick int) string {
	return fmt.Sprintf(
		"Do you want to proceed?\n❯ 1. Yes\n2. No\nEsc to cancel\n"+
			"[background job] processed batch %d, %d records so far",
		tick, tick*137,
	)
}

// TestCheckStuckSessionsOnce_PersistenceBackstopFiresOnGenuineBlockWithChangingOutput
// is the direct synthesis of the case team-lead identified as untested:
// a genuinely blocked dialog beside a background job's own changing
// output. The fast path (exact raw-text match) can NEVER confirm this —
// the pane changes every tick — so only the backstop catches it, and
// only after stuckPersistenceBackstopTicks consecutive ticks, never
// sooner.
func TestCheckStuckSessionsOnce_PersistenceBackstopFiresOnGenuineBlockWithChangingOutput(t *testing.T) {
	srv := newDueScannerTestServer(t, map[string]ProjectConfig{})
	srv.mu.Lock()
	srv.state.Agents["orchestrator-myapp"] = &Agent{Name: "orchestrator-myapp", Project: "myapp", LastSeenAt: time.Now()}
	srv.mu.Unlock()

	tick := 0
	capture := func(session string) (string, error) {
		tick++
		return syntheticBackgroundJobPane(tick), nil
	}
	var dialogCalls int
	dialog := func(title, text string, buttons []string) (string, error) {
		dialogCalls++
		return "Dismiss", nil
	}
	sendKeys := func(session, keys string) error { return nil }

	for i := 1; i < stuckPersistenceBackstopTicks; i++ {
		srv.checkStuckSessionsOnce(capture, dialog, sendKeys)
		info := srv.stuck.snapshot()["orchestrator-myapp"]
		if info.Stuck {
			t.Fatalf("tick %d: expected NOT yet confirmed (backstop needs %d ticks), got Stuck=true", i, stuckPersistenceBackstopTicks)
		}
		if dialogCalls != 0 {
			t.Fatalf("tick %d: expected no dialog before the backstop fires, got %d calls", i, dialogCalls)
		}
	}

	// The Nth tick reaches the backstop threshold.
	srv.checkStuckSessionsOnce(capture, dialog, sendKeys)
	info := srv.stuck.snapshot()["orchestrator-myapp"]
	if !info.Stuck {
		t.Fatalf("tick %d: expected the backstop to confirm a genuine block despite the changing background-job output, got Stuck=false", stuckPersistenceBackstopTicks)
	}
	if dialogCalls != 1 {
		t.Errorf("expected exactly 1 dialog once the backstop fires, got %d calls", dialogCalls)
	}
}

// TestCheckStuckSessionsOnce_StaleDialogWithProgress_SilentBeforeBackstop
// verifies the OTHER direction explicitly for MULTIPLE ticks, not just
// two: a stale dialog beside real, unrelated progress (the g53286 shape)
// must stay silent for every tick short of the backstop threshold — the
// fast path correctly never confirms it, and the backstop hasn't reached
// its count yet either.
func TestCheckStuckSessionsOnce_StaleDialogWithProgress_SilentBeforeBackstop(t *testing.T) {
	srv := newDueScannerTestServer(t, map[string]ProjectConfig{})
	srv.mu.Lock()
	srv.state.Agents["orchestrator-myapp"] = &Agent{Name: "orchestrator-myapp", Project: "myapp", LastSeenAt: time.Now()}
	srv.mu.Unlock()

	tick := 0
	capture := func(session string) (string, error) {
		tick++
		return syntheticBackgroundJobPane(tick), nil
	}
	var dialogCalls int
	dialog := func(title, text string, buttons []string) (string, error) {
		dialogCalls++
		return "Dismiss", nil
	}
	sendKeys := func(session, keys string) error { return nil }

	// Same signature persisting with changing text is INDISTINGUISHABLE,
	// from captured text alone, between "genuinely blocked with a
	// background job" and "resolved dialog beside real progress" — that
	// is exactly why the backstop is a bounded, late fallback and not an
	// instant one. Confirm it stays silent for every tick short of the
	// threshold.
	for i := 1; i < stuckPersistenceBackstopTicks; i++ {
		srv.checkStuckSessionsOnce(capture, dialog, sendKeys)
	}
	info := srv.stuck.snapshot()["orchestrator-myapp"]
	if info.Stuck {
		t.Errorf("expected still not confirmed with %d ticks (backstop needs %d), got Stuck=true", stuckPersistenceBackstopTicks-1, stuckPersistenceBackstopTicks)
	}
	if dialogCalls != 0 {
		t.Errorf("expected no dialog before the backstop threshold, got %d calls", dialogCalls)
	}
}

// TestCheckStuckSessionsOnce_ConfirmedThenRepaint_NoClearLogOrActivityEntry
// verifies the sticky-display requirement directly: once genuinely
// confirmed, repaint ticks (same signature, changing raw text — a
// background job's own output) must NOT log a stuck->clear transition or
// append a "cleared" bridge activity entry — a dialog still on screen has
// not cleared, and logging that it did would put noise straight into the
// feed the user reads.
func TestCheckStuckSessionsOnce_ConfirmedThenRepaint_NoClearLogOrActivityEntry(t *testing.T) {
	srv := newDueScannerTestServer(t, map[string]ProjectConfig{})
	srv.mu.Lock()
	srv.state.Agents["orchestrator-myapp"] = &Agent{Name: "orchestrator-myapp", Project: "myapp", LastSeenAt: time.Now()}
	srv.mu.Unlock()

	// Confirm via the fast path first (two identical ticks).
	capture := func(session string) (string, error) { return twoOptionPane, nil }
	dialog := func(title, text string, buttons []string) (string, error) { return "Dismiss", nil }
	sendKeys := func(session, keys string) error { return nil }
	srv.checkStuckSessionsOnce(capture, dialog, sendKeys)
	srv.checkStuckSessionsOnce(capture, dialog, sendKeys)
	if info := srv.stuck.snapshot()["orchestrator-myapp"]; !info.Stuck {
		t.Fatalf("test setup broken: expected confirmed stuck before the repaint ticks, got %+v", info)
	}

	// Now repaint every tick (same signature, changing raw text) for
	// several ticks, well short of the backstop threshold.
	tick := 0
	capture = func(session string) (string, error) {
		tick++
		return syntheticBackgroundJobPane(tick), nil
	}
	for i := 0; i < stuckPersistenceBackstopTicks-1; i++ {
		srv.checkStuckSessionsOnce(capture, dialog, sendKeys)
	}

	if info := srv.stuck.snapshot()["orchestrator-myapp"]; !info.Stuck {
		t.Errorf("expected Stuck to stay true through repaint ticks (sticky display), got %+v", info)
	}

	srv.mu.Lock()
	defer srv.mu.Unlock()
	for _, a := range srv.state.Activity {
		if strings.Contains(a.Text, "cleared") {
			t.Errorf("expected no 'cleared' activity entry while the dialog is still on screen (repainting, not resolved), got %q", a.Text)
		}
	}
}

// TestUpdateStuckState_TextRepeatsInPairs_ExactlyOneDialogForSameEpisode
// is the episode-identity invariant, tested by name per team-lead:
// episode identity IS the signature (marker+options) — full stop, not
// "signature plus whether the fast path happened to byte-match this
// particular tick." A pane whose text repeats in PAIRS (matches, changes,
// matches again — the exact shape a repainting element produces) with
// the SAME marker+options throughout must fire exactly ONE dialog for
// the whole sequence, below the persistence-backstop threshold, because
// it is one episode from start to finish. Before decoupling the dialog-
// dedup ledger (LastPromptedHash/LastPrompted) from the fast path's
// transient byte-level clears — see updateStuckState's "Sticky display"
// section — a second "changes, matches again" pair re-fired a second
// dialog for the same episode: precisely the prompt-storm shape, just
// bounded instead of unbounded. tray-alert.sh dedupes on this same
// signature notion via its own .last-stuck marker and is the live,
// user-facing alert path (the daemon's own osascript dialog is disabled
// via TN_NO_DIALOG=1 in the plist) — a second episode there re-notifies
// the user for a prompt they already saw, which is how an alert channel
// gets trained into background noise.
func TestUpdateStuckState_TextRepeatsInPairs_ExactlyOneDialogForSameEpisode(t *testing.T) {
	srv := newDueScannerTestServer(t, map[string]ProjectConfig{})
	var dialogCalls int
	dialog := func(title, text string, buttons []string) (string, error) {
		dialogCalls++
		return "Dismiss", nil
	}
	sendKeys := func(session, keys string) error { return nil }
	capture := func(session string) (string, error) { return "", nil }

	textA := "elapsed: 12s\nDo you want to proceed?\n❯ 1. Yes\n2. No"
	textB := "elapsed: 47s\nDo you want to proceed?\n❯ 1. Yes\n2. No" // same marker+options, different raw text

	now := time.Now()
	// matches (A, A) -> confirms, dialog #1
	srv.updateStuckState("orchestrator-myapp", "tn-myapp", true, false, textA, now, capture, dialog, sendKeys, false)
	srv.updateStuckState("orchestrator-myapp", "tn-myapp", true, false, textA, now, capture, dialog, sendKeys, false)
	// changes (B) -> fast path can't confirm this tick; sticky display
	// (and the dedup ledger) must NOT treat this as the episode clearing
	srv.updateStuckState("orchestrator-myapp", "tn-myapp", true, false, textB, now.Add(time.Minute), capture, dialog, sendKeys, false)
	// matches again (B, B) -> fast path re-confirms, but it is the SAME
	// episode by signature — must NOT fire a second dialog
	srv.updateStuckState("orchestrator-myapp", "tn-myapp", true, false, textB, now.Add(2*time.Minute), capture, dialog, sendKeys, false)

	if dialogCalls != 1 {
		t.Errorf("expected exactly ONE dialog for the whole repeats-in-pairs sequence (same episode throughout), got %d calls", dialogCalls)
	}
}

// --- claude.ai usage-limit ("rate limited") detection ---
//
// All fixtures below are verbatim pane text from a real limited session,
// not invented shapes — the whole point of this detector is matching what
// Claude Code actually prints.

const (
	rateLimitPaneSessionLimit = "You've hit your session limit · resets 2:30pm (Europe/Berlin)\n" +
		"/usage-credits to finish what you’re working on."
	rateLimitPaneContinuing = "You've hit your session limit · resets 2:30pm (Europe/Berlin)\n" +
		"Continuing automatically at 2:30pm · esc to cancel\n" +
		"/usage-credits to continue now\n" +
		"\n" +
		"⏺ Usage limit reached · continuing automatically at 2:30pm · esc or type to cancel"
	rateLimitPaneTeammate = "⏺ Teammate @doc-placement-research failed: You've hit your session limit · resets 2:30pm (Europe/Berlin)"
	rateLimitPaneReset    = "Your claude.ai usage limit has reset. Continue the task you were working on when the " +
		"limit was reached; do not repeat work that is already complete."
)

// TestDetectRateLimitWait covers each marker the detector accepts, the
// explicit reset banner that ends an episode, and a normal idle pane that
// must never register.
func TestDetectRateLimitWait(t *testing.T) {
	cases := []struct {
		name string
		pane string
		want bool
	}{
		{"session limit banner", rateLimitPaneSessionLimit, true},
		{"session limit with continuing-automatically block", rateLimitPaneContinuing, true},
		{"teammate failure carrying the limit text", rateLimitPaneTeammate, true},
		{"hit your limit (non-session wording)", "You've hit your limit · resets 2:30pm (Europe/Berlin)", true},
		{"usage limit reached alone", "⏺ Usage limit reached", true},
		{"continuing automatically alone", "Continuing automatically at 2:30pm · esc to cancel", true},
		{"case-insensitive", "YOU'VE HIT YOUR SESSION LIMIT", true},
		{"limit lifted — banner still in window but explicitly reset",
			rateLimitPaneContinuing + "\n" + rateLimitPaneReset, false},
		{"normal idle pane", "Running tests...\nAll tests passed\n$", false},
		{"empty", "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, got := detectRateLimitWait(c.pane)
			if got != c.want {
				t.Errorf("detectRateLimitWait(%q) = %v, want %v", c.pane, got, c.want)
			}
		})
	}
}

// TestDetectRateLimitWait_ResetText verifies the raw reset string is
// carried through as printed — "resets ..." preferred, "continuing
// automatically at ..." as fallback, cut at end-of-line or a " ·"
// separator, never parsed into a time.
func TestDetectRateLimitWait_ResetText(t *testing.T) {
	cases := []struct {
		name string
		pane string
		want string
	}{
		{"resets form keeps the timezone label", rateLimitPaneSessionLimit, "2:30pm (Europe/Berlin)"},
		{"resets wins over continuing-automatically when both present", rateLimitPaneContinuing, "2:30pm (Europe/Berlin)"},
		{"teammate line", rateLimitPaneTeammate, "2:30pm (Europe/Berlin)"},
		{"continuing-automatically fallback cuts at the separator",
			"Continuing automatically at 2:30pm · esc to cancel", "2:30pm"},
		{"usage limit reached with no time at all", "⏺ Usage limit reached", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, limited := detectRateLimitWait(c.pane)
			if !limited {
				t.Fatalf("expected detectRateLimitWait to report a limit for %q", c.pane)
			}
			if got != c.want {
				t.Errorf("reset text = %q, want %q", got, c.want)
			}
		})
	}
}

// TestDetectRateLimitWait_OnlyScansRecentLines verifies a limit banner
// buried far above the visible tail is not treated as a live wait — same
// scrollback discipline the permission-prompt detector applies, just with
// a wider window (rateLimitScanLines).
func TestDetectRateLimitWait_OnlyScansRecentLines(t *testing.T) {
	pane := rateLimitPaneContinuing + "\n" + strings.Repeat("still working\n", 60)
	if _, limited := detectRateLimitWait(pane); limited {
		t.Error("expected a limit banner outside the last ~30 lines to not register as a live wait")
	}
}

// newRateLimitTestServer registers one agent whose tmux session is
// tn-<project>, the shape checkStuckSessionsOnce derives for a legacy
// agent, so a single injected capture drives the whole detector pass.
func newRateLimitTestServer(t *testing.T, agentName string) *Server {
	t.Helper()
	srv := newDueScannerTestServer(t, map[string]ProjectConfig{})
	srv.mu.Lock()
	srv.state.Agents[agentName] = &Agent{Name: agentName, Project: "myapp", LastSeenAt: time.Now()}
	srv.mu.Unlock()
	return srv
}

// TestCheckStuckSessionsOnce_RateLimitDetectedAndCleared walks a full
// episode: detected on the first tick (no two-tick confirmation — unlike
// a permission prompt there is nothing ambiguous to double-check and
// nothing to answer), then cleared by the explicit reset banner.
func TestCheckStuckSessionsOnce_RateLimitDetectedAndCleared(t *testing.T) {
	srv := newRateLimitTestServer(t, "orchestrator-myapp")
	pane := rateLimitPaneContinuing
	capture := func(session string) (string, error) { return pane, nil }
	dialog := func(title, text string, buttons []string) (string, error) { return "OK", nil }
	sendKeys := func(session, keys string) error { return nil }

	before := time.Now()
	srv.checkStuckSessionsOnce(capture, dialog, sendKeys)
	info := srv.stuck.snapshot()["orchestrator-myapp"]
	if !info.RateLimited {
		t.Fatalf("expected RateLimited after one tick, got %+v", info)
	}
	if info.RateLimitResetsAt != "2:30pm (Europe/Berlin)" {
		t.Errorf("RateLimitResetsAt = %q, want %q", info.RateLimitResetsAt, "2:30pm (Europe/Berlin)")
	}
	if info.RateLimitSince.Before(before) {
		t.Errorf("expected RateLimitSince stamped at detection, got %v", info.RateLimitSince)
	}
	if info.Stuck {
		t.Error("a usage-limit wait must not be reported as a permission-prompt stuck")
	}

	pane = rateLimitPaneContinuing + "\n" + rateLimitPaneReset
	srv.checkStuckSessionsOnce(capture, dialog, sendKeys)
	info = srv.stuck.snapshot()["orchestrator-myapp"]
	if info.RateLimited || info.RateLimitResetsAt != "" || !info.RateLimitSince.IsZero() {
		t.Errorf("expected the episode cleared by the reset banner, got %+v", info)
	}
}

// TestCheckStuckSessionsOnce_RateLimitClearsWhenMarkersVanish is the
// other clear path: the banner simply scrolls away as the session resumes,
// with no explicit reset line ever landing in the window.
func TestCheckStuckSessionsOnce_RateLimitClearsWhenMarkersVanish(t *testing.T) {
	srv := newRateLimitTestServer(t, "orchestrator-myapp")
	pane := rateLimitPaneSessionLimit
	capture := func(session string) (string, error) { return pane, nil }
	dialog := func(title, text string, buttons []string) (string, error) { return "OK", nil }
	sendKeys := func(session, keys string) error { return nil }

	srv.checkStuckSessionsOnce(capture, dialog, sendKeys)
	if !srv.stuck.snapshot()["orchestrator-myapp"].RateLimited {
		t.Fatal("expected RateLimited on the limit pane")
	}

	pane = "⏺ Reading serve.go\n  ⎿ Read 200 lines\n"
	srv.checkStuckSessionsOnce(capture, dialog, sendKeys)
	if srv.stuck.snapshot()["orchestrator-myapp"].RateLimited {
		t.Error("expected RateLimited cleared once the markers left the scanned window")
	}
}

// TestCheckStuckSessionsOnce_RateLimitDialogOncePerEpisode verifies the
// notification fires exactly once for an episode no matter how many ticks
// it spans (there is nothing to approve, so re-showing it would be pure
// nagging), fires again when the reset time CHANGES (the previous
// notification's headline fact is now wrong), and re-arms after the
// episode ends so a later, identically-shaped limit still alerts.
func TestCheckStuckSessionsOnce_RateLimitDialogOncePerEpisode(t *testing.T) {
	srv := newRateLimitTestServer(t, "orchestrator-myapp")
	pane := rateLimitPaneContinuing
	capture := func(session string) (string, error) { return pane, nil }
	var titles, texts []string
	dialog := func(title, text string, buttons []string) (string, error) {
		titles = append(titles, title)
		texts = append(texts, text)
		if len(buttons) != 1 || buttons[0] != "OK" {
			t.Errorf("expected a single OK button, got %v", buttons)
		}
		return "OK", nil
	}
	sendKeys := func(session, keys string) error { return nil }

	srv.checkStuckSessionsOnce(capture, dialog, sendKeys)
	srv.checkStuckSessionsOnce(capture, dialog, sendKeys)
	srv.checkStuckSessionsOnce(capture, dialog, sendKeys)
	if len(titles) != 1 {
		t.Fatalf("expected exactly one dialog for a continuing episode, got %d: %v", len(titles), titles)
	}
	if titles[0] != "Usage limit — orchestrator-myapp" {
		t.Errorf("dialog title = %q, want %q", titles[0], "Usage limit — orchestrator-myapp")
	}
	for _, want := range []string{"resets 2:30pm (Europe/Berlin)", "continues automatically at reset", "/login", "/usage-credits"} {
		if !strings.Contains(texts[0], want) {
			t.Errorf("dialog text %q missing %q", texts[0], want)
		}
	}

	// The limit moves: a new reset string is a new episode.
	pane = "You've hit your session limit · resets 4:00pm (Europe/Berlin)"
	srv.checkStuckSessionsOnce(capture, dialog, sendKeys)
	if len(titles) != 2 {
		t.Fatalf("expected a second dialog when the reset time changed, got %d: %v", len(titles), titles)
	}

	// Episode ends, then an identically-shaped one recurs: must alert
	// again rather than being suppressed by the previous episode's
	// bookkeeping (same re-arming requirement the stuck dialog has).
	pane = "⏺ back to work\n"
	srv.checkStuckSessionsOnce(capture, dialog, sendKeys)
	pane = "You've hit your session limit · resets 4:00pm (Europe/Berlin)"
	srv.checkStuckSessionsOnce(capture, dialog, sendKeys)
	if len(titles) != 3 {
		t.Errorf("expected a fresh dialog for a NEW episode of an identically-shaped limit, got %d: %v", len(titles), titles)
	}
}

// TestCheckStuckSessionsOnce_RateLimitAndStuckAreIndependent verifies
// neither condition suppresses detection of the other: a pane can carry a
// usage-limit banner AND a live permission prompt, and both flags must be
// set (each on its own confirmation schedule).
func TestCheckStuckSessionsOnce_RateLimitAndStuckAreIndependent(t *testing.T) {
	srv := newRateLimitTestServer(t, "orchestrator-myapp")
	pane := "You've hit your session limit · resets 2:30pm (Europe/Berlin)\n" +
		"Do you want to proceed?\n❯ 1. Yes\n2. No"
	capture := func(session string) (string, error) { return pane, nil }
	dialog := func(title, text string, buttons []string) (string, error) { return "", nil }
	sendKeys := func(session, keys string) error { return nil }

	srv.checkStuckSessionsOnce(capture, dialog, sendKeys) // rate limit confirms here
	srv.checkStuckSessionsOnce(capture, dialog, sendKeys) // stuck confirms here (two-tick)

	info := srv.stuck.snapshot()["orchestrator-myapp"]
	if !info.RateLimited {
		t.Error("expected RateLimited alongside a live permission prompt")
	}
	if !info.Stuck {
		t.Error("expected Stuck alongside a usage-limit banner")
	}
}

// TestStatusAndDashboard_RateLimitedAgent covers the reporting surface:
// /status carries rateLimited/rateLimitResetsAt/rateLimitSince, the
// backlog fields keep their real values while rate-limited (the backlog is
// expected, not fictional), and the DISPLAYED badge — dashboard row and
// session page — defers to the usage-limit marker instead of the backlog
// alarm.
func TestStatusAndDashboard_RateLimitedAgent(t *testing.T) {
	now := time.Now()
	since := now.Add(-5 * time.Minute)
	agent := Agent{Name: "orchestrator-myapp-g1", Project: "myapp", Accepting: true, LastSeenAt: now.Add(-20 * time.Minute)}
	stuckByAgent := map[string]stuckInfo{
		"orchestrator-myapp-g1": {
			UnackedBacklog:    true,
			RateLimited:       true,
			RateLimitResetsAt: "2:30pm (Europe/Berlin)",
			RateLimitSince:    since,
		},
	}

	statusResp := buildStatusResponse(dashboardSnapshot{Agents: []Agent{agent}},
		stuckByAgent, nil, nil, nil, map[string]ProjectConfig{"myapp": {AutoSpawn: true}},
		fixedRepoSettings(defaultRepoSettings()), now, 8391, false, nil, time.Time{}, 0, "", nil, nil, nil)

	var got *statusAgent
	for i, sa := range statusResp.Projects["myapp"].Agents {
		if sa.Name == "orchestrator-myapp-g1" {
			got = &statusResp.Projects["myapp"].Agents[i]
		}
	}
	if got == nil {
		t.Fatalf("agent missing from /status: %+v", statusResp.Projects)
	}
	if !got.RateLimited || got.RateLimitResetsAt != "2:30pm (Europe/Berlin)" {
		t.Errorf("expected rate-limit fields on the /status agent, got %+v", got)
	}
	if got.RateLimitSince == nil || !got.RateLimitSince.Equal(since) {
		t.Errorf("expected rateLimitSince = %v, got %v", since, got.RateLimitSince)
	}
	if !got.UnackedBacklog {
		t.Error("expected unackedBacklog still REPORTED while rate-limited — only the badge defers, not the field")
	}

	rows := buildSessionRows([]Agent{agent}, nil, now, "", stuckByAgent)
	if len(rows) != 1 {
		t.Fatalf("expected one session row, got %d", len(rows))
	}
	status := sessionStatusDisplay(rows[0].Alive, rows[0].Stuck, rows[0].UnackedBacklog, rows[0].Draining, rows[0].RateLimited, rows[0].RateLimitResetsAt)
	if status != "⏳ usage limit (resets 2:30pm (Europe/Berlin))" {
		t.Errorf("dashboard status = %q, want the usage-limit marker instead of gone/backlog", status)
	}

	page := renderSessionPageMarkdown(agent, false, nil, now, stuckByAgent["orchestrator-myapp-g1"])
	if !strings.Contains(page, "## Waiting: claude.ai usage limit") ||
		!strings.Contains(page, "Resets: 2:30pm (Europe/Berlin)") {
		t.Errorf("session page missing the usage-limit block:\n%s", page)
	}
}

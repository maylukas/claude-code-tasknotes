package tn

import (
	"strings"
	"testing"
	"time"
)

// --- Stranded-message reassignment: the 2026-08-27 defect ---
//
// Root-cause analysis (verified live against production state, not
// theory): 31 queued/delivered messages sat permanently addressed to
// non-alive or never-valid recipients. handleRetire already reassigns
// pending mail on a CLEAN retire, but a generation that dies WITHOUT
// calling it — the common case — had NO code path that ever reassigns its
// mail. Two complementary mechanisms close this:
//   - reassignStrandedMessagesForDeadAgent, reusing the EXISTING
//     confirmed-dead signal (checkOrphanedDeathsOnce's deadConfirmTicks),
//     for messages addressed to a known-but-dead agent.
//   - sweepUnroutableMessagesOnce, for messages addressed to a name that
//     was NEVER a valid agent or queue at all (e.g. "claude/setup").

// seedPendingMessage adds a queued message addressed to `to`, optionally
// carrying taskPath, directly into server state — the shape a real
// "Task assigned:" (or any other) dispatch would have left behind.
func seedPendingMessage(srv *Server, id, to, taskPath, text string) {
	srv.mu.Lock()
	srv.state.Messages = append(srv.state.Messages, &Message{
		ID: id, To: to, TaskPath: taskPath, Text: text, Status: "queued",
		CreatedAt: time.Now().Add(-time.Hour),
	})
	srv.mu.Unlock()
}

func messageByID(srv *Server, id string) *Message {
	srv.mu.Lock()
	defer srv.mu.Unlock()
	for _, m := range srv.state.Messages {
		if m.ID == id {
			return m
		}
	}
	return nil
}

// killAgent backdates name's LastSeenAt so it reads as dead (isAlive false)
// without going through a real inbox-poll timeout.
func killAgent(srv *Server, name string) {
	srv.mu.Lock()
	srv.state.Agents[name].LastSeenAt = time.Now().Add(-5 * time.Minute)
	srv.mu.Unlock()
}

// TestReassignStrandedMessagesForDeadAgent_DirtyDeathReassignsToLiveSibling
// is the core regression test: a message with no TaskPath, addressed to an
// agent that died WITHOUT retiring, is reassigned to a live accepting
// sibling once the death is confirmed (deadConfirmTicks consecutive
// misses) — the exact gap handleRetire never covered.
func TestReassignStrandedMessagesForDeadAgent_DirtyDeathReassignsToLiveSibling(t *testing.T) {
	t.Setenv("TN_ENABLE_ORPHAN_SWEEP", "1")
	apiSrv := fakeOrphanTaskAPI(t, map[string]Task{})
	srv, ts := newTestServer(t)
	srv.tnClient = NewClient(Config{URL: apiSrv.URL})
	srv.startedAt = time.Now().Add(-2 * aliveWindow)

	register(t, ts, "orchestrator-myapp-g1", "myapp")
	register(t, ts, "orchestrator-myapp-g2", "myapp")
	seedPendingMessage(srv, "m1", "orchestrator-myapp-g1", "", "please review the design doc")
	killAgent(srv, "orchestrator-myapp-g1")

	srv.checkOrphanedDeathsOnce() // 1st miss — not confirmed yet
	if m := messageByID(srv, "m1"); m.To != "orchestrator-myapp-g1" || m.Status != "queued" {
		t.Fatalf("expected message untouched after a single missed tick, got %+v", m)
	}

	srv.checkOrphanedDeathsOnce() // 2nd consecutive miss — confirmed dead
	m := messageByID(srv, "m1")
	if m.To != "orchestrator-myapp-g2" {
		t.Errorf("expected message reassigned to the live sibling, got To=%q", m.To)
	}
	if m.Status != "queued" || m.DeliveredAt != nil {
		t.Errorf("expected reassigned message back to queued with no DeliveredAt, got status=%q deliveredAt=%v", m.Status, m.DeliveredAt)
	}
}

// TestReassignStrandedMessagesForDeadAgent_SingleMissedTickDoesNotReassign
// is the false-positive guard: reassignment must reuse the SAME
// confirmation discipline as task-orphan detection, not judge liveness
// independently — a single missed poll is common for an agent doing real
// synchronous work and must never move its mail.
func TestReassignStrandedMessagesForDeadAgent_SingleMissedTickDoesNotReassign(t *testing.T) {
	t.Setenv("TN_ENABLE_ORPHAN_SWEEP", "1")
	apiSrv := fakeOrphanTaskAPI(t, map[string]Task{})
	srv, ts := newTestServer(t)
	srv.tnClient = NewClient(Config{URL: apiSrv.URL})
	srv.startedAt = time.Now().Add(-2 * aliveWindow)

	register(t, ts, "orchestrator-myapp-g1", "myapp")
	register(t, ts, "orchestrator-myapp-g2", "myapp")
	seedPendingMessage(srv, "m1", "orchestrator-myapp-g1", "", "please review the design doc")
	killAgent(srv, "orchestrator-myapp-g1")

	srv.checkOrphanedDeathsOnce()

	if m := messageByID(srv, "m1"); m.To != "orchestrator-myapp-g1" {
		t.Errorf("expected no reassignment after only one missed tick, got To=%q", m.To)
	}
}

// TestReassignStrandedMessagesForDeadAgent_TaskDoneArchivesInstead verifies
// requirement 1 of the fix: a stale "Task assigned:"-shaped message whose
// referenced task is already done must NOT be bulk-delivered as if new —
// that would recreate the duplicate-work problem the reassignment exists
// to fix in the first place. It archives, with a reason recorded.
func TestReassignStrandedMessagesForDeadAgent_TaskDoneArchivesInstead(t *testing.T) {
	t.Setenv("TN_ENABLE_ORPHAN_SWEEP", "1")
	const path = "Tasks/AlreadyDone.md"
	apiSrv := fakeOrphanTaskAPI(t, map[string]Task{
		path: {Path: path, Title: "Bump the thing", Status: "done"},
	})
	srv, ts := newTestServer(t)
	srv.tnClient = NewClient(Config{URL: apiSrv.URL})
	srv.startedAt = time.Now().Add(-2 * aliveWindow)

	register(t, ts, "orchestrator-myapp-g1", "myapp")
	register(t, ts, "orchestrator-myapp-g2", "myapp")
	seedPendingMessage(srv, "m1", "orchestrator-myapp-g1", path, "Task assigned: Bump the thing ("+path+")")
	killAgent(srv, "orchestrator-myapp-g1")

	srv.checkOrphanedDeathsOnce()
	srv.checkOrphanedDeathsOnce()

	m := messageByID(srv, "m1")
	if m.Status != "archived" {
		t.Fatalf("expected the message archived, not reassigned, got status=%q to=%q", m.Status, m.To)
	}
	if m.To != "orchestrator-myapp-g1" {
		t.Errorf("expected To left as the original (historical) recipient, got %q", m.To)
	}
	if m.ArchivedAt == nil || m.ArchiveReason == "" {
		t.Errorf("expected ArchivedAt/ArchiveReason set, got %+v", m)
	}
	if !strings.Contains(m.ArchiveReason, path) {
		t.Errorf("expected the archive reason to name the done task, got %q", m.ArchiveReason)
	}
}

// TestReassignStrandedMessagesForDeadAgent_TaskStillOpenReassigns verifies
// the other half: a still-open (or in-progress, needs-input, etc.) task's
// assignment message DOES get reassigned, not archived.
func TestReassignStrandedMessagesForDeadAgent_TaskStillOpenReassigns(t *testing.T) {
	t.Setenv("TN_ENABLE_ORPHAN_SWEEP", "1")
	const path = "Tasks/StillOpen.md"
	apiSrv := fakeOrphanTaskAPI(t, map[string]Task{
		path: {Path: path, Title: "Fix the leak", Status: "in-progress"},
	})
	srv, ts := newTestServer(t)
	srv.tnClient = NewClient(Config{URL: apiSrv.URL})
	srv.startedAt = time.Now().Add(-2 * aliveWindow)

	register(t, ts, "orchestrator-myapp-g1", "myapp")
	register(t, ts, "orchestrator-myapp-g2", "myapp")
	seedPendingMessage(srv, "m1", "orchestrator-myapp-g1", path, "Task assigned: Fix the leak ("+path+")")
	killAgent(srv, "orchestrator-myapp-g1")

	srv.checkOrphanedDeathsOnce()
	srv.checkOrphanedDeathsOnce()

	m := messageByID(srv, "m1")
	if m.Status != "queued" || m.To != "orchestrator-myapp-g2" {
		t.Errorf("expected the message reassigned to the live sibling since its task is still open, got status=%q to=%q", m.Status, m.To)
	}
	if !strings.HasPrefix(m.Text, strandedMessageProvenanceMarker) {
		t.Errorf("expected a reassigned message to carry the stale-provenance banner (task-open is only a proxy for still-relevant — see the 2026-08-27 terminated-VM incident), got:\n%s", m.Text)
	}
}

// TestReassignStrandedMessagesForDeadAgent_TaskFetchErrorReassignsConservatively
// verifies the "absence of signal is not signal of absence" discipline
// this daemon uses everywhere else: a TaskNotes fetch failure (network
// blip, deleted task, whatever) must NOT be treated as "task is done" —
// only a CONFIRMED done status may archive. Anything unknown reassigns.
func TestReassignStrandedMessagesForDeadAgent_TaskFetchErrorReassignsConservatively(t *testing.T) {
	t.Setenv("TN_ENABLE_ORPHAN_SWEEP", "1")
	apiSrv := fakeOrphanTaskAPI(t, map[string]Task{}) // empty — GetTask 404s for anything
	srv, ts := newTestServer(t)
	srv.tnClient = NewClient(Config{URL: apiSrv.URL})
	srv.startedAt = time.Now().Add(-2 * aliveWindow)

	register(t, ts, "orchestrator-myapp-g1", "myapp")
	register(t, ts, "orchestrator-myapp-g2", "myapp")
	seedPendingMessage(srv, "m1", "orchestrator-myapp-g1", "Tasks/Gone.md", "Task assigned: Gone (Tasks/Gone.md)")
	killAgent(srv, "orchestrator-myapp-g1")

	srv.checkOrphanedDeathsOnce()
	srv.checkOrphanedDeathsOnce()

	m := messageByID(srv, "m1")
	if m.Status != "queued" || m.To != "orchestrator-myapp-g2" {
		t.Errorf("expected a fetch error to reassign (not archive) conservatively, got status=%q to=%q", m.Status, m.To)
	}
}

// TestReassignStrandedMessagesForDeadAgent_NoAcceptingSiblingUsesLogicalQueue
// verifies the fallback when no live accepting agent exists for the
// project: the message moves to the logical orchestrator-<slug> queue, the
// same one a fresh registration will retarget via
// retargetLogicalQueueLocked — not left stranded, not a malformed name.
func TestReassignStrandedMessagesForDeadAgent_NoAcceptingSiblingUsesLogicalQueue(t *testing.T) {
	t.Setenv("TN_ENABLE_ORPHAN_SWEEP", "1")
	apiSrv := fakeOrphanTaskAPI(t, map[string]Task{})
	srv, ts := newTestServer(t)
	srv.tnClient = NewClient(Config{URL: apiSrv.URL})
	srv.startedAt = time.Now().Add(-2 * aliveWindow)

	register(t, ts, "orchestrator-myapp-g1", "myapp") // the only agent for myapp
	seedPendingMessage(srv, "m1", "orchestrator-myapp-g1", "", "please review the design doc")
	killAgent(srv, "orchestrator-myapp-g1")

	srv.checkOrphanedDeathsOnce()
	srv.checkOrphanedDeathsOnce()

	m := messageByID(srv, "m1")
	if m.To != "orchestrator-myapp" {
		t.Errorf("expected the message moved to the logical orchestrator-myapp queue, got To=%q", m.To)
	}
	if m.Status != "queued" {
		t.Errorf("expected status queued (so a fresh registration's retarget picks it up), got %q", m.Status)
	}
}

// --- sweepUnroutableMessagesOnce: names that were never valid at all ---

// TestSweepUnroutableMessagesOnce_NeverValidRecipientArchives is the
// regression test for the OTHER half of the live incident: 18 of 31
// stranded messages were addressed to "claude/setup", an identity that was
// never a real registered agent — no agent record exists to judge dead vs
// alive, so this must be handled independently of the dead-agent path.
func TestSweepUnroutableMessagesOnce_NeverValidRecipientArchives(t *testing.T) {
	t.Setenv("TN_ENABLE_ORPHAN_SWEEP", "1")
	apiSrv := fakeOrphanTaskAPI(t, map[string]Task{})
	srv, _ := newTestServer(t)
	srv.tnClient = NewClient(Config{URL: apiSrv.URL})

	seedPendingMessage(srv, "m1", "claude/setup", "", "welcome message")

	srv.sweepUnroutableMessagesOnce()

	m := messageByID(srv, "m1")
	if m.Status != "archived" {
		t.Fatalf("expected the message archived, got status=%q", m.Status)
	}
	if m.ArchivedAt == nil || !strings.Contains(m.ArchiveReason, "claude/setup") {
		t.Errorf("expected an archive reason naming the never-valid recipient, got %+v", m)
	}
}

// TestSweepUnroutableMessagesOnce_MalformedOrchestratorPrefixArchives
// covers the specific malformed-name incident: a bare "orchestrator-" with
// nothing after the dash (composed by an earlier, unguarded version of
// orchestratorQueueName when project was "") matches no register call and
// never will — it must be archived, not left stranded forever.
func TestSweepUnroutableMessagesOnce_MalformedOrchestratorPrefixArchives(t *testing.T) {
	t.Setenv("TN_ENABLE_ORPHAN_SWEEP", "1")
	apiSrv := fakeOrphanTaskAPI(t, map[string]Task{})
	srv, _ := newTestServer(t)
	srv.tnClient = NewClient(Config{URL: apiSrv.URL})

	seedPendingMessage(srv, "m1", "orchestrator-", "", "stranded by an empty project")

	srv.sweepUnroutableMessagesOnce()

	if m := messageByID(srv, "m1"); m.Status != "archived" {
		t.Errorf("expected the malformed-recipient message archived, got status=%q to=%q", m.Status, m.To)
	}
}

// TestSweepUnroutableMessagesOnce_LegitimatePendingQueueLeftAlone verifies
// the sweep does NOT touch a message correctly waiting in a logical
// orchestrator-<slug> queue for a project the daemon actually knows about
// (an autoSpawn config entry) — that's normal in-flight queueing ahead of
// a fresh spawn, not a bug, and retargetLogicalQueueLocked is what's
// supposed to drain it once a generation registers.
func TestSweepUnroutableMessagesOnce_LegitimatePendingQueueLeftAlone(t *testing.T) {
	t.Setenv("TN_ENABLE_ORPHAN_SWEEP", "1")
	apiSrv := fakeOrphanTaskAPI(t, map[string]Task{})
	srv, _ := newTestServer(t)
	srv.tnClient = NewClient(Config{URL: apiSrv.URL})
	srv.config.Projects = map[string]ProjectConfig{"myapp": {AutoSpawn: true, Cwd: "/tmp"}}

	seedPendingMessage(srv, "m1", "orchestrator-myapp", "", "waiting for a fresh generation")

	srv.sweepUnroutableMessagesOnce()

	if m := messageByID(srv, "m1"); m.Status != "queued" || m.To != "orchestrator-myapp" {
		t.Errorf("expected the legitimate pending queue message left untouched, got status=%q to=%q", m.Status, m.To)
	}
}

// TestSweepUnroutableMessagesOnce_GatedByOrphanSweepFlag verifies this
// sweep respects the same opt-in gate as the rest of the orphan-handling
// family (default OFF) — archiving is a real, visible action and must not
// fire silently before the flag is deliberately enabled.
func TestSweepUnroutableMessagesOnce_GatedByOrphanSweepFlag(t *testing.T) {
	apiSrv := fakeOrphanTaskAPI(t, map[string]Task{})
	srv, _ := newTestServer(t)
	srv.tnClient = NewClient(Config{URL: apiSrv.URL})

	seedPendingMessage(srv, "m1", "claude/setup", "", "welcome message")

	srv.sweepUnroutableMessagesOnce()

	if m := messageByID(srv, "m1"); m.Status != "queued" {
		t.Errorf("expected no action while TN_ENABLE_ORPHAN_SWEEP is unset, got status=%q", m.Status)
	}
}

// --- orchestratorQueueName / pickReassignmentTargetLocked ---

func TestOrchestratorQueueName(t *testing.T) {
	cases := []struct{ project, want string }{
		{"myapp", "orchestrator-myapp"},
		{"MYAPP", "orchestrator-myapp"},
		{"  myapp  ", "orchestrator-myapp"},
		{"", ""},
		{"   ", ""},
	}
	for _, c := range cases {
		if got := orchestratorQueueName(c.project); got != c.want {
			t.Errorf("orchestratorQueueName(%q) = %q, want %q", c.project, got, c.want)
		}
	}
}

// --- stampStrandedProvenance: "open" is only a proxy for "still relevant" ---

// TestStampStrandedProvenance_PrependsOriginalDateAndCaveat is the
// regression test for the 2026-08-27 follow-up: the archive-vs-reassign
// decision only asks whether a referenced TASK is closed, which cannot
// detect that the WORLD a still-open task describes has changed (the live
// example: a task about a specific VM stayed "open" after that VM was
// terminated). A reassigned message must carry its original send date and
// a caveat, not arrive looking freshly written.
func TestStampStrandedProvenance_PrependsOriginalDateAndCaveat(t *testing.T) {
	sentAt := time.Date(2026, 8, 21, 10, 0, 0, 0, time.UTC)
	got := stampStrandedProvenance("Reinstall the app APK on tainted VMs", sentAt)

	if !strings.HasPrefix(got, strandedMessageProvenanceMarker) {
		t.Fatalf("expected the provenance marker prefix, got:\n%s", got)
	}
	if !strings.Contains(got, "2026-08-21") {
		t.Errorf("expected the original send date in the banner, got:\n%s", got)
	}
	if !strings.Contains(got, "Reinstall the app APK on tainted VMs") {
		t.Errorf("expected the original text preserved, got:\n%s", got)
	}
}

// TestStampStrandedProvenance_IdempotentAgainstDoubleStamping verifies a
// message that somehow gets reassigned twice (a repeat sweep pass racing
// something else) never stacks a second banner on top of the first.
func TestStampStrandedProvenance_IdempotentAgainstDoubleStamping(t *testing.T) {
	sentAt := time.Date(2026, 8, 21, 10, 0, 0, 0, time.UTC)
	once := stampStrandedProvenance("original text", sentAt)
	twice := stampStrandedProvenance(once, sentAt)

	if once != twice {
		t.Errorf("expected stamping an already-stamped message to be a no-op, got:\nonce:  %q\ntwice: %q", once, twice)
	}
}

// --- sweepStrandedMessagesAtStartup: recovering a backlog from BEFORE this daemon restart ---

// TestSweepStrandedMessagesAtStartup_RecoversCrossRestartDeadRecipient is
// the regression test for the exact headline case team-lead found live:
// orchestrator-myapp-g17165, dead since well before the daemon's current
// restart and never re-observed this lifetime, holding stranded mail that
// reassignStrandedMessagesForDeadAgent alone can never reach (it only
// sees agents THIS process watched transition from alive to dead).
// Recovery here goes through the SAME tmux-session-confirmed-gone check
// deadTaskOwners already uses for owned tasks — a positive, verifiable
// fact, not an inference from silence.
func TestSweepStrandedMessagesAtStartup_RecoversCrossRestartDeadRecipient(t *testing.T) {
	t.Setenv("TN_ENABLE_ORPHAN_SWEEP", "1")
	apiSrv := fakeOrphanTaskAPI(t, map[string]Task{})
	srv, ts := newTestServer(t)
	srv.tnClient = NewClient(Config{URL: apiSrv.URL})
	srv.startedAt = time.Now().Add(-2 * aliveWindow) // clear of the startup grace period (no sleep)

	const session = "tn-myapp-g17165"
	seedUnobservedAgent(srv, "orchestrator-myapp-g17165", session) // never seen THIS lifetime — the cross-restart case
	register(t, ts, "orchestrator-myapp-g2", "myapp")              // live sibling to receive the reassignment
	seedPendingMessage(srv, "m1", "orchestrator-myapp-g17165", "", "Task assigned: Bump catalog:example")

	srv.sweepStrandedMessagesAtStartup(srv.tnClient, fakeTmuxSessionExistence(map[string]tmuxSessionState{
		session: tmuxSessionGone,
	}))

	m := messageByID(srv, "m1")
	if m.To != "orchestrator-myapp-g2" || m.Status != "queued" {
		t.Errorf("expected the cross-restart-dead recipient's mail reassigned to the live sibling, got To=%q status=%q", m.To, m.Status)
	}
}

// TestSweepStrandedMessagesAtStartup_HungNotDeadLeftAlone verifies the
// same three-state discipline as deadTaskOwners applies here: a tmux
// session that still EXISTS means hung, not dead (the stuck detector's
// case, not this one's) — its mail must be left alone, not reassigned out
// from under a session that might still resume.
func TestSweepStrandedMessagesAtStartup_HungNotDeadLeftAlone(t *testing.T) {
	t.Setenv("TN_ENABLE_ORPHAN_SWEEP", "1")
	apiSrv := fakeOrphanTaskAPI(t, map[string]Task{})
	srv, ts := newTestServer(t)
	srv.tnClient = NewClient(Config{URL: apiSrv.URL})
	srv.startedAt = time.Now().Add(-2 * aliveWindow)

	const session = "tn-myapp-g17165"
	seedUnobservedAgent(srv, "orchestrator-myapp-g17165", session)
	register(t, ts, "orchestrator-myapp-g2", "myapp")
	seedPendingMessage(srv, "m1", "orchestrator-myapp-g17165", "", "Task assigned: still hung, not dead")

	srv.sweepStrandedMessagesAtStartup(srv.tnClient, fakeTmuxSessionExistence(map[string]tmuxSessionState{
		session: tmuxSessionExists,
	}))

	if m := messageByID(srv, "m1"); m.To != "orchestrator-myapp-g17165" {
		t.Errorf("expected a HUNG (session exists) recipient's mail left alone, got To=%q", m.To)
	}
}

// --- resolveTargetLocked / dispatchMessage: fail loudly, don't queue into a void ---

// TestDispatchMessage_EmptyProjectNoRouteDropsWithoutQueueing is the
// regression test for the root cause behind the malformed "orchestrator-"
// messages: routeTaskNotification (and similar internal callers) can
// derive an empty project (a task with no claude-project/projects[] set)
// and dispatch through with no explicit `to` either. The old code silently
// queued this to a bare "orchestrator-" that could never be delivered; the
// fix must drop it instead — visibly (a log line), never silently, but
// also never by manufacturing an address nothing will ever answer to.
func TestDispatchMessage_EmptyProjectNoRouteDropsWithoutQueueing(t *testing.T) {
	srv, _ := newTestServer(t)

	before := len(queuedMessagesTo(srv, "orchestrator-"))
	if before != 0 {
		t.Fatalf("test setup: expected no pre-existing messages to the malformed name")
	}

	msg, status, _ := srv.dispatchMessage(sendRequest{Project: "", TaskPath: "Tasks/NoProject.md", Text: "Task assigned: something"})

	if status != 400 {
		t.Errorf("expected a 400-class failure status, got %d", status)
	}
	if msg.ID != "" {
		t.Errorf("expected a zero Message returned (nothing queued), got %+v", msg)
	}
	srv.mu.Lock()
	total := len(srv.state.Messages)
	srv.mu.Unlock()
	if total != 0 {
		t.Errorf("expected NOTHING queued for an unroutable request, got %d message(s)", total)
	}
}

// TestDispatchMessage_ValidProjectStillRoutesToLogicalQueue is the sanity
// check alongside the fix above: a NORMAL empty-`to`/no-owner/no-accepting-
// agent dispatch with a real project must still queue to the logical
// orchestrator-<slug> queue exactly as before — the new guard must only
// reject the genuinely unroutable (empty project) case, not regress the
// ordinary "nobody's alive yet, queue it for the next spawn" path.
func TestDispatchMessage_ValidProjectStillRoutesToLogicalQueue(t *testing.T) {
	srv, _ := newTestServer(t)

	msg, status, extra := srv.dispatchMessage(sendRequest{Project: "myapp", Text: "hello"})

	if msg.To != "orchestrator-myapp" {
		t.Errorf("expected the message queued to the logical orchestrator-myapp queue, got To=%q", msg.To)
	}
	if status != 202 {
		t.Errorf("expected 202 Accepted (queued, no live agent), got %d", status)
	}
	if extra["queued"] != true {
		t.Errorf("expected extra[queued]=true, got %+v", extra)
	}
}

package tn

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"
)

// --- fakeUsageClient ------------------------------------------------------

// fakeUsageResult scripts one Fetch outcome.
type fakeUsageResult struct {
	snap   *usageSnapshot
	status int
	err    error
}

// fakeRefreshResult scripts one Refresh outcome.
type fakeRefreshResult struct {
	access, refresh string
	expiresAt       time.Time
	status          int
	err             error
}

// fakeUsageClient is the usageClient tests inject — no test in this
// package may ever construct httpUsageClient (that would mean a real
// network call to api.anthropic.com/platform.claude.com). Outcomes are
// scripted per token, consumed in order; a call past the scripted sequence
// fails the test loudly rather than hanging or silently returning a zero
// value.
type fakeUsageClient struct {
	mu           sync.Mutex
	fetch        map[string][]fakeUsageResult
	refresh      map[string][]fakeRefreshResult
	refreshCalls []string // refresh tokens, in call order
	fetchCalls   []string // access tokens, in call order
}

func newFakeUsageClient() *fakeUsageClient {
	return &fakeUsageClient{fetch: map[string][]fakeUsageResult{}, refresh: map[string][]fakeRefreshResult{}}
}

func (f *fakeUsageClient) scriptFetch(accessToken string, r fakeUsageResult) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fetch[accessToken] = append(f.fetch[accessToken], r)
}

func (f *fakeUsageClient) scriptRefresh(refreshToken string, r fakeRefreshResult) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.refresh[refreshToken] = append(f.refresh[refreshToken], r)
}

func (f *fakeUsageClient) Fetch(ctx context.Context, accessToken string) (*usageSnapshot, int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fetchCalls = append(f.fetchCalls, accessToken)
	seq := f.fetch[accessToken]
	if len(seq) == 0 {
		return nil, 0, fmt.Errorf("fakeUsageClient: no scripted Fetch left for token %q", accessToken)
	}
	r := seq[0]
	f.fetch[accessToken] = seq[1:]
	return r.snap, r.status, r.err
}

func (f *fakeUsageClient) Refresh(ctx context.Context, refreshToken string) (access, refresh string, expiresAt time.Time, status int, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.refreshCalls = append(f.refreshCalls, refreshToken)
	seq := f.refresh[refreshToken]
	if len(seq) == 0 {
		return "", "", time.Time{}, 0, fmt.Errorf("fakeUsageClient: no scripted Refresh left for token %q", refreshToken)
	}
	r := seq[0]
	f.refresh[refreshToken] = seq[1:]
	return r.access, r.refresh, r.expiresAt, r.status, r.err
}

// --- parseUsageSnapshot ----------------------------------------------------

func TestParseUsageSnapshot_PrefersLimitsFallsBackToWindows(t *testing.T) {
	now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	resetsSession := "2026-09-09T11:39:59.886685+00:00"
	resetsWeekly := "2026-09-11T03:59:59.886710+00:00"
	raw := usageAPIResponse{
		FiveHour: &usageAPIWindow{Utilization: 999, ResetsAt: nil}, // must be ignored: limits[] present
		SevenDay: &usageAPIWindow{Utilization: 999, ResetsAt: nil},
		Limits: []usageAPILimit{
			{Kind: "session", Percent: 3, ResetsAt: &resetsSession},
			{Kind: "weekly_all", Percent: 0, ResetsAt: &resetsWeekly},
			{Kind: "weekly_scoped", Percent: 1, ResetsAt: &resetsWeekly},
		},
	}
	snap := parseUsageSnapshot(raw, now)
	if snap.Session.Percent != 3 || snap.Session.ResetsAt.IsZero() {
		t.Errorf("session = %+v", snap.Session)
	}
	if snap.Weekly.Percent != 0 {
		t.Errorf("weekly = %+v", snap.Weekly)
	}
	if snap.WeeklyScoped == nil || snap.WeeklyScoped.Percent != 1 {
		t.Errorf("weeklyScoped = %+v", snap.WeeklyScoped)
	}

	// No limits[] at all: falls back to five_hour/seven_day.
	raw2 := usageAPIResponse{
		FiveHour: &usageAPIWindow{Utilization: 42, ResetsAt: &resetsSession},
		SevenDay: nil, // null seven_day: weekly stays zero, no panic
	}
	snap2 := parseUsageSnapshot(raw2, now)
	if snap2.Session.Percent != 42 || snap2.Session.ResetsAt.IsZero() {
		t.Errorf("fallback session = %+v", snap2.Session)
	}
	if snap2.Weekly.Percent != 0 || !snap2.Weekly.ResetsAt.IsZero() {
		t.Errorf("expected zero weekly with no seven_day, got %+v", snap2.Weekly)
	}

	// Fully empty response: no panic, everything zero.
	snap3 := parseUsageSnapshot(usageAPIResponse{}, now)
	if snap3.Session.Percent != 0 || snap3.Weekly.Percent != 0 || snap3.WeeklyScoped != nil {
		t.Errorf("empty response = %+v", snap3)
	}
}

// --- poller: dead/revive/strikes -------------------------------------------

func newUsageTestServer(t *testing.T) (*Server, *credsStore, *fakeKeychain) {
	t.Helper()
	srv := newDueScannerTestServer(t, map[string]ProjectConfig{})
	c, kc := newTestCredsStore(t)
	srv.creds = c
	srv.config.Credentials = CredentialsConfig{
		AutoSwap: true, MinSwapInterval: 10 * time.Minute,
		SwapAtPercent: 90, SwapMarginPercent: 20,
	}
	return srv, c, kc
}

// TestPollOneProfile_InactiveDeadOnRefreshRejectedThenRevives walks the
// spec's core poller scenario for an INACTIVE profile: a 401 on usage
// fetch triggers a refresh attempt, which is itself rejected (400) ->
// marked dead; a later pass whose refresh succeeds and whose fetch then
// succeeds revives it.
func TestPollOneProfile_InactiveDeadOnRefreshRejectedThenRevives(t *testing.T) {
	srv, c, kc := newUsageTestServer(t)
	kc.items[kcKey(claudeCredsService, testKeychainUser)] = credsBlob("accA", "refA")
	if _, err := c.Save("a"); err != nil {
		t.Fatal(err)
	}
	kc.items[kcKey(claudeCredsService, testKeychainUser)] = credsBlob("accB", "refB")
	if _, err := c.Save("b"); err != nil {
		t.Fatal(err)
	}
	// Back to a as active; b is now the inactive profile under test.
	if _, err := c.Swap("a", "test-setup"); err != nil {
		t.Fatal(err)
	}

	client := newFakeUsageClient()
	// credsBlob's expiresAt (epoch ms 1) is always "near expiry", so the
	// poller always tries a refresh first for an inactive profile.
	client.scriptRefresh("refB", fakeRefreshResult{status: 400})
	now := time.Now()
	srv.pollOneProfile(context.Background(), client, "b", false, now)
	if dead, reason := c.DeadReason("b"); !dead || reason == "" {
		t.Fatalf("expected b dead with a reason after refresh rejected, got dead=%v reason=%q", dead, reason)
	}
	if len(client.fetchCalls) != 0 {
		t.Errorf("a rejected refresh must short-circuit before any Fetch, got %v", client.fetchCalls)
	}

	// A later pass: refresh succeeds this time, then fetch succeeds.
	client.scriptRefresh("refB", fakeRefreshResult{access: "accB2", refresh: "refB2", expiresAt: now.Add(time.Hour), status: 200})
	client.scriptFetch("accB2", fakeUsageResult{snap: &usageSnapshot{Session: usageWindow{Percent: 5}, Weekly: usageWindow{Percent: 1}}, status: 200})
	srv.pollOneProfile(context.Background(), client, "b", false, now)
	if dead, reason := c.DeadReason("b"); dead {
		t.Errorf("expected b revived after a successful fetch, got dead=%v reason=%q", dead, reason)
	}
	if u := c.ProfileUsage("b"); u == nil || u.Session.Percent != 5 {
		t.Errorf("expected b's usage snapshot recorded, got %+v", u)
	}
}

// TestPollOneProfile_ActiveNeedsThreeStrikesBeforeDead verifies the
// ownership-rule asymmetry: the ACTIVE profile is never refreshed by tn
// (Claude Code owns that), and a single 401 must not immediately declare
// it dead — only usageDeadStrikeThreshold consecutive ones do.
func TestPollOneProfile_ActiveNeedsThreeStrikesBeforeDead(t *testing.T) {
	srv, c, kc := newUsageTestServer(t)
	kc.items[kcKey(claudeCredsService, testKeychainUser)] = credsBlob("accA", "refA")
	if _, err := c.Save("a"); err != nil {
		t.Fatal(err)
	}
	client := newFakeUsageClient()
	for i := 0; i < usageDeadStrikeThreshold; i++ {
		client.scriptFetch("accA", fakeUsageResult{status: 401})
	}
	now := time.Now()
	for i := 0; i < usageDeadStrikeThreshold-1; i++ {
		srv.pollOneProfile(context.Background(), client, "a", true, now)
		if dead, _ := c.DeadReason("a"); dead {
			t.Fatalf("must not be dead before %d strikes (tick %d)", usageDeadStrikeThreshold, i+1)
		}
		if len(client.refreshCalls) != 0 {
			t.Errorf("the ACTIVE profile must never be refreshed by tn, got refresh calls %v", client.refreshCalls)
		}
	}
	srv.pollOneProfile(context.Background(), client, "a", true, now)
	if dead, reason := c.DeadReason("a"); !dead || reason == "" {
		t.Errorf("expected active profile dead after %d consecutive 401s, got dead=%v reason=%q", usageDeadStrikeThreshold, dead, reason)
	}

	// A success anywhere in between clears the strike count — verify via a
	// fresh scenario rather than reusing the now-dead profile above.
	srv2, c2, kc2 := newUsageTestServer(t)
	kc2.items[kcKey(claudeCredsService, testKeychainUser)] = credsBlob("accX", "refX")
	if _, err := c2.Save("x"); err != nil {
		t.Fatal(err)
	}
	client2 := newFakeUsageClient()
	client2.scriptFetch("accX", fakeUsageResult{status: 401})
	client2.scriptFetch("accX", fakeUsageResult{status: 401})
	client2.scriptFetch("accX", fakeUsageResult{snap: &usageSnapshot{}, status: 200}) // strike reset
	client2.scriptFetch("accX", fakeUsageResult{status: 401})
	client2.scriptFetch("accX", fakeUsageResult{status: 401})
	for i := 0; i < 5; i++ {
		srv2.pollOneProfile(context.Background(), client2, "x", true, now)
	}
	if dead, _ := c2.DeadReason("x"); dead {
		t.Error("a successful fetch between 401s must reset the strike count, so 2+2 must not total 3")
	}
}

// TestPollOneProfile_RefreshWritesBackPreservingOtherFields verifies a
// refreshed inactive profile's Keychain item keeps every field the blob
// carried besides claudeAiOauth's own token trio.
func TestPollOneProfile_RefreshWritesBackPreservingOtherFields(t *testing.T) {
	srv, c, kc := newUsageTestServer(t)
	kc.items[kcKey(claudeCredsService, testKeychainUser)] = []byte(
		`{"claudeAiOauth":{"accessToken":"accB","refreshToken":"refB","expiresAt":1,"scopes":["user:inference"]},"subscriptionType":"pro"}`)
	if _, err := c.Save("b"); err != nil {
		t.Fatal(err)
	}
	kc.items[kcKey(claudeCredsService, testKeychainUser)] = credsBlob("accA", "refA")
	if _, err := c.Save("a"); err != nil {
		t.Fatal(err)
	} // a active now, b inactive with the blob above still in its own item

	client := newFakeUsageClient()
	client.scriptRefresh("refB", fakeRefreshResult{access: "accB2", refresh: "refB2", expiresAt: time.Now().Add(time.Hour), status: 200})
	client.scriptFetch("accB2", fakeUsageResult{snap: &usageSnapshot{Session: usageWindow{Percent: 1}}, status: 200})
	srv.pollOneProfile(context.Background(), client, "b", false, time.Now())

	got, err := kc.Get(credsProfileService, "b")
	if err != nil {
		t.Fatal(err)
	}
	var v map[string]any
	if err := json.Unmarshal(got, &v); err != nil {
		t.Fatal(err)
	}
	if v["subscriptionType"] != "pro" {
		t.Errorf("unrelated top-level field lost: %v", v)
	}
	oauth, _ := v["claudeAiOauth"].(map[string]any)
	if oauth["accessToken"] != "accB2" || oauth["refreshToken"] != "refB2" {
		t.Errorf("token pair not updated: %v", oauth)
	}
	scopes, _ := oauth["scopes"].([]any)
	if len(scopes) != 1 || scopes[0] != "user:inference" {
		t.Errorf("unrelated oauth field lost: %v", oauth)
	}
}

// --- proactive swap ---------------------------------------------------------

func TestMaybeProactiveSwap(t *testing.T) {
	srv, c, kc := newUsageTestServer(t)
	kc.items[kcKey(claudeCredsService, testKeychainUser)] = credsBlob("accA", "refA")
	if _, err := c.Save("a"); err != nil {
		t.Fatal(err)
	}
	kc.items[kcKey(claudeCredsService, testKeychainUser)] = credsBlob("accB", "refB")
	if _, err := c.Save("b"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Swap("a", "test-setup"); err != nil {
		t.Fatal(err)
	}
	c.lastSwap = nil
	credsSwapSettle = 0
	dialog := func(title, text string, buttons []string) (string, error) { return "OK", nil }
	capture := func(string) (string, error) { return "", nil }
	sendKeys := func(string, string) error { return nil }

	setUsage := func(label string, sessionPct, weeklyPct float64) {
		t.Helper()
		if err := c.recordUsageSuccess(label, &usageSnapshot{
			Session: usageWindow{Percent: sessionPct, ResetsAt: time.Now().Add(3 * time.Hour)},
			Weekly:  usageWindow{Percent: weeklyPct},
		}); err != nil {
			t.Fatal(err)
		}
	}

	// Below swapAtPercent: no swap.
	setUsage("a", 50, 10)
	setUsage("b", 10, 10)
	srv.maybeProactiveSwap(time.Now(), dialog, capture, sendKeys)
	if active, _ := c.ResolveActive(); active != "a" {
		t.Fatalf("active = %q, want a (below swapAtPercent must not swap)", active)
	}

	// Above threshold but candidate too close (margin 12 < 20): no swap.
	setUsage("a", 92, 10)
	setUsage("b", 80, 10)
	srv.maybeProactiveSwap(time.Now(), dialog, capture, sendKeys)
	if active, _ := c.ResolveActive(); active != "a" {
		t.Fatalf("active = %q, want a (margin too small must not swap)", active)
	}

	// Bring in a third, lower-usage profile "c" and mark it dead, plus a
	// fourth "d" that's genuinely the best eligible candidate — verifies
	// both the dead-skip and the sort-by-session-percent behaviour in one
	// swap: d (25%), not c (5%, dead) or b (40%), is picked.
	kc.items[kcKey(claudeCredsService, testKeychainUser)] = credsBlob("accC", "refC")
	if _, err := c.Save("c"); err != nil {
		t.Fatal(err)
	}
	kc.items[kcKey(claudeCredsService, testKeychainUser)] = credsBlob("accD", "refD")
	if _, err := c.Save("d"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Swap("a", "test-setup"); err != nil {
		t.Fatal(err)
	}
	c.lastSwap = nil
	setUsage("a", 95, 10)
	setUsage("b", 40, 10)
	setUsage("c", 5, 10)
	setUsage("d", 25, 10)
	if err := c.markDead("c", "test", time.Now()); err != nil {
		t.Fatal(err)
	}
	waitForNudge(t, srv, func() { srv.maybeProactiveSwap(time.Now(), dialog, capture, sendKeys) })
	if active, _ := c.ResolveActive(); active != "d" {
		t.Fatalf("active = %q, want d (lowest usage among non-dead candidates; c is dead and must be skipped despite lower usage)", active)
	}

	// A fresh trigger inside MinSwapInterval must not swap again even
	// though the condition still holds.
	setUsage("d", 95, 10)
	setUsage("b", 10, 10)
	srv.maybeProactiveSwap(time.Now(), dialog, capture, sendKeys)
	if active, _ := c.ResolveActive(); active != "d" {
		t.Fatalf("active = %q, want d (still within MinSwapInterval of the last swap)", active)
	}

	// Past the interval, but the best candidate is cooling down: must be
	// skipped in favor of the next-best eligible one. a is still cooling
	// down too (the swap above marked it limited until its own session
	// window resets, per the spec), so a fresh profile "e" stands in for
	// "the next-best eligible one".
	kc.items[kcKey(claudeCredsService, testKeychainUser)] = credsBlob("accE", "refE")
	if _, err := c.Save("e"); err != nil {
		t.Fatal(err)
	}
	// Saving "e" made it the active profile (it copies the live Claude
	// item) — swap back to d, which is what this scenario's active profile
	// should be.
	if _, err := c.Swap("d", "test-setup"); err != nil {
		t.Fatal(err)
	}
	c.lastSwap.At = time.Now().Add(-time.Hour)
	if err := c.MarkLimited("b", time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	setUsage("d", 95, 10)
	setUsage("e", 30, 10)
	waitForNudge(t, srv, func() { srv.maybeProactiveSwap(time.Now(), dialog, capture, sendKeys) })
	if active, _ := c.ResolveActive(); active != "e" {
		t.Fatalf("active = %q, want e (b is cooling down and must be skipped)", active)
	}
}

// --- reactive path: MarkLimited from the snapshot, never pane text --------

func TestAutoSwapCredentials_MarkLimitedUsesSnapshotResetsAtNotPaneText(t *testing.T) {
	srv, c, _ := newAutoSwapTestServer(t, true, "orchestrator-myapp")
	resetsAt := time.Now().Add(3 * time.Hour).Round(time.Second)
	if err := c.recordUsageSuccess("a", &usageSnapshot{Session: usageWindow{Percent: 100, ResetsAt: resetsAt}}); err != nil {
		t.Fatal(err)
	}
	capture := func(string) (string, error) { return "", nil }
	sendKeys := func(string, string) error { return nil }
	var res *credsAutoSwapResult
	waitForNudge(t, srv, func() {
		res = srv.autoSwapCredentials("orchestrator-myapp", "this text is deliberately not a parseable clock time", time.Now(), capture, sendKeys)
	})
	if res == nil || !res.swapped {
		t.Fatalf("expected a swap, got %+v", res)
	}
	_, views, err := c.List()
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, v := range views {
		if v.Label != "a" {
			continue
		}
		found = true
		if v.LimitedUntil == nil || !v.LimitedUntil.Round(time.Second).Equal(resetsAt) {
			t.Errorf("LimitedUntil = %v, want the snapshot's Session.ResetsAt %v (never derived from pane text)", v.LimitedUntil, resetsAt)
		}
	}
	if !found {
		t.Fatal("profile a not found in List()")
	}
}

// --- review fixes: concurrency, weekly-exhausted margin skip, transient refresh errors ---

// credsBlobExpiring is credsBlob with a controllable expiresAt, so a test
// can put a profile OUTSIDE the "near expiry" window (credsBlob's own
// fixed epoch-1 expiresAt is always near-expiry, which is exactly what
// most tests above want, but the post-Fetch-401 refresh-retry path below
// needs to be reached WITHOUT the pre-Fetch near-expiry refresh firing
// first).
func credsBlobExpiring(access, refresh string, expiresAt time.Time) []byte {
	b, _ := json.Marshal(map[string]any{"claudeAiOauth": map[string]any{
		"accessToken": access, "refreshToken": refresh, "expiresAt": expiresAt.UnixMilli(), "scopes": []string{"user:inference"},
	}})
	return b
}

// countingUsageClient just counts calls and always succeeds — used only to
// detect whether pollUsageOnce lets two concurrent passes both refresh the
// same token (they must not; see usagePollMu).
type countingUsageClient struct {
	mu           sync.Mutex
	refreshCalls int
	fetchCalls   int
}

func (c *countingUsageClient) Fetch(ctx context.Context, accessToken string) (*usageSnapshot, int, error) {
	c.mu.Lock()
	c.fetchCalls++
	c.mu.Unlock()
	return &usageSnapshot{Session: usageWindow{Percent: 1}}, 200, nil
}

func (c *countingUsageClient) Refresh(ctx context.Context, refreshToken string) (access, refresh string, expiresAt time.Time, status int, err error) {
	c.mu.Lock()
	c.refreshCalls++
	c.mu.Unlock()
	// Widen the race window: without usagePollMu serializing pollUsageOnce
	// end to end, two concurrent passes would both observe the profile's
	// ORIGINAL (near-expiry) blob and both land here before either write
	// completes.
	time.Sleep(15 * time.Millisecond)
	return "acc2", "ref2", time.Now().Add(time.Hour), http.StatusOK, nil
}

// TestPollUsageOnce_SerializedAgainstConcurrentPasses covers review fix #1:
// pollUsageOnce is reachable from the ticker goroutine AND POST /creds/poll
// at the same time; without usagePollMu, two concurrent passes over the
// same near-expiry inactive profile both call Refresh with the SAME
// refresh token, and whichever write lands last can leave the profile
// holding a dead token pair.
func TestPollUsageOnce_SerializedAgainstConcurrentPasses(t *testing.T) {
	srv, c, kc := newUsageTestServer(t)
	kc.items[kcKey(claudeCredsService, testKeychainUser)] = credsBlob("accA", "refA")
	if _, err := c.Save("a"); err != nil {
		t.Fatal(err)
	}
	// credsBlob's fixed expiresAt (epoch ms 1) is always near-expiry, so b
	// triggers the pre-Fetch refresh on every poll unless it's already
	// been refreshed this pass.
	kc.items[kcKey(claudeCredsService, testKeychainUser)] = credsBlob("accB", "refB")
	if _, err := c.Save("b"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Swap("a", "test-setup"); err != nil {
		t.Fatal(err)
	}

	client := &countingUsageClient{}
	dialog := func(string, string, []string) (string, error) { return "OK", nil }
	capture := func(string) (string, error) { return "", nil }
	sendKeys := func(string, string) error { return nil }

	var wg sync.WaitGroup
	wg.Add(2)
	for i := 0; i < 2; i++ {
		go func() {
			defer wg.Done()
			srv.pollUsageOnce(client, time.Now(), dialog, capture, sendKeys)
		}()
	}
	wg.Wait()

	if client.refreshCalls != 1 {
		t.Errorf("expected exactly one Refresh call across two concurrent poll passes, got %d", client.refreshCalls)
	}
}

// TestMaybeProactiveSwap_WeeklyExhaustedIgnoresSessionMargin covers review
// fix #2: a weekly-exhausted active profile must swap even when the
// SESSION percent gap to the target is below swapMarginPercent — the
// account is unusable regardless of its session percent once its weekly
// cap is hit, so the session margin comparison doesn't apply.
func TestMaybeProactiveSwap_WeeklyExhaustedIgnoresSessionMargin(t *testing.T) {
	srv, c, kc := newUsageTestServer(t)
	kc.items[kcKey(claudeCredsService, testKeychainUser)] = credsBlob("accA", "refA")
	if _, err := c.Save("a"); err != nil {
		t.Fatal(err)
	}
	kc.items[kcKey(claudeCredsService, testKeychainUser)] = credsBlob("accB", "refB")
	if _, err := c.Save("b"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Swap("a", "test-setup"); err != nil {
		t.Fatal(err)
	}
	c.lastSwap = nil
	credsSwapSettle = 0

	// Active: session 10%, weekly 100% (exhausted). Target: session 5%,
	// weekly 0% — only a 5-point session gap, well under the default 20
	// swapMarginPercent, yet the swap must still fire because the trigger
	// is the weekly exhaustion, not the session threshold.
	if err := c.recordUsageSuccess("a", &usageSnapshot{
		Session: usageWindow{Percent: 10, ResetsAt: time.Now().Add(3 * time.Hour)},
		Weekly:  usageWindow{Percent: 100},
	}); err != nil {
		t.Fatal(err)
	}
	if err := c.recordUsageSuccess("b", &usageSnapshot{
		Session: usageWindow{Percent: 5},
		Weekly:  usageWindow{Percent: 0},
	}); err != nil {
		t.Fatal(err)
	}

	dialog := func(string, string, []string) (string, error) { return "OK", nil }
	capture := func(string) (string, error) { return "", nil }
	sendKeys := func(string, string) error { return nil }
	waitForNudge(t, srv, func() { srv.maybeProactiveSwap(time.Now(), dialog, capture, sendKeys) })
	if active, _ := c.ResolveActive(); active != "b" {
		t.Fatalf("active = %q, want b (weekly-exhausted trigger must ignore the session-percent margin check)", active)
	}
}

// TestPollOneProfile_TransientRefreshFailuresDoNotMarkDead covers review
// fix #3: after a 401 on Fetch, only a DEFINITIVE refresh rejection
// (400/401) is evidence of a dead token — a transport error or a 429/5xx
// from Refresh is transient and must leave the profile alive with an
// error recorded, not dead.
func TestPollOneProfile_TransientRefreshFailuresDoNotMarkDead(t *testing.T) {
	srv, c, kc := newUsageTestServer(t)
	kc.items[kcKey(claudeCredsService, testKeychainUser)] = credsBlob("accA", "refA")
	if _, err := c.Save("a"); err != nil {
		t.Fatal(err)
	}
	// b's token is NOT near expiry, so the pre-Fetch refresh check is
	// skipped and the poll reaches Fetch directly — this test is about the
	// POST-401 refresh-retry path, not the near-expiry pre-check (already
	// covered by TestPollOneProfile_InactiveDeadOnRefreshRejectedThenRevives).
	kc.items[kcKey(claudeCredsService, testKeychainUser)] = credsBlobExpiring("accB", "refB", time.Now().Add(time.Hour))
	if _, err := c.Save("b"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Swap("a", "test-setup"); err != nil {
		t.Fatal(err)
	}
	now := time.Now()

	client := newFakeUsageClient()

	// Fetch -> 401, Refresh -> transport error: not dead, UsageError set.
	client.scriptFetch("accB", fakeUsageResult{status: 401})
	client.scriptRefresh("refB", fakeRefreshResult{err: fmt.Errorf("dial tcp: connection refused")})
	srv.pollOneProfile(context.Background(), client, "b", false, now)
	if dead, reason := c.DeadReason("b"); dead {
		t.Fatalf("a transport error on refresh must not mark dead, got dead=%v reason=%q", dead, reason)
	}
	if _, views, _ := c.List(); !credsHasUsageError(views, "b") {
		t.Error("expected UsageError set after a transient refresh failure")
	}

	// Fetch -> 401, Refresh -> 503: not dead (a 429/5xx is transient, not
	// a definitive rejection).
	client.scriptFetch("accB", fakeUsageResult{status: 401})
	client.scriptRefresh("refB", fakeRefreshResult{status: 503})
	srv.pollOneProfile(context.Background(), client, "b", false, now)
	if dead, reason := c.DeadReason("b"); dead {
		t.Errorf("a 503 from refresh must not mark dead, got dead=%v reason=%q", dead, reason)
	}

	// Fetch -> 401, Refresh -> 400: THIS is definitive — dead.
	client.scriptFetch("accB", fakeUsageResult{status: 401})
	client.scriptRefresh("refB", fakeRefreshResult{status: 400})
	srv.pollOneProfile(context.Background(), client, "b", false, now)
	if dead, reason := c.DeadReason("b"); !dead || reason == "" {
		t.Errorf("a 400 from refresh must mark dead, got dead=%v reason=%q", dead, reason)
	}
}

// credsHasUsageError reports whether views contains label with a non-empty
// UsageError.
func credsHasUsageError(views []credsProfileView, label string) bool {
	for _, v := range views {
		if v.Label == label {
			return v.UsageError != ""
		}
	}
	return false
}

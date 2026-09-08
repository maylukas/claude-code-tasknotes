package tn

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeKeychain is the map-backed keychainStore tests use. No test in this
// package may ever construct securityCLIKeychain.
type fakeKeychain struct {
	items map[string][]byte
	sets  []string // "service/account" in write order, for ordering assertions
}

func newFakeKeychain() *fakeKeychain { return &fakeKeychain{items: map[string][]byte{}} }

func kcKey(service, account string) string { return service + "\x00" + account }

func (f *fakeKeychain) Get(service, account string) ([]byte, error) {
	b, ok := f.items[kcKey(service, account)]
	if !ok {
		return nil, errKeychainNotFound
	}
	return append([]byte(nil), b...), nil
}

func (f *fakeKeychain) Set(service, account string, secret []byte) error {
	f.items[kcKey(service, account)] = append([]byte(nil), secret...)
	f.sets = append(f.sets, service+"/"+account)
	return nil
}

func (f *fakeKeychain) Delete(service, account string) error {
	if _, ok := f.items[kcKey(service, account)]; !ok {
		return errKeychainNotFound
	}
	delete(f.items, kcKey(service, account))
	return nil
}

func (f *fakeKeychain) FindAccount(service string) (string, error) {
	prefix := service + "\x00"
	for k := range f.items {
		if strings.HasPrefix(k, prefix) {
			return strings.TrimPrefix(k, prefix), nil
		}
	}
	return "", errKeychainNotFound
}

func credsBlob(access, refresh string) []byte {
	b, _ := json.Marshal(map[string]any{"claudeAiOauth": map[string]any{
		"accessToken": access, "refreshToken": refresh, "expiresAt": 1, "scopes": []string{"user:inference"},
	}})
	return b
}

const testKeychainUser = "someone"

func newTestCredsStore(t *testing.T) (*credsStore, *fakeKeychain) {
	t.Helper()
	kc := newFakeKeychain()
	c := newCredsStore(kc, filepath.Join(t.TempDir(), "creds.json"))
	c.emailLookup = func() string { return "a@example.com" }
	return c, kc
}

func TestCreds_SaveListRemove(t *testing.T) {
	c, kc := newTestCredsStore(t)
	if _, err := c.Save("a"); err == nil {
		t.Fatal("expected save to fail with no Claude item in the keychain")
	}
	kc.items[kcKey(claudeCredsService, testKeychainUser)] = credsBlob("accA", "refA")

	email, err := c.Save("a")
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	if email != "a@example.com" {
		t.Errorf("email = %q", email)
	}
	if got, _ := kc.Get(credsProfileService, "a"); !bytes.Equal(got, credsBlob("accA", "refA")) {
		t.Error("profile item does not hold the Claude blob")
	}
	if _, err := c.Save("Bad Label"); !errors.Is(err, errCredsInvalidLabel) {
		t.Errorf("expected invalid-label error, got %v", err)
	}

	active, views, err := c.List()
	if err != nil {
		t.Fatal(err)
	}
	if active != "a" || len(views) != 1 || !views[0].Active || !views[0].Stored || views[0].Email != "a@example.com" {
		t.Errorf("list = active %q, %+v", active, views)
	}

	if err := c.Remove("a"); !errors.Is(err, errCredsActiveProfile) {
		t.Errorf("removing the active profile must be refused, got %v", err)
	}
	if err := c.Remove("nope"); !errors.Is(err, errCredsProfileNotFound) {
		t.Errorf("expected not-found, got %v", err)
	}

	// A second account logs in (manual /login behind tn's back); save it,
	// then "a" is removable.
	kc.items[kcKey(claudeCredsService, testKeychainUser)] = credsBlob("accB", "refB")
	if _, err := c.Save("b"); err != nil {
		t.Fatal(err)
	}
	if err := c.Remove("a"); err != nil {
		t.Fatalf("remove a: %v", err)
	}
	if _, err := kc.Get(credsProfileService, "a"); !errors.Is(err, errKeychainNotFound) {
		t.Error("profile item for a should be deleted")
	}
	active, views, _ = c.List()
	if active != "b" || len(views) != 1 {
		t.Errorf("after remove: active %q, %d profiles", active, len(views))
	}
}

func TestCreds_ResolveActiveByRefreshToken(t *testing.T) {
	c, kc := newTestCredsStore(t)
	kc.items[kcKey(claudeCredsService, testKeychainUser)] = credsBlob("accA", "refA")
	if _, err := c.Save("a"); err != nil {
		t.Fatal(err)
	}
	kc.items[kcKey(claudeCredsService, testKeychainUser)] = credsBlob("accB", "refB")
	if _, err := c.Save("b"); err != nil {
		t.Fatal(err)
	}
	// The user runs /login back into account A (with a rotated access
	// token but the same refresh token). Metadata still says "b".
	kc.items[kcKey(claudeCredsService, testKeychainUser)] = credsBlob("accA2", "refA")
	got, err := c.ResolveActive()
	if err != nil {
		t.Fatal(err)
	}
	if got != "a" {
		t.Errorf("resolved active = %q, want a (matched by refresh token)", got)
	}
	// An account nobody saved: fall back to the recorded label.
	kc.items[kcKey(claudeCredsService, testKeychainUser)] = credsBlob("accX", "refX")
	if got, _ := c.ResolveActive(); got != "b" {
		t.Errorf("unknown blob should fall back to recorded active b, got %q", got)
	}
}

func TestCreds_SwapWritesBackRotatedTokenBeforeOverwriting(t *testing.T) {
	c, kc := newTestCredsStore(t)
	kc.items[kcKey(claudeCredsService, testKeychainUser)] = credsBlob("accA", "refA")
	if _, err := c.Save("a"); err != nil {
		t.Fatal(err)
	}
	kc.items[kcKey(claudeCredsService, testKeychainUser)] = credsBlob("accB", "refB")
	if _, err := c.Save("b"); err != nil {
		t.Fatal(err)
	}
	// Claude rotated b's tokens while it was live.
	rotated := credsBlob("accB-rotated", "refB-rotated")
	kc.items[kcKey(claudeCredsService, testKeychainUser)] = rotated
	// Metadata says b is active, and the rotated blob matches nothing by
	// token — the recorded label is what identifies it.
	kc.sets = nil

	from, err := c.Swap("a", "manual")
	if err != nil {
		t.Fatalf("swap: %v", err)
	}
	if from != "b" {
		t.Errorf("from = %q, want b", from)
	}
	if got, _ := kc.Get(credsProfileService, "b"); !bytes.Equal(got, rotated) {
		t.Error("profile b must hold the ROTATED blob that was live before the swap")
	}
	if got, _ := kc.Get(claudeCredsService, testKeychainUser); !bytes.Equal(got, credsBlob("accA", "refA")) {
		t.Error("Claude item must now hold profile a")
	}
	if len(kc.sets) != 2 || kc.sets[0] != credsProfileService+"/b" || kc.sets[1] != claudeCredsService+"/"+testKeychainUser {
		t.Errorf("write-back must happen BEFORE the Claude item is overwritten; writes = %v", kc.sets)
	}
	if got, _ := c.ResolveActive(); got != "a" {
		t.Errorf("active after swap = %q", got)
	}
	last := c.LastSwap()
	if last == nil || last.From != "b" || last.To != "a" || last.Trigger != "manual" {
		t.Errorf("lastSwap = %+v", last)
	}

	if _, err := c.Swap("a", "manual"); !errors.Is(err, errCredsSameProfile) {
		t.Errorf("swap to the active profile must be a no-op error, got %v", err)
	}
	if _, err := c.Swap("zzz", "manual"); !errors.Is(err, errCredsProfileNotFound) {
		t.Errorf("expected not-found, got %v", err)
	}
}

func TestParseResetTime(t *testing.T) {
	berlin, _ := time.LoadLocation("Europe/Berlin")
	now := time.Date(2026, 9, 2, 13, 0, 0, 0, berlin) // 13:00 Berlin
	cases := []struct {
		raw  string
		want time.Time
		ok   bool
	}{
		{"2:30pm (Europe/Berlin)", time.Date(2026, 9, 2, 14, 30, 0, 0, berlin), true},
		{"2:30pm", time.Date(2026, 9, 2, 14, 30, 0, 0, berlin), true},
		{"15:04", time.Date(2026, 9, 2, 15, 4, 0, 0, berlin), true},
		{"3pm", time.Date(2026, 9, 2, 15, 0, 0, 0, berlin), true},
		// Already passed today → tomorrow.
		{"11:00am (Europe/Berlin)", time.Date(2026, 9, 3, 11, 0, 0, 0, berlin), true},
		{"", time.Time{}, false},
		{"soon", time.Time{}, false},
	}
	for _, tc := range cases {
		got, ok := parseResetTime(tc.raw, now)
		if ok != tc.ok {
			t.Errorf("%q: ok = %v, want %v", tc.raw, ok, tc.ok)
			continue
		}
		if ok && !got.Equal(tc.want) {
			t.Errorf("%q: got %v, want %v", tc.raw, got, tc.want)
		}
	}
}

func TestCreds_MarkLimitedAndEligibleOrdering(t *testing.T) {
	c, kc := newTestCredsStore(t)
	base := time.Date(2026, 9, 2, 13, 0, 0, 0, time.UTC)
	c.now = func() time.Time { return base }
	kc.items[kcKey(claudeCredsService, testKeychainUser)] = credsBlob("accA", "refA")
	for _, l := range []string{"a", "b", "c"} {
		kc.items[kcKey(claudeCredsService, testKeychainUser)] = credsBlob("acc"+l, "ref"+l)
		if _, err := c.Save(l); err != nil {
			t.Fatal(err)
		}
	}
	// c is live. Give b an older lastUsedAt than a via a swap sequence:
	// swap to b at t+1, swap to a at t+2, back to c at t+3.
	for i, l := range []string{"b", "a", "c"} {
		c.now = func() time.Time { return base.Add(time.Duration(i+1) * time.Minute) }
		if _, err := c.Swap(l, "manual"); err != nil {
			t.Fatalf("swap %s: %v", l, err)
		}
	}
	c.now = func() time.Time { return base.Add(10 * time.Minute) }
	active, eligible, err := c.Eligible()
	if err != nil {
		t.Fatal(err)
	}
	if active != "c" {
		t.Errorf("active = %q", active)
	}
	if strings.Join(eligible, ",") != "b,a" {
		t.Errorf("eligible = %v, want oldest-used first: [b a]", eligible)
	}

	// b hits a limit that resets in an hour: ineligible until then.
	until, err := c.MarkLimited("b", "14:30")
	if err != nil {
		t.Fatal(err)
	}
	if !until.After(c.now()) {
		t.Errorf("limitedUntil %v should be in the future", until)
	}
	if _, eligible, _ = c.Eligible(); strings.Join(eligible, ",") != "a" {
		t.Errorf("eligible while b cools down = %v, want [a]", eligible)
	}
	// Unparseable reset text → 5h fallback.
	until, _ = c.MarkLimited("a", "later")
	if want := c.now().Add(credsLimitFallbackCooldown); !until.Equal(want) {
		t.Errorf("fallback cooldown = %v, want %v", until, want)
	}
	if _, eligible, _ = c.Eligible(); len(eligible) != 0 {
		t.Errorf("expected nothing eligible, got %v", eligible)
	}
	// Cooldown expires.
	c.now = func() time.Time { return base.Add(6 * time.Hour) }
	if _, eligible, _ = c.Eligible(); strings.Join(eligible, ",") != "b,a" {
		t.Errorf("after cooldown eligible = %v", eligible)
	}
	// Unknown label: ignored, no error.
	if _, err := c.MarkLimited("ghost", "3pm"); err != nil {
		t.Errorf("unknown label should be ignored, got %v", err)
	}
}

// --- daemon integration -------------------------------------------------

type keyLog struct {
	keys []string // "session: keys"
}

func (k *keyLog) send(session, keys string) error {
	k.keys = append(k.keys, session+": "+keys)
	return nil
}

func newAutoSwapTestServer(t *testing.T, autoSwap bool, agents ...string) (*Server, *credsStore, *fakeKeychain) {
	t.Helper()
	credsNudgeDelay = 0
	srv := newRateLimitTestServer(t, agents[0])
	for _, a := range agents[1:] {
		srv.mu.Lock()
		srv.state.Agents[a] = &Agent{Name: a, Project: "myapp", LastSeenAt: time.Now()}
		srv.mu.Unlock()
	}
	c, kc := newTestCredsStore(t)
	kc.items[kcKey(claudeCredsService, testKeychainUser)] = credsBlob("accA", "refA")
	if _, err := c.Save("a"); err != nil {
		t.Fatal(err)
	}
	kc.items[kcKey(claudeCredsService, testKeychainUser)] = credsBlob("accB", "refB")
	if _, err := c.Save("b"); err != nil {
		t.Fatal(err)
	}
	// Back to a as the live account.
	if _, err := c.Swap("a", "test-setup"); err != nil {
		t.Fatal(err)
	}
	c.lastSwap = nil
	srv.creds = c
	srv.config.Credentials = CredentialsConfig{AutoSwap: autoSwap, MinSwapInterval: 10 * time.Minute}
	return srv, c, kc
}

func TestAutoSwap_OffKeepsPlainDialog(t *testing.T) {
	srv, _, kc := newAutoSwapTestServer(t, false, "orchestrator-myapp")
	capture := func(session string) (string, error) { return rateLimitPaneContinuing, nil }
	var titles []string
	dialog := func(title, text string, buttons []string) (string, error) {
		titles = append(titles, title)
		if strings.Contains(text, "Auto-swap") {
			t.Errorf("auto-swap off must not mention auto-swap: %q", text)
		}
		return "OK", nil
	}
	kl := &keyLog{}
	srv.checkStuckSessionsOnce(capture, dialog, kl.send)
	if len(titles) != 1 || titles[0] != "Usage limit — orchestrator-myapp" {
		t.Errorf("titles = %v", titles)
	}
	if len(kl.keys) != 0 {
		t.Errorf("no keys must be sent when auto-swap is off, got %v", kl.keys)
	}
	if got, _ := kc.Get(claudeCredsService, testKeychainUser); !bytes.Equal(got, credsBlob("accA", "refA")) {
		t.Error("Claude item must be untouched")
	}
}

func TestAutoSwap_SwapsNudgesAllParkedPanesAndRespectsMinInterval(t *testing.T) {
	srv, c, kc := newAutoSwapTestServer(t, true, "orchestrator-myapp-g1", "orchestrator-myapp-g2")
	capture := func(session string) (string, error) { return rateLimitPaneContinuing, nil }
	var titles, texts []string
	dialog := func(title, text string, buttons []string) (string, error) {
		titles = append(titles, title)
		texts = append(texts, text)
		return "OK", nil
	}
	kl := &keyLog{}

	srv.checkStuckSessionsOnce(capture, dialog, kl.send)

	if got, _ := kc.Get(claudeCredsService, testKeychainUser); !bytes.Equal(got, credsBlob("accB", "refB")) {
		t.Fatal("Claude item should now hold profile b")
	}
	if active, _ := c.ResolveActive(); active != "b" {
		t.Errorf("active = %q", active)
	}
	// Only ONE swap for two agents parked on the same account: whichever
	// agent's tick runs first (checkStuckSessionsOnce iterates the agent
	// snapshot in map order — deliberately not asserted) triggers it; the
	// other's episode starts inside MinSwapInterval and is skipped.
	if len(titles) != 2 {
		t.Fatalf("expected one dialog per agent, got %v", titles)
	}
	if titles[0] != "Usage limit — credentials swapped" {
		t.Errorf("first dialog title = %q", titles[0])
	}
	first, second := "orchestrator-myapp-g1", "orchestrator-myapp-g2"
	if strings.HasPrefix(texts[0], second) {
		first, second = second, first
	}
	for _, want := range []string{first + " hit the claude.ai session limit", "resets 2:30pm (Europe/Berlin)", "a → b", "now uses b"} {
		if !strings.Contains(texts[0], want) {
			t.Errorf("swapped dialog text %q missing %q", texts[0], want)
		}
	}
	if titles[1] != "Usage limit — "+second || !strings.Contains(texts[1], "Auto-swap: last swap (a → b)") {
		t.Errorf("second dialog should explain the interval skip: %q / %q", titles[1], texts[1])
	}
	// Only the first agent was tracked as rate-limited at swap time (the
	// other's tick comes after), so exactly its pane gets Escape then the
	// continuation text.
	session := "tn-myapp-" + strings.TrimPrefix(first, "orchestrator-myapp-")
	want := []string{
		session + ": Escape",
		session + ": " + credsNudgeText("b"),
	}
	if strings.Join(kl.keys, "|") != strings.Join(want, "|") {
		t.Errorf("keys sent = %v, want %v", kl.keys, want)
	}
	// Profile a is cooling down until the parsed reset time.
	_, views, _ := c.List()
	for _, v := range views {
		if v.Label == "a" && (v.LimitedUntil == nil || !v.LimitedUntil.After(time.Now())) {
			t.Errorf("profile a should be marked limited, got %+v", v)
		}
	}

	// A later episode after the interval, with everything cooling down
	// except nothing → "no eligible" note, no swap, no keys.
	c.lastSwap.At = time.Now().Add(-time.Hour)
	if _, err := c.MarkLimited("a", "later"); err != nil {
		t.Fatal(err)
	}
	kl.keys = nil
	titles, texts = nil, nil
	capture = func(session string) (string, error) { return "⏺ working\n", nil }
	srv.checkStuckSessionsOnce(capture, dialog, kl.send) // episodes end
	capture = func(session string) (string, error) {
		return "You've hit your session limit · resets 4:00pm (Europe/Berlin)", nil
	}
	srv.checkStuckSessionsOnce(capture, dialog, kl.send)
	if len(titles) != 2 {
		t.Fatalf("expected two dialogs for the new episode, got %v", titles)
	}
	for i := range titles {
		if !strings.Contains(texts[i], "Auto-swap: no eligible credential profile") {
			t.Errorf("dialog %d should carry the no-eligible note: %q", i, texts[i])
		}
	}
	if len(kl.keys) != 0 {
		t.Errorf("no nudge without a swap, got %v", kl.keys)
	}
	if got, _ := kc.Get(claudeCredsService, testKeychainUser); !bytes.Equal(got, credsBlob("accB", "refB")) {
		t.Error("Claude item must still hold b")
	}
}

func TestAutoSwap_NoStoreIsNoOp(t *testing.T) {
	srv := newRateLimitTestServer(t, "orchestrator-myapp")
	srv.config.Credentials.AutoSwap = true // but srv.creds == nil
	capture := func(session string) (string, error) { return rateLimitPaneContinuing, nil }
	var texts []string
	dialog := func(title, text string, buttons []string) (string, error) {
		texts = append(texts, text)
		return "OK", nil
	}
	kl := &keyLog{}
	srv.checkStuckSessionsOnce(capture, dialog, kl.send)
	if len(texts) != 1 || strings.Contains(texts[0], "Auto-swap") || len(kl.keys) != 0 {
		t.Errorf("nil store must behave exactly like auto-swap off: %v / %v", texts, kl.keys)
	}
}

func TestCredsEndpoints(t *testing.T) {
	srv, c, kc := newAutoSwapTestServer(t, false, "orchestrator-myapp")
	kl := &keyLog{}
	srv.stuckSendKeys = kl.send
	mux := newMux(srv)

	// Park the agent so a manual swap has something to nudge.
	dialog := func(title, text string, buttons []string) (string, error) { return "OK", nil }
	srv.checkStuckSessionsOnce(func(string) (string, error) { return rateLimitPaneContinuing, nil }, dialog, kl.send)
	kl.keys = nil

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/creds", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /creds = %d: %s", rec.Code, rec.Body)
	}
	body := rec.Body.String()
	for _, secret := range []string{"accA", "refA", "accB", "refB"} {
		if strings.Contains(body, secret) {
			t.Fatalf("GET /creds leaked a token: %s", body)
		}
	}
	var list struct {
		AutoSwap bool               `json:"autoSwap"`
		Active   string             `json:"active"`
		Profiles []credsProfileView `json:"profiles"`
		LastSwap *credsSwapRecord   `json:"lastSwap"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	if list.AutoSwap || list.Active != "a" || len(list.Profiles) != 2 || list.LastSwap != nil {
		t.Errorf("GET /creds = %+v", list)
	}

	post := func(body string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/creds/swap", strings.NewReader(body)))
		return rec
	}
	if rec := post(`{"label":"zzz"}`); rec.Code != http.StatusNotFound {
		t.Errorf("unknown label = %d", rec.Code)
	}
	if rec := post(`{"label":"a"}`); rec.Code != http.StatusConflict {
		t.Errorf("active label = %d", rec.Code)
	}
	if rec := post(`{}`); rec.Code != http.StatusBadRequest {
		t.Errorf("missing label = %d", rec.Code)
	}
	rec = post(`{"label":"b"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("swap = %d: %s", rec.Code, rec.Body)
	}
	var res struct {
		From   string   `json:"from"`
		To     string   `json:"to"`
		Nudged []string `json:"nudged"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &res)
	if res.From != "a" || res.To != "b" || strings.Join(res.Nudged, ",") != "orchestrator-myapp" {
		t.Errorf("swap response = %+v", res)
	}
	if got, _ := kc.Get(claudeCredsService, testKeychainUser); !bytes.Equal(got, credsBlob("accB", "refB")) {
		t.Error("Claude item should hold b after the manual swap")
	}
	if len(kl.keys) != 2 || kl.keys[0] != "tn-myapp: Escape" {
		t.Errorf("manual swap must nudge the parked pane, keys = %v", kl.keys)
	}
	if last := c.LastSwap(); last == nil || last.Trigger != "manual" {
		t.Errorf("lastSwap = %+v", last)
	}

	// No store wired: 503, never a Keychain touch.
	srv.creds = nil
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/creds", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("nil store GET /creds = %d", rec.Code)
	}
	if rec := post(`{"label":"a"}`); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("nil store POST /creds/swap = %d", rec.Code)
	}
}

func TestResolveServeConfig_CredentialsDefaults(t *testing.T) {
	// No serve.json in a scratch HOME → defaults.
	t.Setenv("HOME", t.TempDir())
	cfg := resolveServeConfig(0)
	if cfg.Credentials.AutoSwap || cfg.Credentials.MinSwapInterval != credsDefaultMinSwapInterval {
		t.Errorf("defaults = %+v", cfg.Credentials)
	}
}

func TestStatus_CredentialsBlock(t *testing.T) {
	srv, c, kc := newAutoSwapTestServer(t, true, "orchestrator-myapp")
	mux := newMux(srv)
	get := func() map[string]any {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/status", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("GET /status = %d: %s", rec.Code, rec.Body)
		}
		for _, secret := range []string{"accA", "refA", "accB", "refB"} {
			if strings.Contains(rec.Body.String(), secret) {
				t.Fatalf("/status leaked a token")
			}
		}
		var m map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
			t.Fatal(err)
		}
		return m
	}
	creds, ok := get()["credentials"].(map[string]any)
	if !ok {
		t.Fatal("expected /status.credentials with a store wired")
	}
	if creds["autoSwap"] != true || creds["active"] != "a" || len(creds["profiles"].([]any)) != 2 {
		t.Errorf("credentials = %v", creds)
	}
	// A swap invalidates the cache: the next /status reflects it.
	if _, err := c.Swap("b", "manual"); err != nil {
		t.Fatal(err)
	}
	creds = get()["credentials"].(map[string]any)
	if creds["active"] != "b" || creds["lastSwap"] == nil {
		t.Errorf("after swap credentials = %v", creds)
	}
	_ = kc
	// No store: block omitted.
	srv.creds = nil
	if _, present := get()["credentials"]; present {
		t.Error("credentials must be omitted when no store is wired")
	}
}

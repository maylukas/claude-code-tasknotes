package tn

// Usage-API-driven credential auto-swap. See docs/design/SPEC-usage-swap.md
// for the full design and the live incident (2026-09-08/09) that motivated
// it: creds.go used to infer account state entirely from pane text, which
// produced three real failures in one incident — an unprobed dead profile
// (revoked refresh token) taking down every Claude session on the machine,
// a phantom rate-limit episode from an un-normalized reset string
// re-wrapping mid-episode, and a post-swap nudge racing Claude's own ~30s
// Keychain read cache. The fix: ask claude.ai's own usage endpoint instead
// of reading it off a terminal screen.
//
// Ownership rule (non-negotiable, see the spec): the ACTIVE profile is the
// live `Claude Code-credentials` item — tn only ever READS it, never
// refreshes its token (Claude Code owns that refresh; two refreshers racing
// on one refresh token leaves one of them dead). INACTIVE profiles
// (`tn-credential-profiles` items) are tn's alone to refresh.

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"
)

// usageAPIHost/usageAPIPath is claude.ai's own usage endpoint, verified
// live 2026-09-09 against the installed Claude Code 2.1.266 binary (see the
// spec). tokenRefreshHost is a package-level var (not a const) so a test
// can point it at an httptest server and so it can be changed without a
// code hunt if Anthropic moves the host again (console.anthropic.com ->
// platform.claude.com already happened once).
const (
	usageAPIHost  = "https://api.anthropic.com"
	usageAPIPath  = "/api/oauth/usage"
	oauthClientID = "9d1c250a-e61b-44d9-88ed-5944d1962f5e"
)

var tokenRefreshHost = "https://platform.claude.com"

// usageWindow is one usage window (session/weekly) from the API.
type usageWindow struct {
	Percent  float64   `json:"percent"`
	ResetsAt time.Time `json:"resetsAt"` // zero when the API reported null
}

// usageSnapshot is one profile's last-polled usage state.
type usageSnapshot struct {
	Session      usageWindow  `json:"session"`
	Weekly       usageWindow  `json:"weekly"`
	WeeklyScoped *usageWindow `json:"weeklyScoped,omitempty"`
	FetchedAt    time.Time    `json:"fetchedAt"`
}

// usageClient is the injectable surface for the two claude.ai OAuth calls
// this file needs. Fetch returns (snapshot, HTTP status, transport error) —
// a non-2xx status is a normal, expected outcome (401 = bad token, 429/5xx
// = transient), not itself an error; err is reserved for a request that
// never got a response at all.
type usageClient interface {
	Fetch(ctx context.Context, accessToken string) (*usageSnapshot, int, error)
	Refresh(ctx context.Context, refreshToken string) (access, refresh string, expiresAt time.Time, status int, err error)
}

// httpUsageClient is the real implementation. Only cmdServe constructs one;
// tests use a fakeUsageClient so no test ever calls api.anthropic.com or
// platform.claude.com.
type httpUsageClient struct{ http *http.Client }

func newHTTPUsageClient() *httpUsageClient {
	return &httpUsageClient{http: &http.Client{Timeout: 10 * time.Second}}
}

// usageAPIResponse is the subset of GET /api/oauth/usage's JSON this cares
// about. Parsed defensively throughout: five_hour/seven_day may be null,
// limits may be absent, any resets_at may be null — see parseUsageSnapshot.
type usageAPIResponse struct {
	FiveHour *usageAPIWindow `json:"five_hour"`
	SevenDay *usageAPIWindow `json:"seven_day"`
	Limits   []usageAPILimit `json:"limits"`
}

type usageAPIWindow struct {
	Utilization float64 `json:"utilization"`
	ResetsAt    *string `json:"resets_at"`
}

type usageAPILimit struct {
	Kind     string  `json:"kind"`
	Group    string  `json:"group"`
	Percent  float64 `json:"percent"`
	Severity string  `json:"severity"`
	ResetsAt *string `json:"resets_at"`
	IsActive bool    `json:"is_active"`
}

func (c *httpUsageClient) Fetch(ctx context.Context, accessToken string) (*usageSnapshot, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, usageAPIHost+usageAPIPath, nil)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("anthropic-beta", "oauth-2025-04-20")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, resp.StatusCode, nil
	}
	var raw usageAPIResponse
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return nil, resp.StatusCode, err
	}
	return parseUsageSnapshot(raw, time.Now()), resp.StatusCode, nil
}

// parseUsageSnapshot prefers limits[] (kind "session"/"weekly_all"/
// "weekly_scoped"), falling back to five_hour/seven_day when limits is
// absent or missing a kind. percent is 0-100+ (can exceed 100); a null
// resets_at yields the zero time.
func parseUsageSnapshot(raw usageAPIResponse, now time.Time) *usageSnapshot {
	snap := &usageSnapshot{FetchedAt: now}
	haveSession, haveWeekly := false, false
	for _, l := range raw.Limits {
		w := usageWindow{Percent: l.Percent, ResetsAt: parseUsageResetsAt(l.ResetsAt)}
		switch l.Kind {
		case "session":
			snap.Session = w
			haveSession = true
		case "weekly_all":
			snap.Weekly = w
			haveWeekly = true
		case "weekly_scoped":
			ws := w
			snap.WeeklyScoped = &ws
		}
	}
	if !haveSession && raw.FiveHour != nil {
		snap.Session = usageWindow{Percent: raw.FiveHour.Utilization, ResetsAt: parseUsageResetsAt(raw.FiveHour.ResetsAt)}
	}
	if !haveWeekly && raw.SevenDay != nil {
		snap.Weekly = usageWindow{Percent: raw.SevenDay.Utilization, ResetsAt: parseUsageResetsAt(raw.SevenDay.ResetsAt)}
	}
	return snap
}

func parseUsageResetsAt(s *string) time.Time {
	if s == nil || *s == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339Nano, *s)
	if err != nil {
		return time.Time{}
	}
	return t
}

// Refresh rotates a refresh token via platform.claude.com. Only ever called
// for a profile tn exclusively owns (an inactive one) — see this file's own
// doc comment.
func (c *httpUsageClient) Refresh(ctx context.Context, refreshToken string) (access, refresh string, expiresAt time.Time, status int, err error) {
	body, _ := json.Marshal(map[string]string{
		"grant_type":    "refresh_token",
		"refresh_token": refreshToken,
		"client_id":     oauthClientID,
		"scope":         "user:inference user:profile",
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenRefreshHost+"/v1/oauth/token", strings.NewReader(string(body)))
	if err != nil {
		return "", "", time.Time{}, 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return "", "", time.Time{}, 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", "", time.Time{}, resp.StatusCode, nil
	}
	var out struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int64  `json:"expires_in"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", "", time.Time{}, resp.StatusCode, err
	}
	exp := time.Now().Add(time.Duration(out.ExpiresIn) * time.Second)
	return out.AccessToken, out.RefreshToken, exp, resp.StatusCode, nil
}

// --- Poller --------------------------------------------------------------

// usagePollInitialDelay is the eager first-run delay after daemon startup,
// same convention as the due-task scanner / MR watcher.
const usagePollInitialDelay = 5 * time.Second

// usageDeadStrikeThreshold is how many consecutive 401s the ACTIVE
// profile's usage fetch must show before it's marked dead. Claude Code
// itself may be mid-refresh when tn's own poll lands (tn never refreshes
// the active profile — see ownership rule), so a single 401 must not
// immediately declare the account dead; three consecutive polls (at the
// default 60s interval, ~2 minutes) is a real, persistent failure.
const usageDeadStrikeThreshold = 3

// usageFetchTimeout bounds one poll pass's total HTTP work (a handful of
// profiles, one refresh-then-fetch each in the worst case).
const usageFetchTimeout = 20 * time.Second

// startUsagePoller launches the background goroutine that keeps every
// saved profile's usage snapshot fresh (active included) and, after each
// pass, checks whether the active profile should be proactively swapped
// away from before it ever hits a hard limit. No-op when s.creds is nil
// (tests, or a daemon that failed to wire a credential store) — only
// cmdServe calls this with a real httpUsageClient.
func (s *Server) startUsagePoller(client usageClient) {
	if s.creds == nil {
		return
	}
	// dialogFuncForEnv's real dialog (dialogReal) can block THIS goroutine
	// for up to 120s if a proactive swap fires and shows its summary
	// dialog — same shape as the stuck detector's own dialog call, and
	// accepted for the same reason: it only delays the NEXT tick, never
	// the caller (POST /creds/poll runs its own pass, not this loop).
	dialog := dialogFuncForEnv()
	go func() {
		time.Sleep(usagePollInitialDelay)
		s.pollUsageOnce(client, time.Now(), dialog, capturePaneReal, sendKeysReal)

		interval := s.config.Credentials.UsagePollInterval
		if interval <= 0 {
			interval = credsDefaultUsagePollInterval
		}
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for range ticker.C {
			s.pollUsageOnce(client, time.Now(), dialog, capturePaneReal, sendKeysReal)
		}
	}()
}

// pollUsageOnce is one poll pass over every saved profile (active
// included), factored out of the ticker loop so it's directly testable
// and reachable synchronously from POST /creds/poll. Logs one line per
// state CHANGE only (dead<->alive, handled inside markCredsDead/
// recordUsageSuccess), never per tick.
//
// Serialized on usagePollMu for its ENTIRE body: this is reachable both
// from the ticker goroutine (startUsagePoller) and synchronously from
// POST /creds/poll (handleCredsPoll) at the same time. Without this, two
// concurrent passes over the same inactive profile both read its blob and
// both call Refresh with the SAME refresh token — the second rotation
// invalidates the first's result, and whichever writeProfileBlob lands
// last can leave the profile holding a dead pair. That is exactly the
// race the ownership rule (see this file's header comment) exists to
// prevent for the ACTIVE profile; inactive profiles need the same
// protection against tn racing ITSELF. A plain Lock (not TryLock): a
// caller of POST /creds/poll should get data at least as fresh as
// whatever pass was already in flight, not a "busy" error.
func (s *Server) pollUsageOnce(client usageClient, now time.Time, dialog dialogFunc, capture captureFunc, sendKeys sendKeysFunc) {
	if s.creds == nil {
		return
	}
	s.usagePollMu.Lock()
	defer s.usagePollMu.Unlock()
	active, views, err := s.creds.List()
	if err != nil {
		log.Printf("serve: usage poll: could not read credential profiles: %v", err)
		s.setUsagePollResult(now, err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), usageFetchTimeout)
	defer cancel()
	for _, v := range views {
		if !v.Stored {
			continue
		}
		s.pollOneProfile(ctx, client, v.Label, v.Label == active, now)
	}
	s.creds.invalidateCache()
	s.setUsagePollResult(now, "")
	s.maybeProactiveSwap(now, dialog, capture, sendKeys)
}

func (s *Server) setUsagePollResult(at time.Time, errMsg string) {
	s.mu.Lock()
	s.usageLastPollAt = at
	s.usageLastPollErr = errMsg
	s.mu.Unlock()
}

// usageLastPoll returns the last poll pass's timestamp and error (empty on
// success), for /creds and /status.credentials.
func (s *Server) usageLastPoll() (time.Time, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.usageLastPollAt, s.usageLastPollErr
}

// pollOneProfile implements the spec's per-profile poll steps: resolve the
// blob (live Claude item when active, the profile's own item otherwise),
// refresh an inactive profile first when its token is near expiry, fetch
// usage, and update Dead/Usage/UsageError accordingly.
func (s *Server) pollOneProfile(ctx context.Context, client usageClient, label string, active bool, now time.Time) {
	blob, err := s.creds.blobFor(label, active)
	if err != nil {
		// Missing Keychain item — the view already reports stored:false;
		// nothing to poll.
		return
	}
	tok := parseCredsBlobTokens(blob)
	if tok.Access == "" {
		return
	}

	if !active && !tok.ExpiresAt.IsZero() && tok.ExpiresAt.Before(now.Add(5*time.Minute)) {
		newAccess, newRefresh, newExpiresAt, status, rerr := client.Refresh(ctx, tok.Refresh)
		switch {
		case rerr != nil:
			if err := s.creds.recordUsageError(label, "refresh failed: "+rerr.Error()); err != nil {
				log.Printf("serve: usage poll: record error for %q: %v", label, err)
			}
			return
		case status == http.StatusOK && newAccess != "":
			if updated, uerr := updateCredsBlobTokens(blob, newAccess, newRefresh, newExpiresAt); uerr == nil {
				if werr := s.creds.writeProfileBlob(label, updated); werr == nil {
					blob = updated
					tok = parseCredsBlobTokens(updated)
				} else {
					log.Printf("serve: usage poll: write refreshed blob for %q: %v", label, werr)
				}
			}
		case status == http.StatusBadRequest || status == http.StatusUnauthorized:
			s.markCredsDead(label, fmt.Sprintf("token refresh rejected (%d)", status), now)
			return
		default:
			if err := s.creds.recordUsageError(label, fmt.Sprintf("refresh: HTTP %d", status)); err != nil {
				log.Printf("serve: usage poll: record error for %q: %v", label, err)
			}
			return
		}
	}

	snap, status, ferr := client.Fetch(ctx, tok.Access)
	switch {
	case ferr != nil:
		if err := s.creds.recordUsageError(label, "usage fetch failed: "+ferr.Error()); err != nil {
			log.Printf("serve: usage poll: record error for %q: %v", label, err)
		}
	case status == http.StatusOK:
		s.usageStrikesClear(label)
		if err := s.creds.recordUsageSuccess(label, snap); err != nil {
			log.Printf("serve: usage poll: record snapshot for %q: %v", label, err)
		}
		if snap.Session.Percent >= 100 {
			if err := s.creds.MarkLimited(label, snap.Session.ResetsAt); err != nil {
				log.Printf("serve: usage poll: mark %q limited: %v", label, err)
			}
		}
	case status == http.StatusUnauthorized:
		if !active {
			// Inactive profile: refresh once, fetch once more before
			// giving up. Only a DEFINITIVE rejection is evidence of a dead
			// token — a transport error or a 429/5xx from either call is
			// transient (rate limiting, a blip on Anthropic's side) and
			// must not be treated the same as a confirmed-bad credential;
			// the profile stays alive with its last good snapshot and just
			// gets a recorded error, to be retried next tick.
			newAccess, _, _, rstatus, rerr := client.Refresh(ctx, tok.Refresh)
			switch {
			case rerr != nil:
				if err := s.creds.recordUsageError(label, "refresh failed: "+rerr.Error()); err != nil {
					log.Printf("serve: usage poll: record error for %q: %v", label, err)
				}
				return
			case rstatus == http.StatusBadRequest || rstatus == http.StatusUnauthorized:
				s.markCredsDead(label, fmt.Sprintf("401 on usage fetch; refresh rejected (%d)", rstatus), now)
				return
			case rstatus != http.StatusOK || newAccess == "":
				// Refresh itself neither succeeded nor was definitively
				// rejected (429/5xx, or a malformed 200) — transient.
				if err := s.creds.recordUsageError(label, fmt.Sprintf("401 on usage fetch; refresh: HTTP %d", rstatus)); err != nil {
					log.Printf("serve: usage poll: record error for %q: %v", label, err)
				}
				return
			}
			snap2, status2, ferr2 := client.Fetch(ctx, newAccess)
			switch {
			case ferr2 != nil:
				if err := s.creds.recordUsageError(label, "usage fetch failed after refresh: "+ferr2.Error()); err != nil {
					log.Printf("serve: usage poll: record error for %q: %v", label, err)
				}
				return
			case status2 == http.StatusOK:
				s.usageStrikesClear(label)
				if err := s.creds.recordUsageSuccess(label, snap2); err != nil {
					log.Printf("serve: usage poll: record snapshot for %q: %v", label, err)
				}
				return
			case status2 == http.StatusUnauthorized:
				// A FRESH token was just rejected: the account itself
				// can't authenticate, not a stale-token fluke.
				s.markCredsDead(label, "401 on usage fetch even with a freshly refreshed token", now)
				return
			default:
				if err := s.creds.recordUsageError(label, fmt.Sprintf("usage fetch after refresh: HTTP %d", status2)); err != nil {
					log.Printf("serve: usage poll: record error for %q: %v", label, err)
				}
				return
			}
		}
		// Active profile: Claude may be mid-refresh right now — see the
		// ownership rule. Requires usageDeadStrikeThreshold consecutive
		// polls before declaring it dead.
		if err := s.creds.recordUsageError(label, "401 on usage fetch"); err != nil {
			log.Printf("serve: usage poll: record error for %q: %v", label, err)
		}
		if s.usageStrikesIncrement(label) >= usageDeadStrikeThreshold {
			s.markCredsDead(label, "401 on usage fetch (3 consecutive polls)", now)
		}
	default:
		if err := s.creds.recordUsageError(label, fmt.Sprintf("usage fetch: HTTP %d", status)); err != nil {
			log.Printf("serve: usage poll: record error for %q: %v", label, err)
		}
	}
}

func (s *Server) markCredsDead(label, reason string, at time.Time) {
	s.usageStrikesClear(label)
	if err := s.creds.markDead(label, reason, at); err != nil {
		log.Printf("serve: usage poll: mark %q dead: %v", label, err)
	}
}

// usageStrikesIncrement/usageStrikesClear track consecutive-401 counts for
// the ACTIVE profile only (see usageDeadStrikeThreshold) — deliberately
// in-memory, not persisted: a daemon restart resetting the streak is the
// same "unknown, not dead" discipline the rest of this codebase already
// applies to liveness (seenSinceStart, ConsecutiveDeadTicks).
func (s *Server) usageStrikesIncrement(label string) int {
	s.usageStrikesMu.Lock()
	defer s.usageStrikesMu.Unlock()
	if s.usageStrikes == nil {
		s.usageStrikes = map[string]int{}
	}
	s.usageStrikes[label]++
	return s.usageStrikes[label]
}

func (s *Server) usageStrikesClear(label string) {
	s.usageStrikesMu.Lock()
	defer s.usageStrikesMu.Unlock()
	delete(s.usageStrikes, label)
}

// --- Proactive swap --------------------------------------------------------

// maybeProactiveSwap runs at the end of every poll pass: if the active
// profile's own last-polled snapshot shows it approaching its limit and a
// meaningfully-lower-usage profile is available, swap before any pane ever
// parks on a hard limit. See SPEC-usage-swap.md's "Swap decision" section
// for the exact condition. dialog/capture/sendKeys injectable so tests
// never touch osascript/tmux.
func (s *Server) maybeProactiveSwap(now time.Time, dialog dialogFunc, capture captureFunc, sendKeys sendKeysFunc) {
	if s.creds == nil || !s.config.Credentials.AutoSwap {
		return
	}
	active, err := s.creds.ResolveActive()
	if err != nil || active == "" {
		return
	}
	activeUsage := s.creds.ProfileUsage(active)
	if activeUsage == nil {
		return // never polled yet
	}
	swapAt := s.config.Credentials.SwapAtPercent
	if swapAt <= 0 {
		swapAt = credsDefaultSwapAtPercent
	}
	// weeklyExhausted is its own trigger, independent of the session
	// margin check below: a weekly cap of 100% makes the account
	// unusable regardless of how low its session percent happens to be
	// (e.g. session 10%, weekly 100% — the account is still dead until
	// the weekly window resets), so a session-percent margin comparison
	// against it doesn't mean anything and must not veto the swap.
	// Eligible already guarantees any candidate it returns has its OWN
	// weekly usage under 100%, so there's nothing further to check there.
	weeklyExhausted := activeUsage.Weekly.Percent >= 100
	if activeUsage.Session.Percent < swapAt && !weeklyExhausted {
		return
	}
	if last := s.creds.LastSwap(); last != nil {
		if now.Sub(last.At) < s.config.Credentials.MinSwapInterval {
			return
		}
	}
	_, eligible, err := s.creds.Eligible(swapAt)
	if err != nil || len(eligible) == 0 {
		return
	}
	target := eligible[0]
	targetPct := 0.0
	if u := s.creds.ProfileUsage(target); u != nil {
		targetPct = u.Session.Percent
	}
	if !weeklyExhausted {
		margin := s.config.Credentials.SwapMarginPercent
		if margin <= 0 {
			margin = credsDefaultSwapMarginPercent
		}
		if activeUsage.Session.Percent-targetPct < margin {
			return
		}
	}

	from, err := s.creds.Swap(target, fmt.Sprintf("usage %.0f%%", activeUsage.Session.Percent))
	if err != nil {
		log.Printf("serve: proactive credential swap to %q failed: %v", target, err)
		return
	}
	if err := s.creds.MarkLimited(from, activeUsage.Session.ResetsAt); err != nil {
		log.Printf("serve: proactive swap: mark %q limited: %v", from, err)
	}
	pending := s.scheduleNudge(target, capture, sendKeys, s.postSwapNudgeDone)
	log.Printf("serve: credentials proactively swapped %s → %s (session %.0f%% → %.0f%%), %d pane(s) pending nudge",
		from, target, activeUsage.Session.Percent, targetPct, len(pending))

	resets := "an unknown time"
	if !activeUsage.Session.ResetsAt.IsZero() {
		resets = activeUsage.Session.ResetsAt.Local().Format("15:04 MST")
	}
	text := fmt.Sprintf("Credentials swapped proactively — %s at %.0f%% (resets %s) → %s (%.0f%%)",
		from, activeUsage.Session.Percent, resets, target, targetPct)
	if _, derr := dialog("Usage limit — credentials swapped proactively", text, []string{"OK"}); derr != nil {
		log.Printf("serve: failed to show proactive-swap dialog: %v", derr)
	}
}

// --- Nudge timing ----------------------------------------------------------

// credsSwapSettle is how long to wait after writing the Keychain item and
// before nudging any parked pane. Claude Code's own Keychain read cache is
// ~30s (unofficial, but consistent across two independent tools and the
// installed 2.1.x build's observed behaviour): a nudge typed before that
// window elapses lands while the session still holds the OLD token and
// gets rejected or ignored. Overridden from serve.json
// credentials.nudgeDelay by cmdServe; zero in tests.
var credsSwapSettle = 35 * time.Second

// credsNudgeSkipMarkers: pane text containing any of these means typing
// into it cannot help — the pane parked on an AUTH failure, not a
// usage-limit wait, so there is no "continue" to nudge — and would only
// stack unread text in the input box (observed live, 2026-09-08/09
// incident).
var credsNudgeSkipMarkers = []string{"login expired", "please run /login"}

func shouldSkipNudge(pane string) bool {
	lower := strings.ToLower(lastNLines(pane, 30))
	for _, m := range credsNudgeSkipMarkers {
		if strings.Contains(lower, m) {
			return true
		}
	}
	return false
}

// scheduleNudge computes which currently-rate-limited panes will be
// nudged and returns their agent names immediately (for a swap response /
// dialog to display), then — after credsSwapSettle, off its own goroutine
// so the caller (an HTTP handler or the stuck-detector tick) returns
// immediately — re-captures each pane and nudges it, skipping any that now
// shows an auth failure (shouldSkipNudge). done is a test-only completion
// hook (nil in production) so tests can synchronize on the goroutine
// finishing without a race.
func (s *Server) scheduleNudge(toLabel string, capture captureFunc, sendKeys sendKeysFunc, done func()) []string {
	pending := s.rateLimitedSessions()
	names := make([]string, 0, len(pending))
	for _, p := range pending {
		names = append(names, p[0])
	}
	go func() {
		if credsSwapSettle > 0 {
			time.Sleep(credsSwapSettle)
		}
		for _, pair := range pending {
			name, session := pair[0], pair[1]
			if capture != nil {
				if pane, err := capture(session); err == nil && shouldSkipNudge(pane) {
					log.Printf("serve: skipping nudge for %s (session %s): pane shows an auth failure, not a usage-limit park", name, session)
					continue
				}
			}
			if err := nudgeParkedPane(session, toLabel, sendKeys); err != nil {
				log.Printf("serve: nudge %s (session %s) after credential swap: %v", name, session, err)
				continue
			}
			log.Printf("serve: nudged %s (session %s) to continue on profile %s", name, session, toLabel)
		}
		if done != nil {
			done()
		}
	}()
	return names
}

// --- HTTP: POST /creds/poll -------------------------------------------------

// handleCredsPoll implements POST /creds/poll: runs one poll pass
// synchronously (real HTTP calls to claude.ai when s.usageClient is the
// real client) and returns the same body GET /creds does — used by `tn
// creds list`'s daemon-freshness path and by tests that don't want to wait
// out the ticker.
func (s *Server) handleCredsPoll(w http.ResponseWriter, r *http.Request) {
	if s.creds == nil || s.usageClient == nil {
		http.Error(w, errCredsStoreUnavailable.Error(), http.StatusServiceUnavailable)
		return
	}
	dialog := dialogFuncForEnv()
	capture := s.stuckCapture
	if capture == nil {
		capture = capturePaneReal
	}
	sendKeys := s.stuckSendKeys
	if sendKeys == nil {
		sendKeys = sendKeysReal
	}
	s.pollUsageOnce(s.usageClient, time.Now(), dialog, capture, sendKeys)
	s.handleCredsList(w, r)
}

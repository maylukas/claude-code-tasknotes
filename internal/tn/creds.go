package tn

// Credential profiles + swap.
//
// Claude Code keeps its claude.ai OAuth credentials in the macOS login
// Keychain as one generic-password item (service claudeCredsService,
// account = the macOS username, secret = a JSON blob) and reads it through
// /usr/bin/security. Every Claude session on this machine reads that same
// item, so replacing its contents switches ALL of them — running sessions
// included — on their next API call (verified live 2026-09-02: after a
// manual /login every orchestrator pane printed "signed-in claude.ai
// account or organization changed on this machine").
//
// A "profile" is a saved copy of that blob under a user-chosen label. The
// secrets never touch disk: profiles are themselves Keychain items (service
// credsProfileService, account = label). Only non-secret bookkeeping
// (which label is active, when each was last used / last rate-limited)
// lives in ~/.local/state/tn/creds.json.
//
// Swapping is ordered so a refresh-token rotation is never lost: the blob
// currently in the Claude item is written BACK into the profile it belongs
// to before the item is overwritten. Without that, the next swap back
// would land on a refresh token Claude already rotated away — a dead
// profile that looks identical to a live one until the first API call.
//
// ~/.claude.json (oauthAccount metadata: email, org) is deliberately never
// touched: running sessions rewrite that file wholesale, so a concurrent
// edit is a corruption race, and nothing in the API path reads it — only
// /login's displayed email may lag behind the real active account.

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	// claudeCredsService is Claude Code's own Keychain item service name.
	claudeCredsService = "Claude Code-credentials"
	// credsProfileService is where tn keeps saved profiles (account = label).
	credsProfileService = "tn-credential-profiles"
	// securityBin is absolute on purpose: launchd's PATH has no /usr/bin
	// guarantees worth relying on, and the daemon runs under launchd.
	securityBin = "/usr/bin/security"
	// credsLimitFallbackCooldown is used when the pane's reset text can't
	// be parsed: Claude's session-limit window is five hours.
	credsLimitFallbackCooldown = 5 * time.Hour
	// credsDefaultMinSwapInterval bounds automatic swaps: several
	// orchestrators share one account and hit the limit together, and a
	// swap that didn't help must not immediately chain into another.
	credsDefaultMinSwapInterval = 10 * time.Minute
	// credsDefaultUsagePollInterval/credsMinUsagePollInterval bound the
	// usage poller's ticker (usage.go's startUsagePoller) — see
	// resolveServeConfig's clamp.
	credsDefaultUsagePollInterval = 60 * time.Second
	credsMinUsagePollInterval     = 30 * time.Second
	// credsDefaultSwapAtPercent/credsDefaultSwapMarginPercent are the
	// proactive-swap thresholds (usage.go's maybeProactiveSwap): swap once
	// the active profile's session window is at or above this percent, but
	// only to a candidate at least SwapMarginPercent lower — a swap that
	// barely helps isn't worth disconnecting Remote Control for.
	credsDefaultSwapAtPercent     = 90.0
	credsDefaultSwapMarginPercent = 20.0
	// credsDefaultNudgeDelay is usage.go's credsSwapSettle default —
	// Claude Code's own ~30s Keychain read cache, plus margin.
	credsDefaultNudgeDelay = 35 * time.Second
)

var (
	errKeychainNotFound      = errors.New("keychain item not found")
	errCredsProfileNotFound  = errors.New("credential profile not found")
	errCredsSameProfile      = errors.New("that profile is already the active one")
	errCredsActiveProfile    = errors.New("profile is the active one — swap to another profile first")
	errCredsInvalidLabel     = errors.New("invalid profile label (use a-z, 0-9, '.', '_', '-'; max 32 chars; must start with a letter or digit)")
	errCredsStoreUnavailable = errors.New("credential store unavailable")
	credsLabelRe             = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,31}$`)
	securityAcctRe           = regexp.MustCompile(`"acct"<blob>="([^"]*)"`)
)

// keychainStore is the injectable Keychain surface. Tests use a map-backed
// fake; only the real daemon/CLI ever construct securityCLIKeychain.
type keychainStore interface {
	Get(service, account string) ([]byte, error)
	Set(service, account string, secret []byte) error
	Delete(service, account string) error
	// FindAccount returns the account name of the (single) item under
	// service — Claude's item is keyed by the macOS username, which is
	// read rather than assumed.
	FindAccount(service string) (string, error)
}

// securityCLIKeychain drives /usr/bin/security — the same tool Claude Code
// itself uses, so the items it writes are shaped exactly like Claude's own.
type securityCLIKeychain struct{}

func (securityCLIKeychain) Get(service, account string) ([]byte, error) {
	out, err := exec.Command(securityBin, "find-generic-password", "-s", service, "-a", account, "-w").Output()
	if err != nil {
		if isKeychainNotFound(err) {
			return nil, errKeychainNotFound
		}
		return nil, fmt.Errorf("security find-generic-password: %w", err)
	}
	out = bytes.TrimSuffix(out, []byte("\n"))
	// -w prints the secret verbatim when it is printable text and as hex
	// otherwise. A JSON blob is printable, so the hex branch is defensive.
	if len(out) > 0 && len(out)%2 == 0 && looksHex(out) {
		if dec, err := hex.DecodeString(string(out)); err == nil && json.Valid(dec) {
			return dec, nil
		}
	}
	return out, nil
}

// Set writes (or replaces, -U) the item. The secret travels as a command
// argument — briefly visible in `ps` to other processes of the same user,
// which is exactly how Claude Code writes the item too; there is no
// stdin form of add-generic-password.
func (securityCLIKeychain) Set(service, account string, secret []byte) error {
	cmd := exec.Command(securityBin, "add-generic-password", "-s", service, "-a", account, "-U", "-w", string(secret))
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("security add-generic-password: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func (securityCLIKeychain) Delete(service, account string) error {
	out, err := exec.Command(securityBin, "delete-generic-password", "-s", service, "-a", account).CombinedOutput()
	if err != nil {
		if isKeychainNotFound(err) {
			return errKeychainNotFound
		}
		return fmt.Errorf("security delete-generic-password: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func (securityCLIKeychain) FindAccount(service string) (string, error) {
	// Metadata only (no -w): the secret is never printed here.
	out, err := exec.Command(securityBin, "find-generic-password", "-s", service).Output()
	if err != nil {
		if isKeychainNotFound(err) {
			return "", errKeychainNotFound
		}
		return "", fmt.Errorf("security find-generic-password: %w", err)
	}
	if m := securityAcctRe.FindSubmatch(out); m != nil && len(m[1]) > 0 {
		return string(m[1]), nil
	}
	return currentUsername(), nil
}

// isKeychainNotFound recognises `security`'s "item could not be found"
// failure: exit status 44 (errSecItemNotFound), with the message on stderr.
func isKeychainNotFound(err error) bool {
	var ee *exec.ExitError
	if !errors.As(err, &ee) {
		return false
	}
	return ee.ExitCode() == 44 || bytes.Contains(ee.Stderr, []byte("could not be found"))
}

func looksHex(b []byte) bool {
	for _, c := range b {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
			return false
		}
	}
	return true
}

func currentUsername() string {
	if u, err := user.Current(); err == nil && u.Username != "" {
		return u.Username
	}
	return os.Getenv("USER")
}

// credsTokens extracts the identity-bearing tokens from a Claude
// credentials blob. Tolerant: an unexpected shape yields empty strings,
// never an error — the blob is otherwise opaque to tn.
func credsTokens(blob []byte) (access, refresh string) {
	var v struct {
		ClaudeAiOauth struct {
			AccessToken  string `json:"accessToken"`
			RefreshToken string `json:"refreshToken"`
		} `json:"claudeAiOauth"`
	}
	if err := json.Unmarshal(blob, &v); err != nil {
		return "", ""
	}
	return v.ClaudeAiOauth.AccessToken, v.ClaudeAiOauth.RefreshToken
}

// credsProfileMeta is the non-secret bookkeeping for one profile.
type credsProfileMeta struct {
	SavedAt       time.Time `json:"savedAt"`
	LastUsedAt    time.Time `json:"lastUsedAt"`
	LastLimitedAt time.Time `json:"lastLimitedAt"`
	LimitedUntil  time.Time `json:"limitedUntil"`
	// Email is best-effort display text captured from ~/.claude.json's
	// oauthAccount at save time (read-only); empty when unavailable.
	Email string `json:"email,omitempty"`
	// Dead/DeadReason/DeadAt mark a profile the usage poller has confirmed
	// cannot authenticate at all (a 401 on usage fetch that a refresh
	// couldn't fix, or a refresh itself rejected 400/401) — see usage.go's
	// pollOneProfile. A dead profile is never returned by Eligible and is
	// refused by credsUse/handleCredsSwap unless force is passed. Cleared
	// automatically the moment a usage fetch for it succeeds again
	// (recordUsageSuccess) — a revoked token can become valid again after a
	// fresh /login into the same profile followed by `tn creds save`.
	Dead       bool      `json:"dead,omitempty"`
	DeadReason string    `json:"deadReason,omitempty"`
	DeadAt     time.Time `json:"deadAt,omitempty"`
	// Usage is the last successful usage-poll snapshot for this profile
	// (nil until the first successful poll). UsageError is the last
	// transient failure's message, cleared on the next success — kept
	// separate from Dead, which is reserved for a confirmed
	// can't-authenticate-at-all state.
	Usage      *usageSnapshot `json:"usage,omitempty"`
	UsageError string         `json:"usageError,omitempty"`
}

type credsMeta struct {
	Active   string                       `json:"active"`
	Profiles map[string]*credsProfileMeta `json:"profiles"`
}

// credsSwapRecord describes the most recent swap this daemon performed.
type credsSwapRecord struct {
	From    string    `json:"from"`
	To      string    `json:"to"`
	Trigger string    `json:"trigger"` // agent name for auto-swaps, "manual" for POST /creds/swap
	At      time.Time `json:"at"`
}

// credsStore is the daemon's (and CLI's) handle on profiles. One mutex
// serialises every Keychain round-trip — swap is a multi-step read-modify-
// write across two items and must never interleave with another.
type credsStore struct {
	mu       sync.Mutex
	kc       keychainStore
	metaPath string
	now      func() time.Time
	// emailLookup returns the currently signed-in account's email for
	// display; nil means unavailable. Injected so tests never read
	// ~/.claude.json.
	emailLookup func() string

	lastSwap *credsSwapRecord

	// ListCached's memo — its own mutex because List takes mu.
	cacheMu     sync.Mutex
	cacheAt     time.Time
	cacheActive string
	cacheViews  []credsProfileView
	cacheErr    error
}

func newCredsStore(kc keychainStore, metaPath string) *credsStore {
	return &credsStore{kc: kc, metaPath: metaPath, now: time.Now, emailLookup: readClaudeAccountEmail}
}

func defaultCredsMetaPath() string {
	return filepath.Join(filepath.Dir(defaultStatePath()), "creds.json")
}

// readClaudeAccountEmail reads (never writes) ~/.claude.json's
// oauthAccount.emailAddress for display. Empty on any failure.
func readClaudeAccountEmail() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	b, err := os.ReadFile(filepath.Join(home, ".claude.json"))
	if err != nil {
		return ""
	}
	var v struct {
		OauthAccount struct {
			EmailAddress string `json:"emailAddress"`
		} `json:"oauthAccount"`
	}
	if json.Unmarshal(b, &v) != nil {
		return ""
	}
	return v.OauthAccount.EmailAddress
}

func validCredsLabel(label string) bool { return credsLabelRe.MatchString(label) }

func (c *credsStore) loadMeta() (*credsMeta, error) {
	m := &credsMeta{Profiles: map[string]*credsProfileMeta{}}
	b, err := os.ReadFile(c.metaPath)
	if errors.Is(err, os.ErrNotExist) {
		return m, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, m); err != nil {
		return nil, fmt.Errorf("parse %s: %w", c.metaPath, err)
	}
	if m.Profiles == nil {
		m.Profiles = map[string]*credsProfileMeta{}
	}
	return m, nil
}

func (c *credsStore) saveMeta(m *credsMeta) error {
	if err := os.MkdirAll(filepath.Dir(c.metaPath), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	if err := atomicWriteFile(c.metaPath, b); err != nil {
		return err
	}
	// No secrets in here, but it still names accounts — keep it private.
	return os.Chmod(c.metaPath, 0o600)
}

func (c *credsStore) claudeAccount() (string, error) {
	acct, err := c.kc.FindAccount(claudeCredsService)
	if err != nil {
		if errors.Is(err, errKeychainNotFound) {
			return "", fmt.Errorf("no Claude Code credentials in the Keychain — run /login in a Claude session first")
		}
		return "", err
	}
	return acct, nil
}

func (c *credsStore) readClaudeBlob() ([]byte, error) {
	acct, err := c.claudeAccount()
	if err != nil {
		return nil, err
	}
	blob, err := c.kc.Get(claudeCredsService, acct)
	if errors.Is(err, errKeychainNotFound) {
		return nil, fmt.Errorf("no Claude Code credentials in the Keychain — run /login in a Claude session first")
	}
	return blob, err
}

// Save copies the current Claude item into profile label and makes it the
// active profile (the Keychain contents ARE this label, by definition).
func (c *credsStore) Save(label string) (email string, err error) {
	if !validCredsLabel(label) {
		return "", errCredsInvalidLabel
	}
	defer c.invalidateCache()
	c.mu.Lock()
	defer c.mu.Unlock()
	blob, err := c.readClaudeBlob()
	if err != nil {
		return "", err
	}
	if err := c.kc.Set(credsProfileService, label, blob); err != nil {
		return "", err
	}
	m, err := c.loadMeta()
	if err != nil {
		return "", err
	}
	p := m.Profiles[label]
	if p == nil {
		p = &credsProfileMeta{}
		m.Profiles[label] = p
	}
	p.SavedAt = c.now()
	if c.emailLookup != nil {
		if e := c.emailLookup(); e != "" {
			p.Email = e
		}
	}
	m.Active = label
	return p.Email, c.saveMeta(m)
}

// credsProfileView is the secret-free description List/GET /creds expose.
type credsProfileView struct {
	Label         string     `json:"label"`
	Email         string     `json:"email,omitempty"`
	Active        bool       `json:"active"`
	Stored        bool       `json:"stored"` // Keychain item present (false = metadata orphan)
	SavedAt       *time.Time `json:"savedAt,omitempty"`
	LastUsedAt    *time.Time `json:"lastUsedAt,omitempty"`
	LastLimitedAt *time.Time `json:"lastLimitedAt,omitempty"`
	LimitedUntil  *time.Time `json:"limitedUntil,omitempty"`
	// Dead/DeadReason/DeadAt/Usage/UsageError mirror credsProfileMeta's
	// fields of the same name — see its doc comments.
	Dead       bool           `json:"dead,omitempty"`
	DeadReason string         `json:"deadReason,omitempty"`
	DeadAt     *time.Time     `json:"deadAt,omitempty"`
	Usage      *usageSnapshot `json:"usage,omitempty"`
	UsageError string         `json:"usageError,omitempty"`
}

func credsTimePtr(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}

// List returns every profile, sorted by label, plus the active label.
// Active is RESOLVED against the live Keychain (see resolveActiveLocked),
// so a manual /login behind tn's back is reflected rather than believed.
func (c *credsStore) List() (active string, views []credsProfileView, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	m, err := c.loadMeta()
	if err != nil {
		return "", nil, err
	}
	active = c.resolveActiveLocked(m)
	labels := make([]string, 0, len(m.Profiles))
	for l := range m.Profiles {
		labels = append(labels, l)
	}
	sort.Strings(labels)
	for _, l := range labels {
		p := m.Profiles[l]
		_, gerr := c.kc.Get(credsProfileService, l)
		views = append(views, credsProfileView{
			Label:         l,
			Email:         p.Email,
			Active:        l == active,
			Stored:        gerr == nil,
			SavedAt:       credsTimePtr(p.SavedAt),
			LastUsedAt:    credsTimePtr(p.LastUsedAt),
			LastLimitedAt: credsTimePtr(p.LastLimitedAt),
			LimitedUntil:  credsTimePtr(p.LimitedUntil),
			Dead:          p.Dead,
			DeadReason:    p.DeadReason,
			DeadAt:        credsTimePtr(p.DeadAt),
			Usage:         p.Usage,
			UsageError:    p.UsageError,
		})
	}
	return active, views, nil
}

// Remove deletes a profile (item + metadata). Refuses the active one: the
// Keychain would still hold its tokens, so "removed" would be a lie.
func (c *credsStore) Remove(label string) error {
	defer c.invalidateCache()
	c.mu.Lock()
	defer c.mu.Unlock()
	m, err := c.loadMeta()
	if err != nil {
		return err
	}
	if _, ok := m.Profiles[label]; !ok {
		return errCredsProfileNotFound
	}
	if c.resolveActiveLocked(m) == label {
		return errCredsActiveProfile
	}
	if err := c.kc.Delete(credsProfileService, label); err != nil && !errors.Is(err, errKeychainNotFound) {
		return err
	}
	delete(m.Profiles, label)
	if m.Active == label {
		m.Active = ""
	}
	return c.saveMeta(m)
}

// ResolveActive reports which stored profile the live Claude item belongs
// to ("" if none matches and nothing is recorded).
func (c *credsStore) ResolveActive() (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	m, err := c.loadMeta()
	if err != nil {
		return "", err
	}
	return c.resolveActiveLocked(m), nil
}

// resolveActiveLocked matches the live Claude blob against every stored
// profile by refresh token, then access token; falls back to the recorded
// Active label when nothing matches (e.g. the Keychain is unreadable).
func (c *credsStore) resolveActiveLocked(m *credsMeta) string {
	blob, err := c.readClaudeBlob()
	if err != nil {
		return m.Active
	}
	curAccess, curRefresh := credsTokens(blob)
	if curRefresh == "" && curAccess == "" {
		return m.Active
	}
	var byAccess string
	labels := make([]string, 0, len(m.Profiles))
	for l := range m.Profiles {
		labels = append(labels, l)
	}
	sort.Strings(labels) // deterministic when two profiles hold the same blob
	for _, l := range labels {
		pb, err := c.kc.Get(credsProfileService, l)
		if err != nil {
			continue
		}
		a, r := credsTokens(pb)
		if curRefresh != "" && r == curRefresh {
			return l
		}
		if byAccess == "" && curAccess != "" && a == curAccess {
			byAccess = l
		}
	}
	if byAccess != "" {
		return byAccess
	}
	return m.Active
}

// Swap makes profile target the live Claude credentials. Order matters:
//  1. read the live blob;
//  2. work out which profile it belongs to;
//  3. write the live blob BACK into that profile (captures a rotated
//     refresh token — see the file comment);
//  4. read the target profile;
//  5. overwrite the Claude item;
//  6. record active/lastUsedAt.
//
// Returns the label swapped away from ("" if unknown).
func (c *credsStore) Swap(target, trigger string) (from string, err error) {
	defer c.invalidateCache()
	c.mu.Lock()
	defer c.mu.Unlock()
	m, err := c.loadMeta()
	if err != nil {
		return "", err
	}
	if _, ok := m.Profiles[target]; !ok {
		return "", errCredsProfileNotFound
	}
	targetBlob, err := c.kc.Get(credsProfileService, target)
	if err != nil {
		if errors.Is(err, errKeychainNotFound) {
			return "", fmt.Errorf("%w: profile %q has metadata but no Keychain item — re-run `tn creds save %s`", errCredsProfileNotFound, target, target)
		}
		return "", err
	}
	acct, err := c.claudeAccount()
	if err != nil {
		return "", err
	}
	cur, err := c.kc.Get(claudeCredsService, acct)
	if err != nil && !errors.Is(err, errKeychainNotFound) {
		return "", err
	}
	from = c.resolveActiveLocked(m)
	if from == target {
		return from, errCredsSameProfile
	}
	if from != "" && len(cur) > 0 {
		if err := c.kc.Set(credsProfileService, from, cur); err != nil {
			return from, fmt.Errorf("write back current credentials into profile %q: %w", from, err)
		}
	}
	if err := c.kc.Set(claudeCredsService, acct, targetBlob); err != nil {
		return from, fmt.Errorf("write Claude credentials: %w", err)
	}
	now := c.now()
	m.Active = target
	m.Profiles[target].LastUsedAt = now
	c.lastSwap = &credsSwapRecord{From: from, To: target, Trigger: trigger, At: now}
	return from, c.saveMeta(m)
}

// LastSwap returns the most recent swap performed through this store
// (this process lifetime), or nil.
func (c *credsStore) LastSwap() *credsSwapRecord {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.lastSwap == nil {
		return nil
	}
	r := *c.lastSwap
	return &r
}

// MarkLimited records that profile label just hit a usage limit that
// resets at until (from the usage API's Session.ResetsAt — never parsed
// from pane text, see SPEC-usage-swap.md); the profile is ineligible for
// automatic swaps until then. A zero or already-past until falls back to
// credsLimitFallbackCooldown (Claude's session window), for a snapshot
// that had no resets_at or is stale. Unknown labels are ignored.
func (c *credsStore) MarkLimited(label string, until time.Time) error {
	defer c.invalidateCache()
	c.mu.Lock()
	defer c.mu.Unlock()
	m, err := c.loadMeta()
	if err != nil {
		return err
	}
	p := m.Profiles[label]
	if p == nil {
		return nil
	}
	now := c.now()
	if until.IsZero() || !until.After(now) {
		until = now.Add(credsLimitFallbackCooldown)
	}
	p.LastLimitedAt = now
	p.LimitedUntil = until
	return c.saveMeta(m)
}

// Eligible lists profiles that could be swapped TO right now: stored, not
// the active one, not dead, not cooling down, and — when a usage snapshot
// exists for the candidate — under swapAtPercent on its session window and
// under 100% on its weekly window. Sorted by session percent ascending
// (lowest usage first), then weekly percent ascending, then LastUsedAt
// ascending, then label — a candidate with no snapshot yet sorts as 0/0,
// i.e. before any known-nonzero usage, same as the pre-usage-API
// oldest-used-first behaviour.
func (c *credsStore) Eligible(swapAtPercent float64) (active string, eligible []string, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	m, err := c.loadMeta()
	if err != nil {
		return "", nil, err
	}
	active = c.resolveActiveLocked(m)
	now := c.now()
	type cand struct {
		label      string
		used       time.Time
		sessionPct float64
		weeklyPct  float64
	}
	var cs []cand
	for l, p := range m.Profiles {
		if l == active || p.Dead {
			continue
		}
		if !p.LimitedUntil.IsZero() && p.LimitedUntil.After(now) {
			continue
		}
		if _, err := c.kc.Get(credsProfileService, l); err != nil {
			continue
		}
		x := cand{label: l, used: p.LastUsedAt}
		if p.Usage != nil {
			if p.Usage.Session.Percent >= swapAtPercent || p.Usage.Weekly.Percent >= 100 {
				continue
			}
			x.sessionPct = p.Usage.Session.Percent
			x.weeklyPct = p.Usage.Weekly.Percent
		}
		cs = append(cs, x)
	}
	sort.Slice(cs, func(i, j int) bool {
		if cs[i].sessionPct != cs[j].sessionPct {
			return cs[i].sessionPct < cs[j].sessionPct
		}
		if cs[i].weeklyPct != cs[j].weeklyPct {
			return cs[i].weeklyPct < cs[j].weeklyPct
		}
		if !cs[i].used.Equal(cs[j].used) {
			return cs[i].used.Before(cs[j].used)
		}
		return cs[i].label < cs[j].label
	})
	for _, x := range cs {
		eligible = append(eligible, x.label)
	}
	return active, eligible, nil
}

// DeadReason reports whether label is currently marked dead, and why (see
// credsProfileMeta.Dead). Unknown labels report false, "".
func (c *credsStore) DeadReason(label string) (dead bool, reason string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	m, err := c.loadMeta()
	if err != nil {
		return false, ""
	}
	p := m.Profiles[label]
	if p == nil {
		return false, ""
	}
	return p.Dead, p.DeadReason
}

// ProfileUsage returns a copy of label's last successful usage snapshot, or
// nil if the profile is unknown or has never been polled successfully.
func (c *credsStore) ProfileUsage(label string) *usageSnapshot {
	c.mu.Lock()
	defer c.mu.Unlock()
	m, err := c.loadMeta()
	if err != nil {
		return nil
	}
	p := m.Profiles[label]
	if p == nil || p.Usage == nil {
		return nil
	}
	u := *p.Usage
	return &u
}

// blobFor returns the Keychain blob currently backing label — the live
// Claude item when active is true, the profile's own item otherwise (see
// usage.go's ownership rule doc comment).
func (c *credsStore) blobFor(label string, active bool) ([]byte, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if active {
		return c.readClaudeBlob()
	}
	return c.kc.Get(credsProfileService, label)
}

// writeProfileBlob overwrites an INACTIVE profile's own Keychain item —
// used only by the usage poller after refreshing that profile's token
// (see the ownership rule: tn never writes the active/Claude item except
// via Swap).
func (c *credsStore) writeProfileBlob(label string, blob []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.kc.Set(credsProfileService, label, blob)
}

// recordUsageSuccess stores a fresh snapshot for label and clears any
// Dead/UsageError state — a usage fetch succeeding is proof the profile
// can authenticate right now, regardless of what it looked like before.
func (c *credsStore) recordUsageSuccess(label string, snap *usageSnapshot) error {
	defer c.invalidateCache()
	c.mu.Lock()
	defer c.mu.Unlock()
	m, err := c.loadMeta()
	if err != nil {
		return err
	}
	p := m.Profiles[label]
	if p == nil {
		return nil
	}
	wasDead := p.Dead
	p.Dead, p.DeadReason, p.DeadAt = false, "", time.Time{}
	p.UsageError = ""
	p.Usage = snap
	if wasDead {
		log.Printf("serve: credential profile %q revived (usage fetch succeeded)", label)
	}
	return c.saveMeta(m)
}

// recordUsageError stores a transient failure note for label (a fetch/
// refresh error that doesn't rise to "confirmed dead" — see markDead),
// keeping its last good snapshot untouched.
func (c *credsStore) recordUsageError(label, msg string) error {
	defer c.invalidateCache()
	c.mu.Lock()
	defer c.mu.Unlock()
	m, err := c.loadMeta()
	if err != nil {
		return err
	}
	p := m.Profiles[label]
	if p == nil {
		return nil
	}
	p.UsageError = msg
	return c.saveMeta(m)
}

// markDead marks label as confirmed unable to authenticate (see
// credsProfileMeta.Dead) — logs only on the alive->dead transition, never
// on a repeated confirmation.
func (c *credsStore) markDead(label, reason string, at time.Time) error {
	defer c.invalidateCache()
	c.mu.Lock()
	defer c.mu.Unlock()
	m, err := c.loadMeta()
	if err != nil {
		return err
	}
	p := m.Profiles[label]
	if p == nil {
		return nil
	}
	wasDead := p.Dead
	p.Dead, p.DeadReason, p.DeadAt = true, reason, at
	if !wasDead {
		log.Printf("serve: credential profile %q marked dead: %s", label, reason)
	}
	return c.saveMeta(m)
}

// credsBlobTokens is the identity-bearing fields parseCredsBlobTokens pulls
// out of a Claude credentials blob, including ExpiresAt (unlike
// credsTokens, which is deliberately access/refresh only for the
// resolveActiveLocked matching path).
type credsBlobTokens struct {
	Access    string
	Refresh   string
	ExpiresAt time.Time // zero when absent/zero in the blob
}

func parseCredsBlobTokens(blob []byte) credsBlobTokens {
	var v struct {
		ClaudeAiOauth struct {
			AccessToken  string `json:"accessToken"`
			RefreshToken string `json:"refreshToken"`
			ExpiresAt    int64  `json:"expiresAt"`
		} `json:"claudeAiOauth"`
	}
	if json.Unmarshal(blob, &v) != nil {
		return credsBlobTokens{}
	}
	t := credsBlobTokens{Access: v.ClaudeAiOauth.AccessToken, Refresh: v.ClaudeAiOauth.RefreshToken}
	if v.ClaudeAiOauth.ExpiresAt > 0 {
		t.ExpiresAt = time.UnixMilli(v.ClaudeAiOauth.ExpiresAt)
	}
	return t
}

// updateCredsBlobTokens returns blob with claudeAiOauth.accessToken/
// refreshToken/expiresAt updated, preserving every other field in the blob
// (scopes, subscriptionType, ...) — a raw map round-trip rather than a
// typed struct, since those other fields are opaque to tn and must survive
// a refresh untouched. refresh is only overwritten when non-empty (a
// refresh response's refresh_token is documented nullable — "reuse the
// existing one" when absent).
func updateCredsBlobTokens(blob []byte, access, refresh string, expiresAt time.Time) ([]byte, error) {
	var v map[string]any
	if err := json.Unmarshal(blob, &v); err != nil {
		return nil, err
	}
	oauth, _ := v["claudeAiOauth"].(map[string]any)
	if oauth == nil {
		oauth = map[string]any{}
	}
	if access != "" {
		oauth["accessToken"] = access
	}
	if refresh != "" {
		oauth["refreshToken"] = refresh
	}
	if !expiresAt.IsZero() {
		oauth["expiresAt"] = expiresAt.UnixMilli()
	}
	v["claudeAiOauth"] = oauth
	return json.Marshal(v)
}

// credsNudgeText is what gets typed into a parked pane after a swap. It
// cancels the "continuing automatically at …" wait (any keystroke does)
// and gives the session the same continuation instruction Claude itself
// prints when a limit resets, so the resume is indistinguishable from
// the natural one.
func credsNudgeText(toLabel string) string {
	return fmt.Sprintf("Usage limit: tn swapped credentials to profile %s. Continue the task you were working on when the limit was reached; do not repeat work that is already complete.", toLabel)
}

// credsNudgeDelay separates the Escape (which cancels the wait and lands
// the pane on its prompt) from the typed text; zero in tests.
var credsNudgeDelay = 500 * time.Millisecond

// nudgeParkedPane resumes one rate-limited pane: Escape (cancel the
// automatic wait; harmless on an idle prompt), then the continuation text.
// sendKeys appends Enter to each call (sendKeysReal), so Escape+Enter is an
// empty submit — a no-op — and the text is submitted as a prompt.
func nudgeParkedPane(session, toLabel string, sendKeys sendKeysFunc) error {
	if err := sendKeys(session, "Escape"); err != nil {
		return err
	}
	if credsNudgeDelay > 0 {
		time.Sleep(credsNudgeDelay)
	}
	return sendKeys(session, credsNudgeText(toLabel))
}

// ---- CLI: tn creds ----------------------------------------------------

func cmdCreds(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: tn creds save|list|usage|use|rm [label]")
	}
	store := newCredsStore(securityCLIKeychain{}, defaultCredsMetaPath())
	sub, rest := args[0], args[1:]
	switch sub {
	case "save":
		if len(rest) != 1 {
			return fmt.Errorf("usage: tn creds save <label>")
		}
		email, err := store.Save(rest[0])
		if err != nil {
			return err
		}
		if email != "" {
			fmt.Printf("saved profile %q (%s) — now the active profile\n", rest[0], email)
		} else {
			fmt.Printf("saved profile %q — now the active profile\n", rest[0])
		}
		probeSavedProfileUsage(store, rest[0])
		fmt.Println("note: never /logout to switch accounts — it revokes the token you just saved; /login directly into the other account instead.")
		return nil
	case "list", "usage":
		return cmdCredsList(store)
	case "use":
		fs := flag.NewFlagSet("tn creds use", flag.ContinueOnError)
		force := fs.Bool("force", false, "swap even if the target profile is marked dead")
		if err := fs.Parse(rest); err != nil {
			return err
		}
		if fs.NArg() != 1 {
			return fmt.Errorf("usage: tn creds use [--force] <label>")
		}
		return credsUse(store, fs.Arg(0), *force)
	case "rm":
		if len(rest) != 1 {
			return fmt.Errorf("usage: tn creds rm <label>")
		}
		if err := store.Remove(rest[0]); err != nil {
			return err
		}
		fmt.Printf("removed profile %q\n", rest[0])
		return nil
	default:
		return fmt.Errorf("tn creds: unknown subcommand %q (save|list|usage|use|rm)", sub)
	}
}

// probeSavedProfileUsage does one usage Fetch right after a save with the
// blob that was just written, so a profile that can't actually authenticate
// (already-revoked refresh token, wrong account) is reported immediately —
// see SPEC-usage-swap.md's incident write-up — rather than discovered hours
// later when auto-swap picks it and takes every session down. Best effort:
// prints the result or the failure, never returns an error (a save that
// succeeded must not be reported as failed because the network probe
// afterward had trouble).
func probeSavedProfileUsage(store *credsStore, label string) {
	blob, err := store.kc.Get(credsProfileService, label)
	if err != nil {
		return
	}
	tok := parseCredsBlobTokens(blob)
	if tok.Access == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	snap, status, err := newHTTPUsageClient().Fetch(ctx, tok.Access)
	switch {
	case err != nil:
		fmt.Printf("usage check failed: %v\n", err)
	case status == http.StatusOK:
		fmt.Printf("session %.0f%% · weekly %.0f%%\n", snap.Session.Percent, snap.Weekly.Percent)
	default:
		fmt.Printf("usage check failed: HTTP %d (this profile may not be able to authenticate)\n", status)
	}
}

// cmdCredsList implements `tn creds list`/`tn creds usage`: prefers the
// daemon's GET /creds (fresh usage snapshots, kept current by the poller),
// falling back to reading the store directly (metadata only, no HTTP) when
// the daemon isn't reachable.
func cmdCredsList(store *credsStore) error {
	b := NewBridgeClient(resolveBridgeURL())
	if body, err := b.request(http.MethodGet, "/creds", nil); err == nil {
		var resp struct {
			Active   string             `json:"active"`
			Profiles []credsProfileView `json:"profiles"`
		}
		if json.Unmarshal(body, &resp) == nil {
			printCredsList(resp.Active, resp.Profiles)
			return nil
		}
	}
	active, views, err := store.List()
	if err != nil {
		return err
	}
	printCredsList(active, views)
	return nil
}

// printCredsList renders the LABEL EMAIL SESSION WEEKLY RESETS
// LIMITED-UNTIL LAST-USED STATE table. SESSION/WEEKLY/RESETS are blank
// when no usage snapshot is available yet (store fallback, or a profile
// never polled).
func printCredsList(active string, views []credsProfileView) {
	if len(views) == 0 {
		fmt.Println("no credential profiles — run /login in a Claude session, then `tn creds save <label>`")
		return
	}
	now := time.Now()
	fmt.Printf("%-2s %-16s %-32s %-8s %-8s %-6s %-17s %-17s %s\n",
		"", "LABEL", "EMAIL", "SESSION", "WEEKLY", "RESETS", "LIMITED-UNTIL", "LAST-USED", "STATE")
	for _, v := range views {
		mark := " "
		if v.Active {
			mark = "*"
		}
		session, weekly, resets := "", "", ""
		if v.Usage != nil {
			session = fmt.Sprintf("%.0f%%", v.Usage.Session.Percent)
			weekly = fmt.Sprintf("%.0f%%", v.Usage.Weekly.Percent)
			if !v.Usage.Session.ResetsAt.IsZero() {
				resets = v.Usage.Session.ResetsAt.Local().Format("15:04")
			}
		}
		limited := ""
		if v.LimitedUntil != nil && v.LimitedUntil.After(now) {
			limited = v.LimitedUntil.Local().Format("2006-01-02 15:04")
		}
		used := ""
		if v.LastUsedAt != nil {
			used = v.LastUsedAt.Local().Format("2006-01-02 15:04")
		}
		state := ""
		switch {
		case v.Active:
			state = "active"
		case v.Dead:
			state = "DEAD: " + v.DeadReason
		case !v.Stored:
			state = "stale (no Keychain item)"
		}
		fmt.Printf("%-2s %-16s %-32s %-8s %-8s %-6s %-17s %-17s %s\n",
			mark, v.Label, v.Email, session, weekly, resets, limited, used, state)
	}
	if active == "" {
		fmt.Println("\n(current Keychain credentials match no saved profile — `tn creds save <label>` to capture them)")
	}
}

// credsUse prefers the daemon (POST /creds/swap) so parked orchestrator
// panes get nudged (after the settle delay — see usage.go's
// credsSwapSettle); without a daemon it swaps directly and says so. A dead
// target is refused unless force is set.
func credsUse(store *credsStore, label string, force bool) error {
	b := NewBridgeClient(resolveBridgeURL())
	body, err := b.request("POST", "/creds/swap", map[string]any{"label": label, "force": force})
	if err == nil {
		var res struct {
			From         string   `json:"from"`
			To           string   `json:"to"`
			NudgePending []string `json:"nudgePending"`
		}
		_ = json.Unmarshal(body, &res)
		fmt.Printf("swapped credentials %s → %s via daemon; %d parked pane(s) will be nudged in ~%s\n",
			orUnknown(res.From), res.To, len(res.NudgePending), credsSwapSettle)
		fmt.Println("note: every Claude session on this machine now uses this account (Remote Control sessions disconnect).")
		return nil
	}
	var unreachable *bridgeUnreachableError
	if !errors.As(err, &unreachable) {
		var he *httpError
		if errors.As(err, &he) {
			return fmt.Errorf("daemon refused swap: %s", strings.TrimSpace(he.Body))
		}
		return err
	}
	if !force {
		if dead, reason := store.DeadReason(label); dead {
			return fmt.Errorf("profile %q is dead: %s (pass --force to override)", label, reason)
		}
	}
	from, err := store.Swap(label, "manual")
	if err != nil {
		return err
	}
	fmt.Printf("swapped credentials %s → %s directly (daemon not running — parked panes were NOT nudged)\n", orUnknown(from), label)
	fmt.Println("note: every Claude session on this machine now uses this account (Remote Control sessions disconnect).")
	return nil
}

func orUnknown(s string) string {
	if s == "" {
		return "(unknown)"
	}
	return s
}

// ---- daemon integration ------------------------------------------------

// credsAutoSwapResult is what updateStuckState folds into the usage-limit
// dialog. nil (from autoSwapCredentials) means auto-swap is off or no
// store is wired — the 0.5.6 dialog is shown unchanged.
type credsAutoSwapResult struct {
	swapped bool
	from    string
	to      string
	nudged  []string
	note    string // why no swap happened, or the error; one line, no secrets
}

func (r *credsAutoSwapResult) dialogText(agentName, resetsAt string) string {
	resets := resetsAt
	if resets == "" {
		resets = "an unknown time"
	}
	if r.swapped {
		return fmt.Sprintf("%s hit the claude.ai session limit (resets %s). tn swapped credentials %s → %s; every Claude session on this machine now uses %s. %d parked orchestrator pane(s) nudged to continue.",
			agentName, resets, orUnknown(r.from), r.to, r.to, len(r.nudged))
	}
	return buildRateLimitDialogText(resetsAt) + " Auto-swap: " + r.note
}

// autoSwapCredentials is the rate-limit episode hook. When enabled it
// marks the active profile as limited (using its own last-polled usage
// snapshot's Session.ResetsAt, never pane text — see MarkLimited), swaps
// to the top eligible profile (respecting MinSwapInterval since the last
// swap), and schedules a nudge for EVERY currently rate-limited pane —
// they share the account, so they all parked together and all come back
// together. Every failure degrades to "no swap + a note in the dialog";
// nothing here can take the detector down. capture/sendKeys are threaded
// through to scheduleNudge (usage.go) so tests never touch tmux/osascript.
func (s *Server) autoSwapCredentials(agentName, resetRaw string, now time.Time, capture captureFunc, sendKeys sendKeysFunc) *credsAutoSwapResult {
	if s.creds == nil || !s.config.Credentials.AutoSwap {
		return nil
	}
	res := &credsAutoSwapResult{}
	if last := s.creds.LastSwap(); last != nil {
		if since := now.Sub(last.At); since < s.config.Credentials.MinSwapInterval {
			res.note = fmt.Sprintf("last swap (%s → %s) was %s ago, under the %s minimum — not swapping again yet.",
				orUnknown(last.From), last.To, since.Truncate(time.Second), s.config.Credentials.MinSwapInterval)
			log.Printf("serve: credential auto-swap skipped for %s: %s", agentName, res.note)
			return res
		}
	}
	swapAt := s.config.Credentials.SwapAtPercent
	if swapAt <= 0 {
		swapAt = credsDefaultSwapAtPercent
	}
	active, eligible, err := s.creds.Eligible(swapAt)
	if err != nil {
		res.note = "could not read credential profiles: " + err.Error()
		log.Printf("serve: credential auto-swap failed for %s: %v", agentName, err)
		return res
	}
	if active != "" {
		var until time.Time
		if u := s.creds.ProfileUsage(active); u != nil {
			until = u.Session.ResetsAt
		}
		if err := s.creds.MarkLimited(active, until); err != nil {
			log.Printf("serve: credential auto-swap: mark %q limited: %v", active, err)
		}
	}
	if len(eligible) == 0 {
		res.note = "no eligible credential profile (none saved, all cooling down, or all dead)."
		log.Printf("serve: credential auto-swap skipped for %s: %s", agentName, res.note)
		return res
	}
	target := eligible[0]
	from, err := s.creds.Swap(target, agentName)
	if err != nil {
		res.note = fmt.Sprintf("swap to %q failed: %v", target, err)
		log.Printf("serve: credential auto-swap failed for %s: %v", agentName, err)
		return res
	}
	res.swapped, res.from, res.to = true, from, target
	log.Printf("serve: credentials auto-swapped %s → %s (trigger %s, resets %s)", orUnknown(from), target, agentName, resetRaw)
	res.nudged = s.scheduleNudge(target, capture, sendKeys, s.postSwapNudgeDone)
	return res
}

// rateLimitedSessions lists (agent, tmux session) for every agent whose
// pane is currently tracked as rate-limited.
func (s *Server) rateLimitedSessions() [][2]string {
	s.stuck.mu.Lock()
	var names []string
	for name, st := range s.stuck.byName {
		if st.RateLimited {
			names = append(names, name)
		}
	}
	s.stuck.mu.Unlock()
	sort.Strings(names)
	var out [][2]string
	s.mu.Lock()
	for _, name := range names {
		a := s.state.Agents[name]
		if a == nil || a.DeregisteredAt != nil {
			continue
		}
		if session, ok := tmuxSessionForAgent(*a); ok {
			out = append(out, [2]string{name, session})
		}
	}
	s.mu.Unlock()
	return out
}

// handleCredsList implements GET /creds: profiles and swap state, never
// secrets. Spectator-safe.
func (s *Server) handleCredsList(w http.ResponseWriter, r *http.Request) {
	if s.creds == nil {
		http.Error(w, errCredsStoreUnavailable.Error(), http.StatusServiceUnavailable)
		return
	}
	active, views, err := s.creds.List()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if views == nil {
		views = []credsProfileView{}
	}
	lastPollAt, lastPollErr := s.usageLastPoll()
	writeJSON(w, http.StatusOK, map[string]any{
		"autoSwap":          s.config.Credentials.AutoSwap,
		"minSwapInterval":   s.config.Credentials.MinSwapInterval.String(),
		"usagePollInterval": s.config.Credentials.UsagePollInterval.String(),
		"swapAtPercent":     s.config.Credentials.SwapAtPercent,
		"active":            active,
		"profiles":          views,
		"lastSwap":          s.creds.LastSwap(),
		"lastPollAt":        credsTimePtr(lastPollAt),
		"lastPollError":     lastPollErr,
	})
}

// handleCredsSwap implements POST /creds/swap {"label","force"}: a manual
// swap, then the same settle-then-nudge every automatic swap does (see
// usage.go's scheduleNudge — the response reports which panes are PENDING
// a nudge, not which have already received one). 404 unknown label, 409
// already active or (without force) a dead target, 503 no store.
func (s *Server) handleCredsSwap(w http.ResponseWriter, r *http.Request) {
	if s.creds == nil {
		http.Error(w, errCredsStoreUnavailable.Error(), http.StatusServiceUnavailable)
		return
	}
	var req struct {
		Label string `json:"label"`
		Force bool   `json:"force"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	if req.Label == "" {
		http.Error(w, "label is required", http.StatusBadRequest)
		return
	}
	if !req.Force {
		if dead, reason := s.creds.DeadReason(req.Label); dead {
			http.Error(w, fmt.Sprintf("profile %q is dead: %s (pass force to override)", req.Label, reason), http.StatusConflict)
			return
		}
	}
	from, err := s.creds.Swap(req.Label, "manual")
	switch {
	case errors.Is(err, errCredsProfileNotFound):
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	case errors.Is(err, errCredsSameProfile):
		http.Error(w, err.Error(), http.StatusConflict)
		return
	case err != nil:
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	log.Printf("serve: credentials swapped %s → %s (manual)", orUnknown(from), req.Label)
	capture := s.stuckCapture
	if capture == nil {
		capture = capturePaneReal
	}
	sendKeys := s.stuckSendKeys
	if sendKeys == nil {
		sendKeys = sendKeysReal
	}
	pending := s.scheduleNudge(req.Label, capture, sendKeys, s.postSwapNudgeDone)
	if pending == nil {
		pending = []string{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"from": from, "to": req.Label, "nudgePending": pending})
}

// credsStatusCacheTTL bounds how often /status re-reads the Keychain:
// the tray polls every 5s and each List is one `security` subprocess per
// profile. Swap/Save/Remove invalidate it, so a change shows on the very
// next poll regardless.
const credsStatusCacheTTL = 15 * time.Second

// credentialsStatus is /status.credentials — nil when no store is wired.
func (s *Server) credentialsStatus() *statusCredentials {
	if s.creds == nil {
		return nil
	}
	lastPollAt, lastPollErr := s.usageLastPoll()
	out := &statusCredentials{
		AutoSwap:          s.config.Credentials.AutoSwap,
		MinSwapInterval:   s.config.Credentials.MinSwapInterval.String(),
		UsagePollInterval: s.config.Credentials.UsagePollInterval.String(),
		SwapAtPercent:     s.config.Credentials.SwapAtPercent,
		LastPollAt:        credsTimePtr(lastPollAt),
		LastPollError:     lastPollErr,
		Profiles:          []credsProfileView{},
		LastSwap:          s.creds.LastSwap(),
	}
	active, views, err := s.creds.ListCached(credsStatusCacheTTL)
	if err != nil {
		out.Error = err.Error()
		return out
	}
	out.Active = active
	if views != nil {
		out.Profiles = views
	}
	return out
}

// ListCached is List with a short TTL; see credsStatusCacheTTL.
func (c *credsStore) ListCached(ttl time.Duration) (string, []credsProfileView, error) {
	c.cacheMu.Lock()
	if c.cacheAt.IsZero() || c.now().Sub(c.cacheAt) >= ttl {
		active, views, err := c.List()
		c.cacheActive, c.cacheViews, c.cacheErr, c.cacheAt = active, views, err, c.now()
	}
	active, views, err := c.cacheActive, append([]credsProfileView(nil), c.cacheViews...), c.cacheErr
	c.cacheMu.Unlock()
	return active, views, err
}

func (c *credsStore) invalidateCache() {
	c.cacheMu.Lock()
	c.cacheAt = time.Time{}
	c.cacheMu.Unlock()
}

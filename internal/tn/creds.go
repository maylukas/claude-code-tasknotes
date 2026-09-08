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
	"encoding/hex"
	"encoding/json"
	"errors"
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

// MarkLimited records that profile label just hit a usage limit whose
// pane text said it resets at resetRaw; the profile is ineligible for
// automatic swaps until then (or credsLimitFallbackCooldown when the text
// can't be parsed). Unknown labels are ignored.
func (c *credsStore) MarkLimited(label, resetRaw string) (until time.Time, err error) {
	defer c.invalidateCache()
	c.mu.Lock()
	defer c.mu.Unlock()
	m, err := c.loadMeta()
	if err != nil {
		return time.Time{}, err
	}
	p := m.Profiles[label]
	if p == nil {
		return time.Time{}, nil
	}
	now := c.now()
	until, ok := parseResetTime(resetRaw, now)
	if !ok {
		until = now.Add(credsLimitFallbackCooldown)
	}
	p.LastLimitedAt = now
	p.LimitedUntil = until
	return until, c.saveMeta(m)
}

// Eligible lists profiles that could be swapped TO right now: stored,
// not the active one, not cooling down. Oldest-used first (round-robin).
func (c *credsStore) Eligible() (active string, eligible []string, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	m, err := c.loadMeta()
	if err != nil {
		return "", nil, err
	}
	active = c.resolveActiveLocked(m)
	now := c.now()
	type cand struct {
		label string
		used  time.Time
	}
	var cs []cand
	for l, p := range m.Profiles {
		if l == active {
			continue
		}
		if !p.LimitedUntil.IsZero() && p.LimitedUntil.After(now) {
			continue
		}
		if _, err := c.kc.Get(credsProfileService, l); err != nil {
			continue
		}
		cs = append(cs, cand{l, p.LastUsedAt})
	}
	sort.Slice(cs, func(i, j int) bool {
		if cs[i].used.Equal(cs[j].used) {
			return cs[i].label < cs[j].label
		}
		return cs[i].used.Before(cs[j].used)
	})
	for _, x := range cs {
		eligible = append(eligible, x.label)
	}
	return active, eligible, nil
}

// parseResetTime turns the pane's raw reset text ("2:30pm (Europe/Berlin)",
// "2:30pm", "15:04", "3pm") into the next such wall-clock instant after
// now. Best effort: ok=false when the text isn't a recognisable clock time.
func parseResetTime(raw string, now time.Time) (time.Time, bool) {
	s := strings.TrimSpace(raw)
	loc := now.Location()
	if i := strings.Index(s, "("); i >= 0 {
		if j := strings.Index(s[i:], ")"); j > 0 {
			if l, err := time.LoadLocation(strings.TrimSpace(s[i+1 : i+j])); err == nil {
				loc = l
			}
		}
		s = strings.TrimSpace(s[:i])
	}
	s = strings.ToLower(s)
	var clock time.Time
	var parsed bool
	for _, layout := range []string{"3:04pm", "3pm", "15:04", "15"} {
		t, err := time.Parse(layout, s)
		if err == nil {
			clock, parsed = t, true
			break
		}
	}
	if !parsed {
		return time.Time{}, false
	}
	local := now.In(loc)
	t := time.Date(local.Year(), local.Month(), local.Day(), clock.Hour(), clock.Minute(), 0, 0, loc)
	if !t.After(now) {
		t = t.Add(24 * time.Hour)
	}
	return t, true
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
		return fmt.Errorf("usage: tn creds save|list|use|rm [label]")
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
		return nil
	case "list":
		active, views, err := store.List()
		if err != nil {
			return err
		}
		if len(views) == 0 {
			fmt.Println("no credential profiles — run /login in a Claude session, then `tn creds save <label>`")
			return nil
		}
		now := time.Now()
		fmt.Printf("%-2s %-16s %-32s %-22s %s\n", "", "LABEL", "EMAIL", "LIMITED UNTIL", "LAST USED")
		for _, v := range views {
			mark := " "
			if v.Active {
				mark = "*"
			}
			limited := ""
			if v.LimitedUntil != nil && v.LimitedUntil.After(now) {
				limited = v.LimitedUntil.Local().Format("2006-01-02 15:04")
			}
			used := ""
			if v.LastUsedAt != nil {
				used = v.LastUsedAt.Local().Format("2006-01-02 15:04")
			}
			if !v.Stored {
				used = "(no Keychain item — re-save)"
			}
			fmt.Printf("%-2s %-16s %-32s %-22s %s\n", mark, v.Label, v.Email, limited, used)
		}
		if active == "" {
			fmt.Println("\n(current Keychain credentials match no saved profile — `tn creds save <label>` to capture them)")
		}
		return nil
	case "use":
		if len(rest) != 1 {
			return fmt.Errorf("usage: tn creds use <label>")
		}
		return credsUse(store, rest[0])
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
		return fmt.Errorf("tn creds: unknown subcommand %q (save|list|use|rm)", sub)
	}
}

// credsUse prefers the daemon (POST /creds/swap) so parked orchestrator
// panes get nudged; without a daemon it swaps directly and says so.
func credsUse(store *credsStore, label string) error {
	b := NewBridgeClient(resolveBridgeURL())
	body, err := b.request("POST", "/creds/swap", map[string]any{"label": label})
	if err == nil {
		var res struct {
			From   string   `json:"from"`
			To     string   `json:"to"`
			Nudged []string `json:"nudged"`
		}
		_ = json.Unmarshal(body, &res)
		fmt.Printf("swapped credentials %s → %s via daemon; %d parked pane(s) nudged\n", orUnknown(res.From), res.To, len(res.Nudged))
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
// marks the active profile as limited until the pane's reset time, swaps
// to the oldest-used eligible profile (respecting MinSwapInterval since
// the last swap), and nudges EVERY currently rate-limited pane — they
// share the account, so they all parked together and all come back
// together. Every failure degrades to "no swap + a note in the dialog";
// nothing here can take the detector down.
func (s *Server) autoSwapCredentials(agentName, resetRaw string, now time.Time, sendKeys sendKeysFunc) *credsAutoSwapResult {
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
	active, eligible, err := s.creds.Eligible()
	if err != nil {
		res.note = "could not read credential profiles: " + err.Error()
		log.Printf("serve: credential auto-swap failed for %s: %v", agentName, err)
		return res
	}
	if active != "" {
		if _, err := s.creds.MarkLimited(active, resetRaw); err != nil {
			log.Printf("serve: credential auto-swap: mark %q limited: %v", active, err)
		}
	}
	if len(eligible) == 0 {
		res.note = "no eligible credential profile (none saved, or all cooling down)."
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
	res.nudged = s.nudgeRateLimitedPanes(target, sendKeys)
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

// nudgeRateLimitedPanes resumes every parked pane after a swap; returns
// the agent names that were nudged successfully.
func (s *Server) nudgeRateLimitedPanes(toLabel string, sendKeys sendKeysFunc) []string {
	var nudged []string
	for _, pair := range s.rateLimitedSessions() {
		name, session := pair[0], pair[1]
		if err := nudgeParkedPane(session, toLabel, sendKeys); err != nil {
			log.Printf("serve: nudge %s (session %s) after credential swap: %v", name, session, err)
			continue
		}
		log.Printf("serve: nudged %s (session %s) to continue on profile %s", name, session, toLabel)
		nudged = append(nudged, name)
	}
	return nudged
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
	writeJSON(w, http.StatusOK, map[string]any{
		"autoSwap":        s.config.Credentials.AutoSwap,
		"minSwapInterval": s.config.Credentials.MinSwapInterval.String(),
		"active":          active,
		"profiles":        views,
		"lastSwap":        s.creds.LastSwap(),
	})
}

// handleCredsSwap implements POST /creds/swap {"label"}: a manual swap,
// then the same pane nudge auto-swap does. 404 unknown label, 409 already
// active, 503 no store.
func (s *Server) handleCredsSwap(w http.ResponseWriter, r *http.Request) {
	if s.creds == nil {
		http.Error(w, errCredsStoreUnavailable.Error(), http.StatusServiceUnavailable)
		return
	}
	var req struct {
		Label string `json:"label"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	if req.Label == "" {
		http.Error(w, "label is required", http.StatusBadRequest)
		return
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
	sendKeys := s.stuckSendKeys
	if sendKeys == nil {
		sendKeys = sendKeysReal
	}
	nudged := s.nudgeRateLimitedPanes(req.Label, sendKeys)
	if nudged == nil {
		nudged = []string{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"from": from, "to": req.Label, "nudged": nudged})
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
	out := &statusCredentials{
		AutoSwap:        s.config.Credentials.AutoSwap,
		MinSwapInterval: s.config.Credentials.MinSwapInterval.String(),
		Profiles:        []credsProfileView{},
		LastSwap:        s.creds.LastSwap(),
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

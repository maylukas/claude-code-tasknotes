package tn

import (
	"bytes"
	"fmt"
	"log"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// --- Stuck session detection ---

const (
	// stuckCheckInitialDelay is how long after daemon startup the first
	// detector pass runs, ahead of the steady-state ticker — binary
	// self-restarts reset any ticker, so without an eager pass a prompt
	// appearing around a restart could wait up to stuckCheckInterval+.
	stuckCheckInitialDelay = 15 * time.Second
	// stuckCheckInterval is how often the detector inspects each alive
	// orchestrator's tmux pane.
	stuckCheckInterval = 2 * time.Minute
	// stuckExcerptLines bounds BOTH how much of the pane is scanned for a
	// live permission prompt (marker + numbered options must both appear
	// within this window — see detectLiveStuckPrompt) and how much is kept
	// as the excerpt shown on the agent's session page when stuck. Used to
	// be two separate windows (a 40-line marker scan, a 15-line excerpt) —
	// unified to one narrow window per the actual live incident: a marker
	// substring anywhere in a wide scan routinely matched an
	// already-answered prompt still sitting in scrollback (Claude prints
	// resolved prompts verbatim as history), which fired a fresh osascript
	// dialog for something the user had already dealt with.
	stuckExcerptLines = 15
	// stuckContextScanLines is a WIDER window used ONLY to build the
	// excerpt/dialog text actually DISPLAYED to a human, once a prompt is
	// already confirmed live via the narrow stuckExcerptLines window above
	// — never used for detection itself (detectLiveStuckPrompt/
	// parseStuckPromptOptions keep using stuckExcerptLines, unchanged).
	// Live incident (2026-08-26): a user asked "I would like to see the
	// command I'm allowing" — the daemon's own stuckExcerptLines=15 window
	// captured only the marker line and the numbered options, dropping the
	// "Bash command · from the X agent" header and the actual command text
	// sitting immediately above them in the pane. Verified live against
	// the real pane that reported the gap: a 40-line capture contains the
	// whole block. Widening ONLY the display window (not detection) is
	// safe in a way bumping stuckExcerptLines itself wouldn't be: this
	// only ever runs after detectLiveStuckPrompt has already confirmed a
	// live prompt on the narrow window, so it cannot reintroduce the
	// stale-scrollback false positive that narrowed detection from 40 to
	// 15 in the first place (see stuckExcerptLines's own doc comment) —
	// worst case it shows a little more leading context than the true
	// block, never less, which is the direction degrading is acceptable.
	stuckContextScanLines = 40
	// unackedBacklogThreshold is how long a (non-Informational) message can
	// sit delivered-but-unacked before it counts as backlog worth surfacing
	// — a DIFFERENT thing from stuck (an agent visibly wedged on a
	// permission prompt). Named UnackedBacklog, not "stalled" (its
	// 2026-08-25-and-earlier name): this measures inbox hygiene, not
	// whether the agent is actually doing anything — a demonstrably
	// working agent can carry this. See Message.Informational's doc
	// comment for the incident that forced the rename.
	unackedBacklogThreshold = 30 * time.Minute
	// stuckDialogRepromptInterval bounds how often the approval dialog is
	// re-shown for an agent that stays stuck on the SAME prompt (see
	// stuckSessionState.LastPromptedHash — a genuinely new prompt episode
	// always reprompts immediately, regardless of this interval).
	stuckDialogRepromptInterval = 15 * time.Minute
	// rateLimitScanLines is the window detectRateLimitWait scans — wider
	// than stuckExcerptLines (15) because a usage-limit block is several
	// lines tall and can be pushed down by whatever the session printed
	// just before it stopped (a failed teammate line, a partial tool
	// result), while the permission-prompt detector's own window stays
	// deliberately narrow (see stuckExcerptLines's doc comment: widening
	// THAT window is what reintroduced stale-scrollback false positives).
	// Widening is safe here because there is nothing to falsely
	// "re-answer": a usage-limit wait has no interactive response, the
	// dialog is purely informational and fires at most once per episode,
	// and the explicit "usage limit has reset" clear (checked BEFORE the
	// markers) is what ends an episode whose banner is still in
	// scrollback.
	rateLimitScanLines = 30
)

// stuckMarkers are substrings that, found in a captured tmux pane, indicate
// the session is blocked on an interactive permission prompt no one is
// watching (a headless session can never answer these itself).
var stuckMarkers = []string{
	"Do you want to proceed?",
	"requires confirmation",
	"Esc to cancel",
}

// rateLimitMarkers are lowercase substrings that, found in the last
// rateLimitScanLines of a captured pane, mean the session is parked on a
// claude.ai usage limit rather than working or blocked on a permission
// prompt. Matched case-insensitively; any one is enough. Verbatim shapes
// seen live:
//
//	You've hit your session limit · resets 2:30pm (Europe/Berlin)
//	Continuing automatically at 2:30pm · esc to cancel
//	⏺ Usage limit reached · continuing automatically at 2:30pm · esc or type to cancel
//	⏺ Teammate @doc-placement-research failed: You've hit your session limit · resets 2:30pm (Europe/Berlin)
//
// This is a SEPARATE condition from Stuck, not a variant of it: a
// permission prompt needs a human to answer it, a usage limit resolves
// itself at the reset time. Neither suppresses detection of the other.
var rateLimitMarkers = []string{
	"hit your session limit",
	"hit your limit",
	"usage limit reached",
	"continuing automatically at",
}

// rateLimitClearedMarker is the line Claude prints when the limit lifts
// ("Your claude.ai usage limit has reset. Continue the task you were
// working on..."), after which the session resumes on its own. Checked
// BEFORE rateLimitMarkers: the banner that announced the limit is still
// sitting in the same window at that moment, so marker-presence alone
// would keep the episode open for as long as it took to scroll away.
const rateLimitClearedMarker = "usage limit has reset"

// detectRateLimitWait reports whether the pane shows a live usage-limit
// wait, plus the RAW reset-time text as printed (e.g. "2:30pm
// (Europe/Berlin)" or "2:30pm") — deliberately never parsed into a
// time.Time: the pane prints a bare local clock time whose date, DST
// treatment and timezone label are all Claude's, and re-deriving an
// absolute instant from that would invent precision the source doesn't
// have. It is display text and an episode identity, nothing more, so ""
// (no reset text found) is a perfectly valid limited=true result.
func detectRateLimitWait(pane string) (resetsAt string, limited bool) {
	recent := lastNLines(pane, rateLimitScanLines)
	lower := strings.ToLower(recent)
	if strings.Contains(lower, rateLimitClearedMarker) {
		return "", false
	}
	found := false
	for _, m := range rateLimitMarkers {
		if strings.Contains(lower, m) {
			found = true
			break
		}
	}
	if !found {
		return "", false
	}
	return parseRateLimitResetText(recent, lower), true
}

// parseRateLimitResetText pulls the raw reset-time text out of a pane
// excerpt: whatever follows "resets " up to end-of-line or a " ·"
// separator, falling back to whatever follows "continuing automatically
// at ". recent and lower must be the same text, the latter lowercased —
// the match is case-insensitive but the returned slice comes from the
// original so its capitalization survives.
func parseRateLimitResetText(recent, lower string) string {
	for _, prefix := range []string{"resets ", "continuing automatically at "} {
		i := strings.Index(lower, prefix)
		if i < 0 {
			continue
		}
		rest := recent[i+len(prefix):]
		if j := strings.IndexByte(rest, '\n'); j >= 0 {
			rest = rest[:j]
		}
		if j := strings.Index(rest, " ·"); j >= 0 {
			rest = rest[:j]
		}
		if v := strings.TrimSpace(rest); v != "" {
			return v
		}
	}
	return ""
}

// captureFunc, dialogFunc, and sendKeysFunc are injectable so tests never
// touch tmux or osascript. dialogFunc shows a dialog with exactly the given
// buttons (built per-prompt by resolveStuckPromptMapping — permission
// prompts come in more than one shape, so the buttons vary) and returns the
// clicked button's label, or "" for a timeout/no selection — both are
// treated as "do nothing" by the caller, so dialogFunc only returns a
// non-nil error for an actual invocation failure.
type captureFunc func(session string) (string, error)
type dialogFunc func(title, text string, buttons []string) (string, error)
type sendKeysFunc func(session, keys string) error

// stuckSessionState is one orchestrator's live detection state. Entirely
// in-memory — never persisted; a fresh daemon restart simply re-detects.
type stuckSessionState struct {
	Stuck          bool
	UnackedBacklog bool
	Excerpt        string    // last stuckExcerptLines of the captured pane, only while Stuck
	StuckSince     time.Time // start of the current stuck episode
	LastPrompted   time.Time // last time the approval dialog was shown for it
	// LastPromptedHash is stuckEpisodeSignature (marker + numbered
	// options), NOT a hash of the raw Excerpt text, at the last dialog
	// show. A tick whose SIGNATURE differs is a NEW prompt episode (e.g.
	// several subagents each triggering their own worktree-relocation
	// prompt back-to-back) and reprompts immediately regardless of
	// stuckDialogRepromptInterval; an unchanged signature means it's still
	// the same prompt (even if the raw excerpt text changed — a
	// repainting pane, elapsed-time counter, or streaming output would
	// otherwise look like a new episode on almost every tick and defeat
	// the reprompt interval entirely), so the normal interval applies.
	LastPromptedHash string
	// LastRawExcerpt/PendingExcerptCandidate implement two-tick
	// confirmation for Stuck (see updateStuckState) — a per-tick marker+
	// options match alone isn't enough: a resolved dialog's text can sit
	// in the scan window while the session is genuinely working below it
	// (confirmed live — g53286's false positive: a running subagent with
	// a climbing token count and elapsed timer, stale "Teach auto mode"
	// prompt text still rendered above it). The distinguishing signal is
	// behavioural, not positional: does the window change between ticks?
	// LastRawExcerpt is the FULL scanned window from the previous tick,
	// captured regardless of stuck status — unlike the public Excerpt
	// field, which only holds content while CONFIRMED stuck.
	// PendingExcerptCandidate records whether a signature was ALSO
	// present last tick, so a signature appearing for the first time
	// never confirms on the same tick it's first seen (a real block
	// takes one extra tick to alert; accepted deliberately — see
	// updateStuckState).
	LastRawExcerpt          string
	PendingExcerptCandidate bool
	// PersistentSignatureCount/LastSeenSignature implement the
	// persistence backstop (see updateStuckState and
	// stuckPersistenceBackstopTicks): the exact-match fast path above
	// requires the FULL window byte-identical across ticks, which
	// correctly rejects a stale dialog beside UNRELATED progress
	// (g53286) — but a GENUINELY blocked agent whose own pane keeps
	// changing for an unrelated reason would never confirm via that path
	// either, and could hang silently forever. ORCHESTRATOR.md has
	// agents own long-running background jobs, which keep emitting
	// output while the foreground is frozen on a permission prompt —
	// exactly that shape. This tracks how many CONSECUTIVE ticks the
	// same dialog SIGNATURE (marker+options — stuckEpisodeSignature, not
	// the raw text) has been seen, regardless of what else in the pane
	// changed, and confirms once the streak reaches
	// stuckPersistenceBackstopTicks — bounding the worst case to a LATE
	// alert instead of a silent one.
	PersistentSignatureCount int
	LastSeenSignature        string
	// HookCommand/HookToolName cache a PermissionRequest hook payload's
	// structured tool_input.command/tool_name (see recordHookCommand) —
	// sourced from Claude Code's own hook JSON, immune to truncation by
	// any pane scan window, unlike anything derived from captured pane
	// text. HookCommandSignature records WHICH prompt episode (see
	// LastSeenSignature) they belong to; updateStuckState only surfaces
	// them when this matches the CURRENT signature, and clears all three
	// once it no longer does — so a command reported for a RESOLVED
	// episode is never shown for a later, different one. Empty whenever
	// the hook hasn't fired for the current episode (e.g. a hard
	// permissions.deny rule bypasses the hook entirely — SPEC-serve.md —
	// or the tool wasn't Bash-shaped), which is fine: display falls back
	// to the wider pane capture alone (stuckContextScanLines).
	HookCommand          string
	HookToolName         string
	HookCommandSignature string
	// RateLimited/RateLimitResetsAt/RateLimitSince track a claude.ai
	// usage-limit wait (detectRateLimitWait) — an INDEPENDENT condition
	// from Stuck, tracked on the same struct only because it comes from
	// the same pane capture on the same tick. A rate-limited session is
	// not stuck (nothing to answer) and not dead (it resumes by itself
	// at the reset time), but it does stop polling its inbox, so without
	// this it reads as "⚫ gone" with an unexplained growing backlog.
	// RateLimitResetsAt is raw pane text (see detectRateLimitWait), and
	// doubles as the episode identity: a different reset string is a
	// different episode.
	RateLimited       bool
	RateLimitResetsAt string
	RateLimitSince    time.Time
	// RateLimitDialogShown/RateLimitDialogResets enforce exactly one
	// dialog per episode. Unlike the stuck dialog there is no reprompt
	// interval: the notification is purely informational (nothing to
	// approve), so re-showing it every 15 minutes for something that
	// resolves itself would be pure nagging. Both are cleared when the
	// episode ends, so an identically-shaped later episode still alerts
	// — the same re-arming reason LastPromptedHash is cleared on clear.
	RateLimitDialogShown  bool
	RateLimitDialogResets string
}

// stuckPersistenceBackstopTicks bounds the worst-case delay before a
// genuinely blocked agent alerts even when its pane keeps changing for
// an unrelated reason (see stuckSessionState.PersistentSignatureCount's
// doc comment). Chosen so the bound is roughly 10 minutes at
// stuckCheckInterval — late, never absent, and short enough that "the
// user is ignoring a stuck badge that never fired" isn't itself the
// failure mode.
const stuckPersistenceBackstopTicks = 5 // 5 * stuckCheckInterval (2min) = 10min

// stuckInfo is a read-only, point-in-time view of one agent's stuck state,
// for dashboard/session-page rendering.
type stuckInfo struct {
	Stuck             bool
	UnackedBacklog    bool
	Excerpt           string
	RateLimited       bool
	RateLimitResetsAt string
	RateLimitSince    time.Time
}

// stuckTracker holds per-agent stuckSessionState behind its own mutex,
// independent of Server.mu (see Server.stuck's doc comment for why).
type stuckTracker struct {
	mu     sync.Mutex
	byName map[string]*stuckSessionState
}

func newStuckTracker() *stuckTracker {
	return &stuckTracker{byName: map[string]*stuckSessionState{}}
}

// snapshot returns a read-only copy of every tracked agent's stuck state,
// for rendering.
func (t *stuckTracker) snapshot() map[string]stuckInfo {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make(map[string]stuckInfo, len(t.byName))
	for name, st := range t.byName {
		out[name] = stuckInfo{
			Stuck:             st.Stuck,
			UnackedBacklog:    st.UnackedBacklog,
			Excerpt:           st.Excerpt,
			RateLimited:       st.RateLimited,
			RateLimitResetsAt: st.RateLimitResetsAt,
			RateLimitSince:    st.RateLimitSince,
		}
	}
	return out
}

// detectLiveStuckPrompt reports whether pane shows a permission prompt that
// is LIVE right now — not the transcript of one already answered and
// sitting in scrollback. Claude's own pane prints a resolved prompt
// verbatim as history, so a bare marker-substring match alone routinely
// re-detects something the user already dealt with (confirmed live:
// /status.stuckPrompts read 0 while an osascript dialog was firing for
// exactly this reason). A prompt only counts as live if BOTH a
// stuckMarkers substring AND at least one numbered menu option
// (parseStuckPromptOptions) appear within the SAME narrow window — the
// last stuckExcerptLines lines. A prompt that has scrolled out of that
// window is by definition resolved: its option list scrolled with it.
// Returns the matching marker line (trimmed) and true only when both hold.
func detectLiveStuckPrompt(pane string) (string, bool) {
	recent := lastNLines(pane, stuckExcerptLines)
	var markerLine string
	for _, line := range strings.Split(recent, "\n") {
		for _, marker := range stuckMarkers {
			if strings.Contains(line, marker) {
				markerLine = strings.TrimSpace(line)
			}
		}
		if markerLine != "" {
			break
		}
	}
	if markerLine == "" {
		return "", false
	}
	if len(parseStuckPromptOptions(pane)) == 0 {
		return "", false
	}
	return markerLine, true
}

// stuckOptionRe matches a numbered menu option line, e.g. "❯ 1. Yes" or
// "  2. Yes, and don't ask again this session" or "3. No, and tell Claude
// what to do differently (esc)".
var stuckOptionRe = regexp.MustCompile(`(?m)^\s*(?:❯\s*)?(\d)\.\s*(.+)$`)

// stuckPromptOption is one parsed numbered menu option.
type stuckPromptOption struct {
	Number int
	Text   string
}

// stuckEpisodeSignature returns a stable identifier for the CURRENT prompt
// episode: the marker line plus every parsed numbered option's number and
// text — deliberately NOT a hash of the raw excerpt text. A repainting
// pane (elapsed-time counter, spinner, streaming tool output) changes the
// raw excerpt on nearly every detector tick while the SAME prompt is still
// showing; hashing that raw text made almost every tick look like a
// brand-new episode and reprompt immediately, defeating
// stuckDialogRepromptInterval entirely (the live incident this fixes).
// Only a genuine change in the marker or WHICH options are offered means a
// different prompt.
func stuckEpisodeSignature(markerLine string, opts []stuckPromptOption) string {
	parts := make([]string, 0, len(opts)+1)
	parts = append(parts, markerLine)
	for _, o := range opts {
		parts = append(parts, strconv.Itoa(o.Number)+":"+o.Text)
	}
	return sha256Hex(strings.Join(parts, "\n"))
}

// parseStuckPromptOptions extracts numbered menu options from the last
// stuckExcerptLines of a captured pane — the SAME window
// detectLiveStuckPrompt requires a marker in, so an option belonging to an
// old, scrolled-away prompt is never mistaken for one on a current prompt.
// Permission prompts come in more than one shape (2-option Yes/No,
// 3-option Yes/Yes-don't-ask-again/No), so callers must resolve option
// meaning from the parsed text — never assume a fixed position/count.
func parseStuckPromptOptions(pane string) []stuckPromptOption {
	recent := lastNLines(pane, stuckExcerptLines)
	matches := stuckOptionRe.FindAllStringSubmatch(recent, -1)
	opts := make([]stuckPromptOption, 0, len(matches))
	for _, m := range matches {
		n, err := strconv.Atoi(m[1])
		if err != nil {
			continue
		}
		opts = append(opts, stuckPromptOption{Number: n, Text: strings.TrimSpace(m[2])})
	}
	return opts
}

// stuckPromptMapping resolves which numbered option corresponds to "Allow
// once" (the bare "Yes" option), "Always allow" (an option whose text
// mentions not asking again), and "Deny" (an option whose text starts with
// "No" — denial is a legitimate response the dialog must be able to send,
// not just a way to dismiss without acting) for one specific captured
// prompt. Any key may be "" if that option isn't present (e.g. a 2-option
// Yes/No prompt has no "Always allow" equivalent).
type stuckPromptMapping struct {
	AllowOnceKey   string
	AlwaysAllowKey string
	DenyKey        string
}

// resolveStuckPromptMapping parses pane fresh and maps its numbered options
// by TEXT, not position — a "don't ask again" option is 2 on some prompts
// and 3 on others.
func resolveStuckPromptMapping(pane string) stuckPromptMapping {
	var m stuckPromptMapping
	for _, opt := range parseStuckPromptOptions(pane) {
		switch {
		case strings.Contains(strings.ToLower(opt.Text), "don't ask again"):
			m.AlwaysAllowKey = strconv.Itoa(opt.Number)
		case opt.Text == "Yes":
			m.AllowOnceKey = strconv.Itoa(opt.Number)
		case strings.HasPrefix(opt.Text, "No"):
			m.DenyKey = strconv.Itoa(opt.Number)
		}
	}
	return m
}

// stuckDialogButtons builds the dialog's button list from a resolved
// mapping: "Dismiss" always first (and the default), then whichever of
// "Deny"/"Allow once"/"Always allow" actually has a corresponding option on
// this specific prompt.
func stuckDialogButtons(m stuckPromptMapping) []string {
	buttons := []string{"Dismiss"}
	if m.DenyKey != "" {
		buttons = append(buttons, "Deny")
	}
	if m.AllowOnceKey != "" {
		buttons = append(buttons, "Allow once")
	}
	if m.AlwaysAllowKey != "" {
		buttons = append(buttons, "Always allow")
	}
	return buttons
}

// stuckDialogTextMaxLen caps the dialog body's length (runes, not bytes,
// so multi-byte glyphs like "❯" count once) — long enough to show real
// context, short enough that osascript's dialog stays readable.
const stuckDialogTextMaxLen = 600

// buildDisplayExcerpt builds what updateStuckState stores as st.Excerpt —
// the text ultimately shown both in /status (tray/webui) and, via
// stuckPromptContext downstream, in the native approval dialog. Two
// sources, combined in priority order (command, subagent/reason context,
// then the marker+options the wider capture and stuckPromptContext already
// carry — see this function's own callers for where options get trimmed):
//
//  1. hookCommand, if hookSignature matches signature (this exact prompt
//     episode) — Claude Code's own structured tool_input.command, reported
//     by the PermissionRequest hook (see stuckSessionState.HookCommand's
//     doc comment). Reliable and un-truncatable, but only ever present
//     when the hook fired for THIS episode (Bash-shaped tool, not blocked
//     by a hard permissions.deny rule that bypasses the hook entirely).
//  2. pane, captured wide (stuckContextScanLines, not the narrow detection
//     window) — the only source for the human-readable framing (which
//     subagent asked, the classifier's stated reason) a hook payload
//     doesn't carry at all. Prepending (1) here, rather than replacing (2)
//     with it, means a command missing from the pane's own rendering (long
//     command truncated/wrapped oddly) is still shown exactly, while the
//     surrounding context a human actually needs to judge the request
//     stays intact either way.
func buildDisplayExcerpt(pane, hookCommand, hookToolName, hookSignature, signature string) string {
	wide := lastNLines(pane, stuckContextScanLines)
	if hookCommand == "" || hookSignature != signature {
		return wide
	}
	label := hookToolName
	if label == "" {
		label = "command"
	}
	return fmt.Sprintf("[hook] %s: %s\n\n%s", label, hookCommand, wide)
}

// stuckPromptContext extracts the request's context from a stored excerpt:
// from the first non-empty line (typically a "Tool use ..." header or the
// action description) down to and including the permission/marker line,
// with numbered menu option lines (see stuckOptionRe) dropped and runs of
// blank lines collapsed to one. This is what actually lets a human judge
// what's being approved — showing just the bare marker line ("Do you want
// to proceed?") gives no context at all.
//
// If no marker line is found in the excerpt (shouldn't happen when called
// only while stuck, but defensive), the whole excerpt is used as a
// fallback rather than returning nothing.
func stuckPromptContext(excerpt string) string {
	lines := strings.Split(excerpt, "\n")

	markerIdx := -1
	for i, line := range lines {
		for _, marker := range stuckMarkers {
			if strings.Contains(line, marker) {
				markerIdx = i
				break
			}
		}
		if markerIdx != -1 {
			break
		}
	}
	end := len(lines)
	if markerIdx != -1 {
		end = markerIdx + 1
	}

	start := 0
	for start < end && strings.TrimSpace(lines[start]) == "" {
		start++
	}

	var out []string
	prevBlank := false
	for _, line := range lines[start:end] {
		if stuckOptionRe.MatchString(line) {
			continue
		}
		blank := strings.TrimSpace(line) == ""
		if blank && prevBlank {
			continue
		}
		out = append(out, line)
		prevBlank = blank
	}
	return strings.TrimSpace(strings.Join(out, "\n"))
}

// truncateMiddle shortens s to at most maxRunes runes, cutting out the
// middle (replaced with "…") so both the start of the request and its
// ending (permission line) stay visible — a tail-only truncation would
// lose the part that actually says what's being asked for.
func truncateMiddle(s string, maxRunes int) string {
	r := []rune(s)
	if len(r) <= maxRunes {
		return s
	}
	const ellipsis = "…"
	avail := maxRunes - 1
	if avail < 0 {
		avail = 0
	}
	head := avail / 2
	tail := avail - head
	return string(r[:head]) + ellipsis + string(r[len(r)-tail:])
}

// buildStuckDialogText renders the approval dialog's body: the request's
// context (stuckPromptContext), middle-truncated to stuckDialogTextMaxLen,
// plus a tmux attach hint.
func buildStuckDialogText(excerpt, session string) string {
	ctx := truncateMiddle(stuckPromptContext(excerpt), stuckDialogTextMaxLen)
	return fmt.Sprintf("%s\n\n(tmux attach -t %s to inspect)", ctx, session)
}

// buildRateLimitDialogText is the once-per-episode usage-limit
// notification's body. Purely informational — there is nothing to approve
// and no keys to send — so it states the one thing that is NOT obvious
// from the pane: the session comes back on its own, and the way to get it
// back sooner is another account's credentials, which every Claude
// session on this machine picks up (they share the same credential
// store), not something done per-pane.
func buildRateLimitDialogText(resetsAt string) string {
	resets := resetsAt
	if resets == "" {
		resets = "an unknown time"
	}
	return fmt.Sprintf("Claude Code session limit hit; resets %s. The session continues automatically at reset. "+
		"To continue now: run /login in any Claude session with another account (all sessions on this machine pick up "+
		"the new credentials), or /usage-credits in the pane.", resets)
}

// lastNLines returns the last n lines of text (fewer if text is shorter).
func lastNLines(text string, n int) string {
	lines := strings.Split(strings.TrimRight(text, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

// oldestDeliveredUnackedAge returns how long the oldest delivered-but-unacked
// message addressed to agentName has been waiting, or 0 if there is none.
// Only "delivered" messages count — a merely "queued" one hasn't reached
// the agent yet, so its age isn't evidence of a stuck/unackedBacklog session.
// Informational messages (see Message.Informational) are excluded entirely:
// they ask nothing of the recipient, so an agent never acking one is not
// evidence of anything — counting it here is exactly the 2026-08-25
// incident (a working agent flagged off one FYI message that sat unacked
// for ~3h because there was nothing to ack).
func oldestDeliveredUnackedAge(agentName string, messages []Message, now time.Time) time.Duration {
	var oldest time.Time
	found := false
	for _, m := range messages {
		if m.To != agentName || m.Status != "delivered" || m.Informational {
			continue
		}
		at := m.CreatedAt
		if m.DeliveredAt != nil {
			at = *m.DeliveredAt
		}
		if !found || at.Before(oldest) {
			oldest = at
			found = true
		}
	}
	if !found {
		return 0
	}
	return now.Sub(oldest)
}

// sessionStatusDisplay implements the dashboard/session-page status
// precedence: rate-limited > gone > stuck > unackedBacklog > draining >
// alive.
//
// rateLimited deliberately outranks even "gone", which every other state
// loses to: a session parked on a usage limit stops polling its inbox, so
// it goes not-alive within aliveWindow BY DEFINITION, and reporting that
// as "⚫ gone" would hide the one fact that actually explains it — it is
// coming back by itself at the reset time, and nobody needs to
// investigate or respawn it. It also outranks unackedBacklog, which
// satisfies "suppress the backlog alarm while rate-limited" at the only
// place that alarm exists (the badge); /status keeps reporting
// unackedBacklog as a field regardless, since the backlog is real, just
// expected.
func sessionStatusDisplay(alive, stuck, unackedBacklog, draining, rateLimited bool, rateLimitResetsAt string) string {
	if rateLimited {
		if rateLimitResetsAt != "" {
			return "⏳ usage limit (resets " + rateLimitResetsAt + ")"
		}
		return "⏳ usage limit"
	}
	if !alive {
		return "⚫ gone"
	}
	if stuck {
		return "⛔ stuck (permission prompt)"
	}
	if unackedBacklog {
		return "🟡 unacked backlog (>30min)"
	}
	if draining {
		return "🌀 draining"
	}
	return "🟢 alive"
}

// contextPctSuffix returns " (N%)" for a known context-usage report, or ""
// when pct is nil (never reported) — appended next to the status wherever
// it's shown (dashboard Sessions table, session pages, `tn agents`), per
// SPEC-generations.md's daemon-managed auto-drain.
func contextPctSuffix(pct *float64) string {
	if pct == nil {
		return ""
	}
	return fmt.Sprintf(" (%.0f%%)", *pct)
}

// startStuckDetector launches a background goroutine that inspects every
// alive orchestrator's tmux pane: once after stuckCheckInitialDelay (so a
// prompt appearing around a binary self-restart isn't stuck waiting for the
// full steady-state interval), then every stuckCheckInterval. Disabled
// entirely when TN_NO_STUCKCHECK=1 (tests and smoke runs that start the
// real daemon should set this) — that turns off DETECTION itself. To keep
// detection but drop only the osascript popup, see dialogFuncForEnv/
// TN_NO_DIALOG instead.
func (s *Server) startStuckDetector() {
	if os.Getenv("TN_NO_STUCKCHECK") == "1" {
		return
	}
	dialog := dialogFuncForEnv()
	go func() {
		time.Sleep(stuckCheckInitialDelay)
		s.checkStuckSessionsOnce(capturePaneReal, dialog, sendKeysReal)
		s.checkOrphanedDeathsOnce()
		s.sweepUnroutableMessagesOnce()

		ticker := time.NewTicker(stuckCheckInterval)
		defer ticker.Stop()
		for range ticker.C {
			s.checkStuckSessionsOnce(capturePaneReal, dialog, sendKeysReal)
			s.checkOrphanedDeathsOnce()
			s.sweepUnroutableMessagesOnce()
		}
	}()
}

// dialogFuncForEnv returns dialogReal, or — when TN_NO_DIALOG=1 — a no-op
// that always returns ("", nil), the same "no selection" outcome
// updateStuckState already treats as do-nothing (dismiss/timeout). This is
// the daemon's ONLY Apple Events dependency (osascript); every other piece
// of stuck-prompt handling — Stuck/UnackedBacklog state, /status.stuckPrompts, the
// dashboard flag, and the session-page excerpt — is computed and recorded
// in updateStuckState BEFORE dialog is ever called, so suppressing it here
// changes nothing about what the daemon detects or reports. The SwiftBar
// tray's Attach / Allow once / Deny actions (POST /agents/approve, see
// handleApprove) already cover the exact same response surface without
// Apple Events, which is what makes the dialog redundant rather than load-
// bearing. Defaults to the real dialog (unset/anything else = enabled);
// the LaunchAgent plist sets TN_NO_DIALOG=1 by default on this machine.
func dialogFuncForEnv() dialogFunc {
	if os.Getenv("TN_NO_DIALOG") == "1" {
		return dialogSuppressed
	}
	return dialogReal
}

// dialogSuppressed is TN_NO_DIALOG=1's dialogFunc — a named top-level
// function (not an inline closure) specifically so tests can verify
// dialogFuncForEnv's return value by identity (reflect.ValueOf(...).
// Pointer()) without ever having to actually CALL the returned function to
// find out which one it is — calling the wrong one by accident in a test
// would mean actually invoking dialogReal's osascript on whatever machine
// runs the suite.
func dialogSuppressed(title, text string, buttons []string) (string, error) {
	return "", nil
}

// checkStuckSessionsOnce is one detector pass, factored out of the ticker
// loop so it's directly testable with injected capture/dialog/sendKeys.
// Inspects every REGISTERED (not deregistered) agent with a known tmux
// session (see tmuxSessionForAgent) — not just orchestrator-<slug>-named
// ones, since generation-aware agents report their own session and
// needn't follow that naming convention.
//
// Deliberately NOT gated on liveness (agentAliveAt/aliveWindow) anywhere
// in this function — that used to be the top-of-loop gate, and it made
// the detector blind in exactly the case it exists for: a session
// blocked on an interactive permission dialog stops polling its inbox
// (that IS what "not alive" measures — see aliveWindow's own doc
// comment), so liveness is a SYMPTOM of being stuck, not an independent
// precondition for checking. Gating capture on it meant the longer a
// session stayed blocked, the more certain the detector was to stop
// looking at it. Capture runs for every known session regardless;
// alerting (recording Stuck/Excerpt, showing the dialog) is what's
// gated, and it's gated on DeregisteredAt instead — a death path
// (finishRetire, reconcileSleepKilledAgent, checkOrphanedDeathsOnce) is
// what should silence a pane, not a stale heartbeat. This deliberately
// does NOT kill the tmux session for a dead agent (only finishRetire's
// explicit retire flow does that) — a deregistered agent's scrollback
// stays real forensic evidence (this exact investigation used it
// repeatedly), it just stops being alerted on.
func (s *Server) checkStuckSessionsOnce(capture captureFunc, dialog dialogFunc, sendKeys sendKeysFunc) {
	s.mu.Lock()
	snap := s.snapshotLocked()
	s.mu.Unlock()

	now := time.Now()
	for _, a := range snap.Agents {
		session, ok := tmuxSessionForAgent(a)
		if !ok {
			continue
		}

		pane, err := capture(session)
		if err != nil {
			// No tmux session (or capture failed) — nothing to detect.
			continue
		}

		if a.DeregisteredAt != nil {
			// A death path already marked this agent gone. Its pane may
			// still be sitting there (never killed, on purpose — see this
			// function's doc comment) showing whatever it last showed,
			// possibly an old unanswered prompt — that must never alert,
			// and must never re-alert on every subsequent tick either.
			continue
		}

		_, stuck := detectLiveStuckPrompt(pane)
		unackedBacklog := !stuck && oldestDeliveredUnackedAge(a.Name, snap.Messages, now) > unackedBacklogThreshold

		s.updateStuckState(a.Name, session, stuck, unackedBacklog, pane, now, capture, dialog, sendKeys, false)
	}
}

// updateStuckState records this pass's stuck/unackedBacklog result for agentName,
// then shows the approval dialog when either this is a genuinely new
// prompt episode (excerpt hash differs from the last one dialog'd — see
// stuckSessionState.LastPromptedHash) or the re-prompt interval has elapsed
// while stuck on the same prompt. The dedup bookkeeping happens under
// s.stuck's own lock, released before the (potentially minutes-long) dialog
// call. The dialog's buttons are built fresh from THIS pane's parsed
// options (resolveStuckPromptMapping) — a 2-option Yes/No prompt only ever
// offers "Allow once", never a mismapped "Always allow".
//
// stuck (the parameter) is only THIS TICK's raw marker+options detection —
// it does NOT by itself mean the agent is treated as stuck. Fresh
// confirmation (confirmedStuck below — what's allowed to fire a NEW
// dialog) comes from either of two independent signals, biased toward
// alerting when ambiguous (a false positive costs an unnecessary
// notification; a false negative costs a hung agent nobody notices):
//
//   - FAST PATH: the SAME signature on the PREVIOUS tick too, AND the
//     scanned window byte-identical between the two — see
//     stuckSessionState.LastRawExcerpt/PendingExcerptCandidate's doc
//     comment. Confirms in 2 ticks when nothing at all is happening
//     (live-confirmed false positive this rejects: a resolved dialog's
//     text sitting in the window while a running subagent below it has a
//     climbing token count/elapsed timer — g53286).
//   - PERSISTENCE BACKSTOP: the same dialog SIGNATURE seen for
//     stuckPersistenceBackstopTicks consecutive ticks, regardless of
//     whether the raw window changed — see PersistentSignatureCount's
//     doc comment. Catches the fast path's own blind spot: a genuinely
//     blocked agent whose pane keeps changing anyway because it owns a
//     background job still emitting output (ORCHESTRATOR.md). Slower
//     (~10 minutes worst case), but bounded — late, never silent.
//
// Any repaint anywhere in the scanned window — even inside the dialog's
// own text, even just a cosmetic element — costs at least one extra
// confirmation tick under the fast path; there's no special case for
// "but the marker+options matched." The detector cannot tell a cosmetic
// repaint from a background job's real output from captured text alone,
// so it doesn't try. This is believed to cost nothing in the common case:
// every real dialog captured this session — five samples, two
// independent sessions, both team-lead's live examples and this
// session's own throwaway reproductions — was completely static once
// rendered, no self-updating element inside the dialog block itself,
// consistent with Claude's own agent loop (the thing that drives
// elapsed/token displays) being paused while blocked on a modal input
// wait. Five samples is thin, but the fast path doesn't depend on it
// being universally true — it only costs an extra tick if it's ever
// wrong, and the backstop bounds even that.
//
// What's DISPLAYED (Stuck/Excerpt/StuckSince, and whether a stuck->clear
// transition gets logged) is a separate, STICKIER signal than fresh
// confirmation — see displayStuck below.
// hookTriggered is true only when this call originated from
// triggerHookConfirmedStuck (a real PermissionRequest hook POST, re-
// verified against a fresh pane capture) — see confirmedStuck below for
// what it changes and SPEC-serve.md's hook-detection section for why
// pane-scraping's fastConfirm/backstopConfirm paths stay fully active
// regardless: an agent whose hook has never fired (Agent.HookConfirmedAt
// nil) never reaches this call with hookTriggered=true at all, so this
// parameter existing doesn't weaken coverage for anyone pane-scraping
// still has to cover.
func (s *Server) updateStuckState(agentName, session string, stuck, unackedBacklog bool, pane string, now time.Time, capture captureFunc, dialog dialogFunc, sendKeys sendKeysFunc, hookTriggered bool) {
	s.stuck.mu.Lock()
	st, ok := s.stuck.byName[agentName]
	if !ok {
		st = &stuckSessionState{}
		s.stuck.byName[agentName] = st
	}
	wasStuck := st.Stuck

	// Usage-limit detection is fully independent of everything below it
	// (its own wider window, its own markers, its own once-per-episode
	// dialog): a session can be rate-limited while also showing a stale
	// permission prompt, and neither result may gate the other.
	rateResets, rateLimited := detectRateLimitWait(pane)
	wasRateLimited := st.RateLimited
	prevRateResets := st.RateLimitResetsAt
	showRateLimitDialog := false
	if rateLimited {
		if !wasRateLimited {
			st.RateLimitSince = now
		}
		st.RateLimited = true
		st.RateLimitResetsAt = rateResets
		// One dialog per episode; a changed reset string IS a new
		// episode (the limit moved, so the previous notification's
		// headline fact is now wrong).
		if !st.RateLimitDialogShown || st.RateLimitDialogResets != rateResets {
			showRateLimitDialog = true
			st.RateLimitDialogShown = true
			st.RateLimitDialogResets = rateResets
		}
	} else {
		st.RateLimited = false
		st.RateLimitResetsAt = ""
		st.RateLimitSince = time.Time{}
		st.RateLimitDialogShown = false
		st.RateLimitDialogResets = ""
	}

	rawExcerpt := lastNLines(pane, stuckExcerptLines)

	var signature string
	if stuck {
		markerLine, _ := detectLiveStuckPrompt(pane)
		signature = stuckEpisodeSignature(markerLine, parseStuckPromptOptions(pane))
	}
	switch {
	case stuck && signature != "" && signature == st.LastSeenSignature:
		st.PersistentSignatureCount++
	case stuck:
		st.PersistentSignatureCount = 1
	default:
		st.PersistentSignatureCount = 0
	}
	st.LastSeenSignature = signature

	// A cached hook command belongs to WHICHEVER episode it was recorded
	// against (HookCommandSignature) — if that's not the CURRENT signature
	// (a different, resolved episode's leftover, or "" now that nothing's
	// stuck), it must not survive into a later, different prompt that
	// never had one. recordHookCommand may run for THIS episode moments
	// before this exact call (triggerHookConfirmedStuck), so this only
	// clears a MISMATCH, never something just set for the tick in
	// progress.
	if st.HookCommandSignature != "" && st.HookCommandSignature != signature {
		st.HookCommand = ""
		st.HookToolName = ""
		st.HookCommandSignature = ""
	}

	fastConfirm := stuck && st.PendingExcerptCandidate && rawExcerpt == st.LastRawExcerpt
	backstopConfirm := stuck && st.PersistentSignatureCount >= stuckPersistenceBackstopTicks
	// hookConfirm is ANDed with stuck (not a bare hookTriggered) for the
	// same reason sendApprovalIfStillStuck always re-verifies from a fresh
	// capture rather than trusting the moment the hook fired: by the time
	// triggerHookConfirmedStuck's own capture lands, the prompt could
	// already be resolved (a fast auto-response, a human who was already
	// watching). hookTriggered alone would mean the hook path can never be
	// WRONG about there being a marker present — that's not true, only
	// that IT doesn't need a second tick to trust one once it's actually
	// there.
	hookConfirm := stuck && hookTriggered
	confirmedStuck := fastConfirm || backstopConfirm || hookConfirm

	// Comparison logging for the hook-vs-pane-scraping reliability check
	// (SPEC-serve.md): every FRESH confirmation (not a continuing streak)
	// logs which path found it. The acceptance bar is that every pane-
	// detector confirmation should also show up as a hook confirmation,
	// sooner — grep this line for both sources and compare timestamps
	// rather than trusting either mechanism's own self-report.
	if !wasStuck && confirmedStuck {
		source := "pane-fast"
		switch {
		case hookConfirm:
			source = "hook"
		case backstopConfirm && !fastConfirm:
			source = "pane-backstop"
		}
		log.Printf("serve: %s confirmed stuck via %s", agentName, source)
	}

	// Sticky display: once genuinely confirmed (via either path above),
	// a repaint of the SAME signature keeps the PUBLIC Stuck status
	// (and StuckSince/Excerpt) stable instead of flickering clear ->
	// candidate -> clear on every tick something repaints — a background
	// job's own output, a spinner, a statusline element. That flicker
	// would otherwise put a "cleared" log line and a bridge activity
	// entry into the user's feed on every such tick: noise, since a
	// dialog still on screen has not actually cleared. This does NOT
	// change confirmedStuck itself (still requires its own fresh
	// evidence — fastConfirm or the backstop — before firing another
	// dialog; see shouldPrompt below), only what gets DISPLAYED/LOGGED
	// once something is already confirmed. PersistentSignatureCount>=2
	// means THIS tick continues an existing streak of the same
	// signature, not a fresh one — a genuinely DIFFERENT signature
	// resets the count to 1 and correctly breaks stickiness (a new
	// prompt replacing the old one is real progress, not a repaint).
	sameSignatureContinuing := stuck && st.PersistentSignatureCount >= 2
	displayStuck := confirmedStuck || (wasStuck && sameSignatureContinuing)

	st.LastRawExcerpt = rawExcerpt
	st.PendingExcerptCandidate = stuck

	st.Stuck = displayStuck
	st.UnackedBacklog = unackedBacklog

	var excerptHash string
	var excerpt string
	if displayStuck {
		st.Excerpt = buildDisplayExcerpt(pane, st.HookCommand, st.HookToolName, st.HookCommandSignature, signature)
		excerpt = st.Excerpt
		excerptHash = signature
		if !wasStuck {
			st.StuckSince = now
		}
	} else {
		st.Excerpt = ""
		// LastPromptedHash/LastPrompted must be cleared here too, not just
		// Excerpt/Stuck — each detection pass has to be authoritative for
		// the CURRENT state, and leaving a resolved episode's signature in
		// place is exactly what silently disarms the alert path for the
		// NEXT episode: a later prompt of the identical shape (this
		// codebase's own worked example is a recurring worktree-relocation
		// prompt — see this struct's LastPromptedHash doc comment) would
		// compute the SAME excerptHash, so `newEpisode` below would read
		// false even though the agent was genuinely unstuck in between,
		// and the dialog — and tray-alert.sh, which dedupes on this exact
		// signature — would stay silent. Confirmed as a real gap by
		// TestStuckStateSequence_IdenticallyShapedRecurrenceStillReArms
		// (fails without this reset, passes with it).
		st.LastPromptedHash = ""
		st.LastPrompted = time.Time{}
	}
	wentClear := wasStuck && !displayStuck

	newEpisode := confirmedStuck && excerptHash != st.LastPromptedHash
	shouldPrompt := confirmedStuck && (newEpisode || now.Sub(st.LastPrompted) >= stuckDialogRepromptInterval)
	if shouldPrompt {
		st.LastPrompted = now
		st.LastPromptedHash = excerptHash
	}
	s.stuck.mu.Unlock()

	// Make the stuck->clear transition visible rather than an unexplained
	// silence: log it, and put it in the feed via the same activity log
	// everything else (MR merges, retires, orphan notices) already flows
	// through — see checkOrphanedTasks's identical "log against 'bridge',
	// not agentName" reasoning just below for why this is attributed to
	// "bridge" too (appendActivityLocked's touchAgentLocked side effect
	// would otherwise bump agentName's LastSeenAt from this call alone).
	// Usage-limit transitions are logged (not pushed into the activity
	// feed): an episode is self-resolving and typically minutes long, so
	// it belongs in the daemon log next to the stuck transitions, not as
	// two entries in the user's feed every time a limit is hit.
	if rateLimited && (!wasRateLimited || prevRateResets != rateResets) {
		log.Printf("serve: %s usage-limit wait detected (resets %s)", agentName, rateResets)
	}
	if !rateLimited && wasRateLimited {
		log.Printf("serve: %s usage-limit wait cleared", agentName)
	}
	if showRateLimitDialog {
		// Auto-swap (creds.go) runs BEFORE the dialog: the dialog can
		// block for up to 120s and the parked panes should be moving
		// again by then, not waiting on a click.
		title := fmt.Sprintf("Usage limit — %s", agentName)
		text := buildRateLimitDialogText(rateResets)
		if res := s.autoSwapCredentials(agentName, rateResets, now, sendKeys); res != nil {
			if res.swapped {
				title = "Usage limit — credentials swapped"
			}
			text = res.dialogText(agentName, rateResets)
		}
		if _, err := dialog(title, text, []string{"OK"}); err != nil {
			log.Printf("serve: failed to show usage-limit dialog for %s: %v", agentName, err)
		}
	}

	if wentClear {
		log.Printf("serve: %s stuck prompt cleared (session %s)", agentName, session)
		s.mu.Lock()
		s.appendActivityLocked("bridge", fmt.Sprintf("%s's stuck prompt cleared", agentName), "")
		s.saveLocked()
		s.mu.Unlock()
		s.triggerRenders()
	}

	if !shouldPrompt {
		return
	}

	mapping := resolveStuckPromptMapping(pane)
	buttons := stuckDialogButtons(mapping)
	title := fmt.Sprintf("Claude orchestrator stuck: %s", agentName)
	text := buildStuckDialogText(excerpt, session)
	choice, err := dialog(title, text, buttons)
	if err != nil {
		log.Printf("serve: failed to show stuck-session dialog for %s: %v", agentName, err)
		return
	}

	switch choice {
	case "Deny":
		s.sendApprovalIfStillStuck(agentName, session, approvalDeny, capture, sendKeys)
	case "Allow once":
		s.sendApprovalIfStillStuck(agentName, session, approvalAllowOnce, capture, sendKeys)
	case "Always allow":
		s.sendApprovalIfStillStuck(agentName, session, approvalAlwaysAllow, capture, sendKeys)
	default:
		// Dismiss, timeout, or an unrecognized response: do nothing.
	}
}

// stuckApprovalKind identifies which numbered option to send, resolved
// fresh from a re-capture — never a number carried over from dialog time.
type stuckApprovalKind int

const (
	approvalAllowOnce stuckApprovalKind = iota
	approvalAlwaysAllow
	approvalDeny
)

func (k stuckApprovalKind) label() string {
	switch k {
	case approvalAlwaysAllow:
		return "Always allow"
	case approvalDeny:
		return "Deny"
	default:
		return "Allow once"
	}
}

// sendApprovalIfStillStuck re-captures the pane, verifies a marker is
// STILL present, and re-parses the option mapping from that fresh capture
// before sending anything — option numbers can differ between the capture
// shown in the dialog and this one (a different prompt may now be showing),
// so neither the marker presence nor the option number is ever assumed
// stale. The user may also have already resolved it by attaching directly.
// Returns whether a key was actually sent, and — when it wasn't — a short
// machine-readable reason (used by POST /agents/approve's response; the
// dialog-response caller ignores both).
func (s *Server) sendApprovalIfStillStuck(agentName, session string, kind stuckApprovalKind, capture captureFunc, sendKeys sendKeysFunc) (sent bool, reason string) {
	pane, err := capture(session)
	if err != nil {
		log.Printf("serve: stuck-session re-check failed for %s: %v", agentName, err)
		return false, fmt.Sprintf("failed to re-check session: %v", err)
	}
	if _, stillStuck := detectLiveStuckPrompt(pane); !stillStuck {
		log.Printf("serve: stuck-session prompt for %s cleared before send, skipping", agentName)
		return false, "prompt cleared before send"
	}

	mapping := resolveStuckPromptMapping(pane)
	var key string
	switch kind {
	case approvalAlwaysAllow:
		key = mapping.AlwaysAllowKey
	case approvalDeny:
		key = mapping.DenyKey
	default:
		key = mapping.AllowOnceKey
	}
	if key == "" {
		log.Printf("serve: stuck-session prompt for %s no longer offers %q on re-check, skipping", agentName, kind.label())
		return false, fmt.Sprintf("prompt no longer offers %q", kind.label())
	}

	if err := sendKeys(session, key); err != nil {
		log.Printf("serve: failed to send approval keys for %s: %v", agentName, err)
		return false, fmt.Sprintf("failed to send keys: %v", err)
	}
	log.Printf("serve: user approved stuck prompt for %s (option %s)", agentName, kind.label())

	s.mu.Lock()
	s.appendActivityLocked(agentName, fmt.Sprintf("Approved stuck-session prompt: %s", kind.label()), "")
	s.saveLocked()
	s.triggerRenders()
	s.mu.Unlock()

	return true, ""
}

// capturePaneReal captures the visible content of a tmux pane read-only.
func capturePaneReal(session string) (string, error) {
	out, err := exec.Command(resolveTmuxBin(), "capture-pane", "-t", session, "-p").Output()
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// sendKeysReal types keys into a tmux pane followed by Enter, e.g. to
// answer a numbered permission-prompt menu.
func sendKeysReal(session, keys string) error {
	return exec.Command(resolveTmuxBin(), "send-keys", "-t", session, keys, "Enter").Run()
}

// tmuxSessionState is the three-state result of checking whether a tmux
// session currently exists — deliberately NOT a bool. "Unknown" must
// never be treated as "gone" by any caller: see hasTmuxSessionReal's doc
// comment for why the check itself can fail in ways that mean nothing
// about the specific session being asked about.
type tmuxSessionState int

const (
	tmuxSessionUnknown tmuxSessionState = iota
	tmuxSessionExists
	tmuxSessionGone
)

// tmuxSessionExistenceFunc checks whether a named tmux session currently
// exists, three-state (see tmuxSessionState) — distinct from the
// existing boolean tmuxHasSessionFunc used by the spawn path, where "we
// couldn't tell" and "it's not there" are safe to collapse together (the
// worst case is a redundant spawn attempt). Here they must NOT collapse:
// this feeds a decision to reset another agent's owned tasks, and
// "unknown" must never authorise that. Injectable (same convention as
// captureFunc/dialogFunc/sendKeysFunc) so tests never touch a real tmux
// server.
type tmuxSessionExistenceFunc func(session string) tmuxSessionState

// hasTmuxSessionReal checks session existence via `tmux has-session`.
// Three-state rather than boolean because `has-session`'s exit code
// ALONE cannot distinguish "this session doesn't exist" (exit 1, stderr
// "can't find session: ...") from "the tmux server itself isn't
// reachable at all" (exit 1, stderr "error connecting to ... (No such
// file or directory)", e.g. no tmux binary, no server running, a
// permissions error) — verified against the installed tmux (3.6b) on
// this machine. Those two cases mean opposite things here: a session
// genuinely gone is a positive, verifiable fact about THAT agent;
// tmux being unreachable is a fact about this machine right now that
// would read identically for every agent simultaneously — exactly the
// mass-false-positive shape the rest of this mechanism exists to avoid.
// So only an explicit "can't find session" in stderr counts as gone;
// anything else (including any future tmux version's differently-worded
// message) is unknown, and unknown must never authorise a sweep.
func hasTmuxSessionReal(session string) tmuxSessionState {
	var stderr bytes.Buffer
	cmd := exec.Command(resolveTmuxBin(), "has-session", "-t", session)
	cmd.Stderr = &stderr
	if err := cmd.Run(); err == nil {
		return tmuxSessionExists
	}
	if strings.Contains(stderr.String(), "can't find session") {
		return tmuxSessionGone
	}
	return tmuxSessionUnknown
}

// appleScriptQuote escapes a string for embedding in an AppleScript string
// literal.
func appleScriptQuote(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	return s
}

// dialogReal shows a macOS display dialog via osascript with the given
// buttons (built per-prompt by the caller from what the pane actually
// offers — see resolveStuckPromptMapping/stuckDialogButtons), default
// buttons[0] ("Dismiss" by construction), giving up after 120s. Returns the
// clicked button's label, or "" (no error) on a timeout/give-up — the
// caller treats both "Dismiss" and "" as no-ops.
func dialogReal(title, text string, buttons []string) (string, error) {
	quoted := make([]string, len(buttons))
	for i, b := range buttons {
		quoted[i] = `"` + appleScriptQuote(b) + `"`
	}
	script := fmt.Sprintf(
		`display dialog "%s" with title "%s" buttons {%s} default button %s giving up after 120`,
		appleScriptQuote(text), appleScriptQuote(title), strings.Join(quoted, ", "), quoted[0],
	)
	out, err := exec.Command("/usr/bin/osascript", "-e", script).Output()
	if err != nil {
		return "", err
	}

	result := strings.TrimSpace(string(out))
	if strings.Contains(result, "gave up:true") {
		return "", nil
	}
	const marker = "button returned:"
	idx := strings.Index(result, marker)
	if idx == -1 {
		return "", nil
	}
	rest := result[idx+len(marker):]
	if commaIdx := strings.Index(rest, ", gave up:"); commaIdx != -1 {
		rest = rest[:commaIdx]
	}
	return strings.TrimSpace(rest), nil
}

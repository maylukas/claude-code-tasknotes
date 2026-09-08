// Structured task-note body layout (SPEC-note-layout.md, plus its 2026-08-19
// addendum): task Details are no longer a flat append log. Instead they're
// up to six anchored sections — ask, reply, links, brief, description,
// history — that the CLI/daemon rewrite reliably via HTML-comment anchors
// and Obsidian callout syntax (folds in both live preview and reading
// view). Every function in this file is pure (no I/O, no locking) so the
// parse/render/migrate logic is directly unit-testable; callers (api.go's
// Client methods, serve.go's bridgeTransitionTask/autoFlipToInProgress) do
// the GET-then-PUT wiring.
package tn

import (
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode"
)

// noteBody is a task's Details decomposed into its structured blocks.
// Block order when rendered is always: ask, reply, links, brief (each if
// non-empty), Description, history (if any) — see renderNoteBody. Ask/
// Reply/Links/Brief are plain text (no callout formatting, no anchors);
// History is newest-first, each element the full plain-text content of one
// entry (its own "**timestamp — by:** text" header included, since that's
// the atomic unit tn note prepends and migration relocates).
type noteBody struct {
	Ask         string
	Reply       string
	Links       string
	Brief       string
	Description string
	History     []string
}

// tn:ask / tn:reply / tn:links / tn:brief / tn:history anchor comments and
// their callout headers, per SPEC-note-layout.md. "+"/"-" control
// Obsidian's default fold state: ask/reply/links/brief render expanded,
// history collapsed.
const (
	askCalloutHeader   = "[!question]+ Needs you"
	replyCalloutHeader = "[!note]+ Your reply"
	linksCalloutHeader = "[!info]+ Links"
	briefCalloutHeader = "[!tip]+ Current"
)

// defaultReplyBody is the reply block's initial content, written by `tn
// ask` alongside the ask block (see SPEC-note-layout.md's addendum) so a
// question always ships with an obvious place to answer. Left in place by
// `tn ask --clear` — the user may reply on any task, not just one with an
// open question. Deliberately no trailing space after "@claude:" (the spec
// markdown shows one, purely as visual guidance): parseNoteBody TrimSpaces
// every block's content like Ask/Brief already do, so a trailing space
// wouldn't survive the next unrelated CLI write to the same task anyway —
// ending the constant here instead avoids that first-edit-only flicker.
const defaultReplyBody = "Write below this line; start the line with `@claude:` and it routes to the agent.\n\n@claude:"

// historyCalloutHeader returns the history block's callout header with its
// entry count recomputed — always derived from len(entries), never carried
// over from whatever count the stored text happened to say.
func historyCalloutHeader(n int) string {
	return fmt.Sprintf("[!abstract]- History (%d)", n)
}

// legacyEntryHeaderRe matches the start of one legacy (pre-migration) or
// current-format history entry: "**2026-08-18 16:01 — claude:** ...".
// Shared by the migration's legacy-paragraph finder and by
// splitHistoryEntries, since a rendered history block's entries use the
// exact same header shape.
var legacyEntryHeaderRe = regexp.MustCompile(`(?m)^\*\*\d{4}-\d{2}-\d{2} \d{2}:\d{2} — .+?:\*\*`)

// formatHistoryEntry renders one history entry's plain text — the same
// "**timestamp — by:** text" shape `tn note` (and its predecessor
// appendNote) has always used, so legacy bodies and freshly-migrated ones
// are indistinguishable once in a history block.
func formatHistoryEntry(by, text string, now time.Time) string {
	return fmt.Sprintf("**%s — %s:** %s", now.Format("2006-01-02 15:04"), by, text)
}

// renderNoteBody renders nb back into a Details string: ask, reply, links,
// brief blocks (each only if non-empty), Description, history block (if
// len(History) > 0) — each pair of adjacent parts separated by exactly one
// blank line. A block with empty content is omitted entirely, so clearing
// Ask/Reply/Links/Brief (or ending up with zero History entries) cleanly
// removes that block from the rendered body rather than leaving an empty
// shell.
func renderNoteBody(nb noteBody) string {
	var parts []string
	if nb.Ask != "" {
		parts = append(parts, wrapCallout("ask", askCalloutHeader, nb.Ask))
	}
	if nb.Reply != "" {
		parts = append(parts, wrapCallout("reply", replyCalloutHeader, nb.Reply))
	}
	if nb.Links != "" {
		parts = append(parts, wrapCallout("links", linksCalloutHeader, nb.Links))
	}
	if nb.Brief != "" {
		parts = append(parts, wrapCallout("brief", briefCalloutHeader, nb.Brief))
	}
	if desc := strings.TrimRight(nb.Description, "\n"); desc != "" {
		parts = append(parts, desc)
	}
	if len(nb.History) > 0 {
		parts = append(parts, wrapCallout("history", historyCalloutHeader(len(nb.History)), strings.Join(nb.History, "\n\n")))
	}
	return strings.Join(parts, "\n\n")
}

// wrapCallout renders text as an anchored, blockquote-prefixed Obsidian
// callout: `<!-- tn:name -->`, the callout header line, text with every
// line `> `-prefixed (a blank line becomes a bare `>`, per
// SPEC-note-layout.md), then `<!-- /tn:name -->`. The CLI owns this
// formatting; callers always pass plain, un-prefixed text.
func wrapCallout(name, header, text string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "<!-- tn:%s -->\n", name)
	fmt.Fprintf(&b, "> %s\n", header)
	lines := strings.Split(text, "\n")
	for i, line := range lines {
		if line == "" {
			b.WriteString(">")
		} else {
			b.WriteString("> ")
			b.WriteString(line)
		}
		if i < len(lines)-1 {
			b.WriteString("\n")
		}
	}
	fmt.Fprintf(&b, "\n<!-- /tn:%s -->", name)
	return b.String()
}

// unwrapCallout reverses wrapCallout: given the raw text between
// `<!-- tn:name -->` and `<!-- /tn:name -->` (including the leading
// newline and the callout header line), drops the header line and strips
// the `> `/`>` blockquote prefix from every remaining line, returning
// plain text.
func unwrapCallout(inner string) string {
	lines := strings.Split(strings.Trim(inner, "\n"), "\n")
	if len(lines) == 0 {
		return ""
	}
	lines = lines[1:] // drop the callout header line
	out := make([]string, len(lines))
	for i, l := range lines {
		switch {
		case strings.HasPrefix(l, "> "):
			out[i] = l[2:]
		case strings.HasPrefix(l, ">"):
			out[i] = l[1:]
		default:
			out[i] = l
		}
	}
	return strings.Join(out, "\n")
}

// extractBlock finds the first `<!-- tn:name --> ... <!-- /tn:name -->`
// block in body, returning its unwrapped plain-text content and body with
// that block removed (surrounding blank lines collapsed so re-joining the
// remaining pieces doesn't accumulate gaps). found=false (with body
// returned unchanged) if the anchor pair isn't present, or is malformed
// (an opening anchor with no matching close — treated as "not found" so
// the caller's migration-ambiguity bail-out path handles it rather than
// this function guessing).
func extractBlock(body, name string) (content, rest string, found bool) {
	openTag := "<!-- tn:" + name + " -->"
	closeTag := "<!-- /tn:" + name + " -->"

	start := strings.Index(body, openTag)
	if start == -1 {
		return "", body, false
	}
	closeIdx := strings.Index(body[start:], closeTag)
	if closeIdx == -1 {
		return "", body, false
	}
	end := start + closeIdx + len(closeTag)

	inner := body[start+len(openTag) : start+closeIdx]
	content = unwrapCallout(inner)

	before := strings.TrimRight(body[:start], "\n")
	after := strings.TrimLeft(body[end:], "\n")
	switch {
	case before == "":
		rest = after
	case after == "":
		rest = before
	default:
		rest = before + "\n\n" + after
	}
	return content, rest, true
}

// splitHistoryEntries splits a history block's unwrapped content back
// into individual entries. Splitting is done by detecting entry-header
// lines (legacyEntryHeaderRe), NOT by blank lines — an entry's own text
// may legitimately contain a blank line (a multi-paragraph note), and that
// must not be mistaken for the separator between two entries. Any content
// before the first header line is folded into the first entry rather than
// dropped (defensive: better to over-include than to lose text).
func splitHistoryEntries(content string) []string {
	content = strings.TrimSpace(content)
	if content == "" {
		return nil
	}
	lines := strings.Split(content, "\n")
	var entries []string
	var current []string
	for _, l := range lines {
		if legacyEntryHeaderRe.MatchString(l) && len(current) > 0 {
			entries = append(entries, strings.TrimRight(strings.Join(current, "\n"), "\n"))
			current = nil
		}
		current = append(current, l)
	}
	if len(current) > 0 {
		entries = append(entries, strings.TrimRight(strings.Join(current, "\n"), "\n"))
	}
	return entries
}

// parseNoteBody parses body's anchored blocks. ok=false means body has no
// tn: anchors at all — a legacy, unstructured body that the caller should
// route through migrateLegacyBody instead. Any single anchor present
// (even just one of the six) counts as "already structured", matching
// SPEC-note-layout.md's idempotency rule: a second write finds anchors and
// does nothing extra.
func parseNoteBody(body string) (nb noteBody, ok bool) {
	ask, rest, hasAsk := extractBlock(body, "ask")
	reply, rest, hasReply := extractBlock(rest, "reply")
	links, rest, hasLinks := extractBlock(rest, "links")
	brief, rest, hasBrief := extractBlock(rest, "brief")
	historyRaw, rest, hasHistory := extractBlock(rest, "history")

	if !hasAsk && !hasReply && !hasLinks && !hasBrief && !hasHistory {
		return noteBody{}, false
	}

	nb.Ask = strings.TrimSpace(ask)
	nb.Reply = strings.TrimSpace(reply)
	nb.Links = strings.TrimSpace(links)
	nb.Brief = strings.TrimSpace(brief)
	nb.Description = strings.TrimSpace(rest)
	if hasHistory {
		nb.History = splitHistoryEntries(historyRaw)
	}
	return nb, true
}

// doneMeansHeadingRe matches a "DONE MEANS:" (or "DONE MEANS", no colon)
// heading line, case-insensitive, anywhere in a task's Details — this is
// the opt-in marker: a task with no such heading carries no DONE MEANS
// block at all, and checkDoneMeans is a no-op for it (mechanism 2's
// synchronous check and informational note both apply only where someone
// deliberately asked for the friction).
var doneMeansHeadingRe = regexp.MustCompile(`(?im)^\s*DONE MEANS:?\s*$`)

// doneMeansItemRe matches one numbered criterion line immediately
// following a DONE MEANS heading, e.g. "1. MR merged, not just opened".
var doneMeansItemRe = regexp.MustCompile(`^(\d+)\.\s+\S`)

// evidenceLineRe matches "Evidence N:" anywhere in Details — deliberately
// NOT anchored to the start of a line: the normal way evidence gets added
// is `tn note <path> "Evidence 1: ..."`, and formatHistoryEntry renders a
// history entry as "**timestamp — by:** Evidence 1: ..." on one line, so
// "Evidence" is never actually the first thing on that rendered line. Also
// not scoped to any one block (Description, a History entry added later,
// etc. all count), since evidence is often written well after the DONE
// MEANS list itself and the two commonly end up in different parts of the
// note. \b before "Evidence" avoids matching inside an unrelated longer
// word (e.g. "NonEvidence1:").
var evidenceLineRe = regexp.MustCompile(`(?i)\bEvidence\s+(\d+)\s*:`)

// doneMeansCheck is the result of checking a task's Details against its
// own DONE MEANS criteria (if any). See checkDoneMeans's doc comment for
// the honest limit on what this actually verifies.
type doneMeansCheck struct {
	// HasBlock is true iff a DONE MEANS heading was found at all — tasks
	// without one are always Satisfied (nothing was asked for).
	HasBlock bool
	// RequiredCount is the number of numbered criteria found directly
	// under the heading (0 if the heading exists but no parseable
	// numbered items immediately follow it).
	RequiredCount int
	// MissingNumbers lists, in order, which criteria (1-indexed) have no
	// matching "Evidence N:" line anywhere in Details.
	MissingNumbers []int
}

// Satisfied reports whether every numbered DONE MEANS criterion has a
// matching Evidence line — vacuously true when there's no DONE MEANS
// block, or a block with zero parseable criteria.
func (c doneMeansCheck) Satisfied() bool {
	return len(c.MissingNumbers) == 0
}

// checkDoneMeans scans details for a "DONE MEANS:" heading followed by a
// contiguous run of numbered criteria (1., 2., 3., ... — stops at the
// first blank line, non-numbered line, or break in the sequence), then
// checks that every one of those N numbers has a corresponding
// "Evidence N:" line somewhere else in details.
//
// THE HONEST LIMIT, stated here rather than only in a message elsewhere:
// this is a MECHANICAL count, not a quality check. It verifies that N
// numbered criteria have N correspondingly-numbered Evidence lines — it
// cannot and does not verify that the evidence actually supports the
// criterion. "Evidence 1: done" passes this check exactly as well as a
// paragraph of genuine proof would. This forces the ACT of writing
// evidence, which is real friction against a bare status flip, but it is
// a floor, not a guarantee — a human or reviewer still has to actually
// read what was written. Do not mistake a passing check for a verified
// close.
func checkDoneMeans(details string) doneMeansCheck {
	loc := doneMeansHeadingRe.FindStringIndex(details)
	if loc == nil {
		return doneMeansCheck{}
	}

	n := 0
	// loc[1] lands right before the heading line's own trailing newline
	// ($ in multiline mode doesn't consume it) — strip exactly that one
	// newline so the first split segment is the first criterion line, not
	// an empty string that would otherwise end the scan immediately.
	afterHeading := strings.TrimPrefix(details[loc[1]:], "\n")
	for _, line := range strings.Split(afterHeading, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			break
		}
		m := doneMeansItemRe.FindStringSubmatch(trimmed)
		if m == nil {
			break
		}
		num := 0
		fmt.Sscanf(m[1], "%d", &num)
		if num != n+1 {
			break // non-sequential numbering — stop counting defensively
		}
		n = num
	}
	if n == 0 {
		return doneMeansCheck{HasBlock: true}
	}

	found := map[int]bool{}
	for _, m := range evidenceLineRe.FindAllStringSubmatch(details, -1) {
		num := 0
		fmt.Sscanf(m[1], "%d", &num)
		found[num] = true
	}
	var missing []int
	for i := 1; i <= n; i++ {
		if !found[i] {
			missing = append(missing, i)
		}
	}
	return doneMeansCheck{HasBlock: true, RequiredCount: n, MissingNumbers: missing}
}

// stripWhitespace removes every Unicode whitespace rune from s — used
// only to VERIFY losslessness (never to produce user-visible text): two
// strings with the same non-whitespace characters in the same order
// stripWhitespace-equal, regardless of exactly how blank lines/spacing
// were normalized in between.
func stripWhitespace(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if !unicode.IsSpace(r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// migrateLegacyBody implements SPEC-note-layout.md's migration: body has
// no tn: anchors, so find legacy note paragraphs (lines matching
// legacyEntryHeaderRe) and move them, newest-first, into History, leaving
// everything before the first match as Description. ok=false means the
// migration could not be VERIFIED lossless (stripped-whitespace content of
// description+entries doesn't reconstruct to exactly the original body's
// stripped-whitespace content) — the caller must then bail out per spec:
// leave body untouched and append the new entry to a fresh history block.
// A body with no legacy paragraphs at all migrates trivially to
// {Description: body} — that's the common case (a plain task, no notes
// yet), not a bail-out.
func migrateLegacyBody(body string) (nb noteBody, ok bool) {
	loc := legacyEntryHeaderRe.FindStringIndex(body)
	if loc == nil {
		return noteBody{Description: strings.TrimSpace(body)}, true
	}

	description := strings.TrimRight(body[:loc[0]], "\n \t")
	legacyRegion := body[loc[0]:]
	chronological := splitHistoryEntries(legacyRegion)

	// Verify losslessness against the CHRONOLOGICAL (oldest-first) order
	// the entries actually appeared in — that's what the original body's
	// character order is. Checking against the newest-first order (below)
	// would be wrong and would spuriously fail every multi-entry body.
	if !verifyLosslessMigration(description, chronological, body) {
		return noteBody{}, false
	}

	// Only now reverse for the new history block: newest-first.
	entries := append([]string(nil), chronological...)
	for i, j := 0, len(entries)-1; i < j; i, j = i+1, j-1 {
		entries[i], entries[j] = entries[j], entries[i]
	}
	return noteBody{Description: description, History: entries}, true
}

// verifyLosslessMigration reports whether reconstructing description +
// chronologicalEntries (concatenated in that order) accounts for every
// non-whitespace character of original — migrateLegacyBody's safety net
// against ever silently dropping text. Broken out as its own function so
// the guarantee is directly testable in isolation, independent of whether
// any real-world input happens to exercise the failure branch (with this
// package's splitHistoryEntries, every line is preserved and only
// reordered, so in practice this check should never actually fail — but
// the net stays in place rather than trusting that by assumption).
func verifyLosslessMigration(description string, chronologicalEntries []string, original string) bool {
	reconstructed := description
	for _, e := range chronologicalEntries {
		reconstructed += e
	}
	return stripWhitespace(reconstructed) == stripWhitespace(original)
}

// parseOrMigrateNoteBody is the shared entry point for every write path
// (tn note/brief/ask, the daemon's auto-flip, MR-watcher transitions): an
// already-anchored body parses normally (idempotent); an anchor-less body
// runs migrateLegacyBody; if that migration can't be verified lossless,
// the ENTIRE original body becomes Description untouched (empty History)
// — SPEC-note-layout.md's explicit bail-out, so the caller's subsequent
// edit (e.g. prepending a new history entry) lands in a fresh history
// block at the end without disturbing a single character of the original.
func parseOrMigrateNoteBody(details string) noteBody {
	if nb, ok := parseNoteBody(details); ok {
		return nb
	}
	if nb, ok := migrateLegacyBody(details); ok {
		return nb
	}
	return noteBody{Description: details}
}

// looksLikeItHasHistoryContent is a cheap, deliberately permissive check
// for whether details might contain history worth protecting, used ONLY
// to decide whether an unparseable/unmigratable body (parseOrMigrateNoteBody's
// own fallback, which would otherwise silently dump the WHOLE body into
// Description) should refuse a --details write rather than risk
// discarding it — see cmdUpdate in main.go. Matches either a real
// history anchor (already-structured, just failed the OTHER anchors'
// parse for some reason) or a legacy note-entry-shaped paragraph
// (legacyEntryHeaderRe, the same pattern migrateLegacyBody itself looks
// for). A false positive here just means an occasional unnecessary
// refusal; a false negative means silent data loss, so this deliberately
// errs toward matching more often, not less.
func looksLikeItHasHistoryContent(details string) bool {
	if strings.Contains(details, "<!-- tn:history -->") {
		return true
	}
	return legacyEntryHeaderRe.MatchString(details)
}

// applyNoteBodyEdit is the shared write helper behind tn note/tn brief/
// tn ask/tn link and their daemon-side equivalents: parse-or-migrate
// details, let edit mutate the resulting noteBody, and render the result
// back to a Details string.
func applyNoteBodyEdit(details string, edit func(*noteBody)) string {
	nb := parseOrMigrateNoteBody(details)
	edit(&nb)
	return renderNoteBody(nb)
}

// --- Links block (SPEC-note-layout.md addendum 2026-08-19) ---

// parseLinkLines splits a links block's content into individual "- ..."
// line entries, dropping blank lines.
func parseLinkLines(content string) []string {
	var out []string
	for _, l := range strings.Split(content, "\n") {
		if strings.TrimSpace(l) == "" {
			continue
		}
		out = append(out, l)
	}
	return out
}

// formatLinkLine renders one links-block entry: "- <label>: <url>", or
// just "- <url>" when label is empty.
func formatLinkLine(label, url string) string {
	if label == "" {
		return "- " + url
	}
	return fmt.Sprintf("- %s: %s", label, url)
}

// linkLineURL extracts the trailing URL token from a rendered link line —
// the last whitespace-delimited field, which is exactly what
// formatLinkLine always produces (a URL never contains whitespace).
func linkLineURL(line string) string {
	fields := strings.Fields(line)
	if len(fields) == 0 {
		return ""
	}
	return fields[len(fields)-1]
}

// upsertLink is `tn link`'s core: appends a new "- <label>: <url>" entry
// to content, or — if an entry for the same URL already exists — replaces
// it in place (same position), so a second `tn link` call for a URL
// already listed updates its label instead of adding a duplicate line.
func upsertLink(content, label, url string) string {
	lines := parseLinkLines(content)
	newLine := formatLinkLine(label, url)
	for i, l := range lines {
		if linkLineURL(l) == url {
			lines[i] = newLine
			return strings.Join(lines, "\n")
		}
	}
	lines = append(lines, newLine)
	return strings.Join(lines, "\n")
}

// setMRLink ensures content's FIRST entry is "- MR: <url>", removing any
// existing entry for the same URL elsewhere in the block first so it's
// never duplicated. Used whenever the mr custom property is set via `tn
// create --mr`/`tn update --mr`, so opening a task always shows the MR
// link without hunting through customProperties — per SPEC-note-layout.md,
// the MR entry is first "whenever that field is set," which this
// re-asserts on every call rather than only the first time.
func setMRLink(content, url string) string {
	var kept []string
	for _, l := range parseLinkLines(content) {
		if linkLineURL(l) == url {
			continue
		}
		kept = append(kept, l)
	}
	lines := append([]string{formatLinkLine("MR", url)}, kept...)
	return strings.Join(lines, "\n")
}

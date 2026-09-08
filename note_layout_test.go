package main

import (
	"sort"
	"strings"
	"testing"
	"time"
)

func mustNoteTime(t *testing.T, s string) time.Time {
	t.Helper()
	tm, err := time.Parse("2006-01-02 15:04", s)
	if err != nil {
		t.Fatal(err)
	}
	return tm
}

// TestRenderNoteBody_BlockOrder verifies the fixed rendering order: ask,
// reply, links, brief, description, history — every combination of
// present/absent blocks, and that an empty block is omitted entirely
// rather than leaving an empty shell.
func TestRenderNoteBody_BlockOrder(t *testing.T) {
	nb := noteBody{
		Ask:         "Merge MR !973 or reply.",
		Reply:       defaultReplyBody,
		Links:       "- MR: https://gitlab.com/x/y/-/merge_requests/973",
		Brief:       "Fix verified on VM4.",
		Description: "Original task description.",
		History:     []string{"**2026-08-18 16:01 — claude:** did the thing"},
	}
	got := renderNoteBody(nb)

	askIdx := strings.Index(got, "<!-- tn:ask -->")
	replyIdx := strings.Index(got, "<!-- tn:reply -->")
	linksIdx := strings.Index(got, "<!-- tn:links -->")
	briefIdx := strings.Index(got, "<!-- tn:brief -->")
	descIdx := strings.Index(got, "Original task description.")
	historyIdx := strings.Index(got, "<!-- tn:history -->")
	if askIdx == -1 || replyIdx == -1 || linksIdx == -1 || briefIdx == -1 || descIdx == -1 || historyIdx == -1 {
		t.Fatalf("expected all six sections present, got:\n%s", got)
	}
	if !(askIdx < replyIdx && replyIdx < linksIdx && linksIdx < briefIdx && briefIdx < descIdx && descIdx < historyIdx) {
		t.Errorf("expected order ask < reply < links < brief < description < history, got indices %d,%d,%d,%d,%d,%d:\n%s",
			askIdx, replyIdx, linksIdx, briefIdx, descIdx, historyIdx, got)
	}

	// Omitted blocks leave no trace.
	onlyDesc := renderNoteBody(noteBody{Description: "Just a description."})
	for _, tag := range []string{"tn:ask", "tn:reply", "tn:links", "tn:brief", "tn:history"} {
		if strings.Contains(onlyDesc, tag) {
			t.Errorf("expected no %s anchor when only description is set, got:\n%s", tag, onlyDesc)
		}
	}
	if onlyDesc != "Just a description." {
		t.Errorf("expected the bare description unchanged, got %q", onlyDesc)
	}
}

// TestRoundTrip_EachBlockIndependently verifies render→parse round-trips
// exactly for ask alone, brief alone, history alone, and all three
// together (with a description). A description-ONLY body is deliberately
// NOT included here — with no anchors at all it's indistinguishable from
// a legacy body, so parseNoteBody correctly reports ok=false for it (see
// TestParseNoteBody_UnanchoredReturnsNotOK); that's by design, not a gap.
func TestRoundTrip_EachBlockIndependently(t *testing.T) {
	cases := []noteBody{
		{Ask: "Merge MR !973 to close this."},
		{Reply: defaultReplyBody},
		{Links: "- MR: https://gitlab.com/x/y/-/merge_requests/973"},
		{Brief: "Fix verified on VM4."},
		{History: []string{"**2026-08-18 16:01 — claude:** did a thing"}},
		{
			Ask:         "Merge MR !973 or reply with @claude: <instruction>.",
			Reply:       defaultReplyBody,
			Links:       "- MR: https://gitlab.com/x/y/-/merge_requests/973\n- Artifact: [[Claude Artifacts/screenshot.png]]",
			Brief:       "Fix verified on VM4.",
			Description: "Original task description.",
			History: []string{
				"**2026-08-18 16:01 — apk-build-verify:** newest entry",
				"**2026-08-18 13:59 — claude:** older entry",
			},
		},
	}
	for i, nb := range cases {
		rendered := renderNoteBody(nb)
		got, ok := parseNoteBody(rendered)
		if !ok {
			t.Fatalf("case %d: expected parseNoteBody to recognize the rendered body as structured, got body:\n%s", i, rendered)
		}
		if got.Ask != nb.Ask {
			t.Errorf("case %d: Ask round-trip: got %q, want %q", i, got.Ask, nb.Ask)
		}
		if got.Reply != nb.Reply {
			t.Errorf("case %d: Reply round-trip: got %q, want %q", i, got.Reply, nb.Reply)
		}
		if got.Links != nb.Links {
			t.Errorf("case %d: Links round-trip: got %q, want %q", i, got.Links, nb.Links)
		}
		if got.Brief != nb.Brief {
			t.Errorf("case %d: Brief round-trip: got %q, want %q", i, got.Brief, nb.Brief)
		}
		if got.Description != nb.Description {
			t.Errorf("case %d: Description round-trip: got %q, want %q", i, got.Description, nb.Description)
		}
		if len(got.History) != len(nb.History) {
			t.Fatalf("case %d: History length round-trip: got %d, want %d (got=%v)", i, len(got.History), len(nb.History), got.History)
		}
		for j := range nb.History {
			if got.History[j] != nb.History[j] {
				t.Errorf("case %d entry %d: got %q, want %q", i, j, got.History[j], nb.History[j])
			}
		}
	}
}

// TestRoundTrip_MultilineAndBlockquoteText verifies history/ask/brief text
// containing embedded newlines (multi-paragraph) and literal "&gt;"
// characters survives an exact round trip.
func TestRoundTrip_MultilineAndBlockquoteText(t *testing.T) {
	ask := "First line.\n\nSecond paragraph with a quote: > like this."
	nb := noteBody{
		Ask: ask,
		History: []string{
			"**2026-08-18 16:01 — claude:** Line one.\nLine two.\n\nA new paragraph.\n> A quoted line inside the note.",
		},
	}
	rendered := renderNoteBody(nb)
	got, ok := parseNoteBody(rendered)
	if !ok {
		t.Fatalf("expected structured parse, got:\n%s", rendered)
	}
	if got.Ask != ask {
		t.Errorf("Ask multiline round-trip failed:\ngot:  %q\nwant: %q", got.Ask, ask)
	}
	if len(got.History) != 1 || got.History[0] != nb.History[0] {
		t.Errorf("History multiline round-trip failed:\ngot:  %q\nwant: %q", got.History, nb.History[0])
	}
}

// TestHistoryBlock_NewestFirstAndRecomputedCount verifies tn note's
// prepend behavior (via applyNoteBodyEdit) puts new entries first and the
// header count always matches len(History), never a stale carried-over
// number.
func TestHistoryBlock_NewestFirstAndRecomputedCount(t *testing.T) {
	now := mustNoteTime(t, "2026-08-18 16:01")
	details := ""
	for i, text := range []string{"first note", "second note", "third note"} {
		by := "claude"
		entry := formatHistoryEntry(by, text, now.Add(time.Duration(i)*time.Minute))
		details = applyNoteBodyEdit(details, func(nb *noteBody) {
			nb.History = append([]string{entry}, nb.History...)
		})
	}

	if !strings.Contains(details, "History (3)") {
		t.Errorf("expected a recomputed count of 3, got:\n%s", details)
	}
	nb, ok := parseNoteBody(details)
	if !ok {
		t.Fatalf("expected structured parse, got:\n%s", details)
	}
	if len(nb.History) != 3 {
		t.Fatalf("expected 3 history entries, got %d: %v", len(nb.History), nb.History)
	}
	if !strings.Contains(nb.History[0], "third note") {
		t.Errorf("expected the newest entry (third note) first, got %q", nb.History[0])
	}
	if !strings.Contains(nb.History[2], "first note") {
		t.Errorf("expected the oldest entry (first note) last, got %q", nb.History[2])
	}
}

// TestParseNoteBody_UnanchoredReturnsNotOK verifies a plain legacy body (no
// tn: anchors at all) is correctly identified as needing migration.
func TestParseNoteBody_UnanchoredReturnsNotOK(t *testing.T) {
	_, ok := parseNoteBody("Just a plain description, no notes.")
	if ok {
		t.Error("expected ok=false for an unanchored body")
	}
}

// TestMigrateLegacyBody_MovesEntriesNewestFirst verifies a realistic
// legacy flat-append body (description + 3 chronological notes) migrates
// with the description intact and entries reversed to newest-first.
func TestMigrateLegacyBody_MovesEntriesNewestFirst(t *testing.T) {
	legacy := "Original task description.\n\n" +
		"**2026-08-17 09:00 — claude:** started work\n\n" +
		"**2026-08-17 14:30 — claude:** made progress\n\n" +
		"**2026-08-18 10:00 — claude:** finished"

	nb, ok := migrateLegacyBody(legacy)
	if !ok {
		t.Fatalf("expected a successful (non-bail-out) migration")
	}
	if nb.Description != "Original task description." {
		t.Errorf("expected the description preserved, got %q", nb.Description)
	}
	if len(nb.History) != 3 {
		t.Fatalf("expected 3 migrated entries, got %d: %v", len(nb.History), nb.History)
	}
	if !strings.Contains(nb.History[0], "finished") {
		t.Errorf("expected the newest entry (finished) first, got %q", nb.History[0])
	}
	if !strings.Contains(nb.History[1], "made progress") {
		t.Errorf("expected the middle entry second, got %q", nb.History[1])
	}
	if !strings.Contains(nb.History[2], "started work") {
		t.Errorf("expected the oldest entry (started work) last, got %q", nb.History[2])
	}
}

// TestMigrateLegacyBody_Idempotent verifies SPEC-note-layout.md's
// idempotency rule via the full write path: migrating, then writing again
// through applyNoteBodyEdit, finds the now-anchored body and does nothing
// extra to the already-migrated entries (only the new prepend changes).
func TestMigrateLegacyBody_Idempotent(t *testing.T) {
	legacy := "Original description.\n\n**2026-08-17 09:00 — claude:** first note"

	firstWrite := applyNoteBodyEdit(legacy, func(nb *noteBody) {
		nb.History = append([]string{formatHistoryEntry("claude", "second note", mustNoteTime(t, "2026-08-18 10:00"))}, nb.History...)
	})
	nb1, ok := parseNoteBody(firstWrite)
	if !ok || len(nb1.History) != 2 {
		t.Fatalf("expected migration + prepend to produce 2 entries, got ok=%v history=%v", ok, nb1.History)
	}

	// Second write: body is now anchored, so this must be a normal parse
	// (no re-migration) — description and existing entries stay exactly
	// as they are, only the new entry is added.
	secondWrite := applyNoteBodyEdit(firstWrite, func(nb *noteBody) {
		nb.History = append([]string{formatHistoryEntry("claude", "third note", mustNoteTime(t, "2026-08-18 11:00"))}, nb.History...)
	})
	nb2, ok := parseNoteBody(secondWrite)
	if !ok || len(nb2.History) != 3 {
		t.Fatalf("expected 3 entries after the second write, got ok=%v history=%v", ok, nb2.History)
	}
	if nb2.Description != nb1.Description {
		t.Errorf("expected the description untouched by the second (already-anchored) write, got %q vs %q", nb2.Description, nb1.Description)
	}
	// The original two entries must still be present, byte-for-byte.
	if nb2.History[1] != nb1.History[0] || nb2.History[2] != nb1.History[1] {
		t.Errorf("expected the pre-existing entries preserved verbatim, got %v vs %v", nb2.History, nb1.History)
	}
}

// sortedRuneMultiset returns s's non-whitespace runes sorted — a
// reordering-tolerant way to compare "same characters, possibly
// rearranged" without requiring exact positional equality (migration
// legitimately REORDERS history entries to newest-first, so the naive
// stripWhitespace(a) == stripWhitespace(b) check would spuriously fail for
// any multi-entry body even when nothing was lost).
func sortedRuneMultiset(s string) []rune {
	r := []rune(stripWhitespace(s))
	sort.Slice(r, func(i, j int) bool { return r[i] < r[j] })
	return r
}

// TestMigrateLegacyBody_Lossless is the explicit provably-lossless check
// the spec calls for: the migrated output (description + every history
// entry) contains every character of the original body — same characters,
// same counts, order aside (entries are deliberately reordered
// newest-first, so this checks the character MULTISET, not positional
// equality; verifyLosslessMigration itself — tested directly below —
// checks the stronger chronological-order equality before any reversal
// happens).
func TestMigrateLegacyBody_Lossless(t *testing.T) {
	legacy := "A description with **bold** and _weird_ chars: <>&\"'.\n\n" +
		"**2026-08-17 09:00 — claude:** note one with a > blockquote char\n\n" +
		"**2026-08-17 14:30 — bob:** note two\nwith an embedded newline\n\n" +
		"**2026-08-18 10:00 — claude:** note three"

	nb, ok := migrateLegacyBody(legacy)
	if !ok {
		t.Fatalf("expected a successful migration")
	}

	reconstructed := nb.Description
	for _, e := range nb.History {
		reconstructed += e
	}
	gotSet, wantSet := sortedRuneMultiset(reconstructed), sortedRuneMultiset(legacy)
	if len(gotSet) != len(wantSet) || string(gotSet) != string(wantSet) {
		t.Fatalf("migration is NOT lossless (character multiset mismatch):\noriginal:      %q\nreconstructed: %q", string(wantSet), string(gotSet))
	}

	// Belt and suspenders: every individual legacy note's distinguishing
	// text is present somewhere in the migrated output.
	full := nb.Description + " " + strings.Join(nb.History, " ")
	for _, mustContain := range []string{
		"A description with **bold**",
		"note one with a > blockquote char",
		"note two",
		"with an embedded newline",
		"note three",
	} {
		if !strings.Contains(full, mustContain) {
			t.Errorf("expected migrated output to contain %q, got:\n%s", mustContain, full)
		}
	}
}

// TestVerifyLosslessMigration tests migrateLegacyBody's safety net in
// isolation: a genuinely lossless (description + chronological entries)
// reconstruction passes, and a reconstruction that's missing text — the
// exact shape of corruption the net exists to catch — is rejected. This
// implementation's splitHistoryEntries preserves every line unconditionally
// (only regrouping, never dropping), so migrateLegacyBody's bail-out
// branch is not reachable through any real input in this package; this
// test instead proves the safety mechanism ITSELF correctly distinguishes
// lossless from lossy, so the net is trustworthy if it's ever needed
// (e.g. after a future change to splitHistoryEntries).
func TestVerifyLosslessMigration(t *testing.T) {
	if !verifyLosslessMigration("desc", []string{"entry1", "entry2"}, "descentry1entry2") {
		t.Error("expected a genuinely lossless reconstruction to be accepted")
	}
	if !verifyLosslessMigration("  desc  \n", []string{"entry1"}, "desc\nentry1") {
		t.Error("expected whitespace-only differences to still be accepted")
	}
	if verifyLosslessMigration("desc", []string{"entry1"}, "desc entry1 entry2-that-got-dropped") {
		t.Error("expected a reconstruction missing text to be REJECTED")
	}
	if verifyLosslessMigration("desc", []string{"entryX"}, "desc entry1") {
		t.Error("expected a reconstruction with substituted/corrupted text to be REJECTED")
	}
}

// TestMigrateLegacyBody_TrivialNoLegacyEntries verifies a body with no
// legacy note paragraphs at all (a plain task, never noted) migrates to a
// pure description with no history — not a bail-out.
func TestMigrateLegacyBody_TrivialNoLegacyEntries(t *testing.T) {
	nb, ok := migrateLegacyBody("Just a plain task description, never noted.")
	if !ok {
		t.Fatal("expected a successful (trivial) migration")
	}
	if nb.Description != "Just a plain task description, never noted." || len(nb.History) != 0 {
		t.Errorf("unexpected result: %+v", nb)
	}
}

// TestParseOrMigrateNoteBody_BailOutContract verifies parseOrMigrateNoteBody's
// documented contract for migrateLegacyBody reporting ok=false: the ENTIRE
// original text becomes Description untouched, empty History — so a
// caller's subsequent edit (a new history entry) lands in a fresh block
// without disturbing a single character of the original. Pinned directly
// against the source (rather than trying to organically trigger the
// bail-out through migrateLegacyBody, which this package's
// splitHistoryEntries makes structurally unreachable — see
// TestVerifyLosslessMigration for the safety net's own correctness).
func TestParseOrMigrateNoteBody_BailOutContract(t *testing.T) {
	const weird = "Some body text that happens to have unbalanced <!-- tn:ask --> markup in it."
	// This body has an unmatched opening anchor with no closing tag —
	// extractBlock treats that as "not found" (see its doc comment), so
	// parseNoteBody correctly reports ok=false and this falls through to
	// migration. It contains no legacy-entry-shaped header, so
	// migrateLegacyBody's trivial (non-bail-out) path applies: the whole
	// string becomes Description as-is. This confirms malformed-but-
	// harmless markup is never mistaken for an ambiguous migration.
	_, ok := parseNoteBody(weird)
	if ok {
		t.Fatalf("expected an unmatched opening anchor to NOT be treated as structured, got ok=true")
	}
	nb := parseOrMigrateNoteBody(weird)
	if nb.Description != weird || len(nb.History) != 0 {
		t.Errorf("expected the whole body preserved as Description with no history, got %+v", nb)
	}

	// Applying an edit on top must land the new entry in a fresh block
	// without touching the original text.
	result := applyNoteBodyEdit(weird, func(nb *noteBody) {
		nb.History = append([]string{formatHistoryEntry("claude", "a new note", mustNoteTime(t, "2026-08-18 10:00"))}, nb.History...)
	})
	if !strings.Contains(result, weird) {
		t.Errorf("expected the original text preserved verbatim, got:\n%s", result)
	}
	if !strings.Contains(result, "a new note") {
		t.Errorf("expected the new entry appended in a fresh history block, got:\n%s", result)
	}
}

// TestApplyNoteBodyEdit_EmptyDetails verifies the very first write to a
// brand-new task (empty Details) produces a clean single-block body.
func TestApplyNoteBodyEdit_EmptyDetails(t *testing.T) {
	result := applyNoteBodyEdit("", func(nb *noteBody) { nb.Brief = "Starting work." })
	if !strings.Contains(result, "<!-- tn:brief -->") || !strings.Contains(result, "Starting work.") {
		t.Errorf("unexpected result for empty details: %q", result)
	}
	nb, ok := parseNoteBody(result)
	if !ok || nb.Brief != "Starting work." || nb.Description != "" {
		t.Errorf("unexpected parse: ok=%v nb=%+v", ok, nb)
	}
}

// TestApplyNoteBodyEdit_ClearingAskRemovesBlock verifies setting Ask back
// to "" removes the block from the rendered body entirely (tn ask
// --clear's behavior).
func TestApplyNoteBodyEdit_ClearingAskRemovesBlock(t *testing.T) {
	withAsk := applyNoteBodyEdit("Description.", func(nb *noteBody) { nb.Ask = "Merge or reply?" })
	if !strings.Contains(withAsk, "tn:ask") {
		t.Fatalf("expected an ask block present, got:\n%s", withAsk)
	}
	cleared := applyNoteBodyEdit(withAsk, func(nb *noteBody) { nb.Ask = "" })
	if strings.Contains(cleared, "tn:ask") {
		t.Errorf("expected the ask block removed after clearing, got:\n%s", cleared)
	}
	if !strings.Contains(cleared, "Description.") {
		t.Errorf("expected the description preserved after clearing ask, got:\n%s", cleared)
	}
}

// TestApplyNoteBodyEdit_BriefReplaces verifies tn brief REPLACES (not
// appends to) the brief block on a second call.
func TestApplyNoteBodyEdit_BriefReplaces(t *testing.T) {
	first := applyNoteBodyEdit("Desc.", func(nb *noteBody) { nb.Brief = "First brief." })
	second := applyNoteBodyEdit(first, func(nb *noteBody) { nb.Brief = "Updated brief." })

	nb, ok := parseNoteBody(second)
	if !ok || nb.Brief != "Updated brief." {
		t.Fatalf("expected the brief replaced, got ok=%v brief=%q", ok, nb.Brief)
	}
	if strings.Contains(second, "First brief.") {
		t.Errorf("expected the old brief text gone entirely (replaced, not appended), got:\n%s", second)
	}
}

// TestUpsertLink_AppendsAndDedupes verifies `tn link`'s core: a new URL is
// appended, and a repeat call for the same URL replaces that entry in
// place (same position) rather than adding a duplicate line.
func TestUpsertLink_AppendsAndDedupes(t *testing.T) {
	content := upsertLink("", "MR", "https://gitlab.com/x/y/-/merge_requests/1")
	if content != "- MR: https://gitlab.com/x/y/-/merge_requests/1" {
		t.Fatalf("unexpected first insert: %q", content)
	}

	content = upsertLink(content, "Artifact", "https://example.com/shot.png")
	wantTwo := "- MR: https://gitlab.com/x/y/-/merge_requests/1\n- Artifact: https://example.com/shot.png"
	if content != wantTwo {
		t.Fatalf("unexpected second insert:\ngot:  %q\nwant: %q", content, wantTwo)
	}

	// Re-adding the first URL with a different label updates in place —
	// still 2 lines, MR still first, label changed.
	content = upsertLink(content, "Merge Request", "https://gitlab.com/x/y/-/merge_requests/1")
	wantUpdated := "- Merge Request: https://gitlab.com/x/y/-/merge_requests/1\n- Artifact: https://example.com/shot.png"
	if content != wantUpdated {
		t.Fatalf("expected dedup-by-URL to update in place:\ngot:  %q\nwant: %q", content, wantUpdated)
	}
	if strings.Count(content, "merge_requests/1") != 1 {
		t.Errorf("expected exactly one entry for the deduped URL, got:\n%s", content)
	}
}

// TestUpsertLink_NoLabel verifies a `tn link` call with no --label renders
// a bare "- <url>" entry.
func TestUpsertLink_NoLabel(t *testing.T) {
	got := upsertLink("", "", "https://example.com/x")
	if got != "- https://example.com/x" {
		t.Errorf("unexpected no-label entry: %q", got)
	}
}

// TestSetMRLink_FirstEntryAndDedup verifies the mr-field auto-entry: the MR
// URL is always the FIRST links-block entry, and re-asserting it (e.g. a
// second `--mr` update with the same URL) doesn't duplicate it or demote
// entries that were added after it.
func TestSetMRLink_FirstEntryAndDedup(t *testing.T) {
	// Existing manual entry, then the mr field gets set: MR becomes first.
	content := "- Artifact: https://example.com/shot.png"
	content = setMRLink(content, "https://gitlab.com/x/y/-/merge_requests/9")
	want := "- MR: https://gitlab.com/x/y/-/merge_requests/9\n- Artifact: https://example.com/shot.png"
	if content != want {
		t.Fatalf("expected MR inserted first:\ngot:  %q\nwant: %q", content, want)
	}

	// Re-observing the same mr URL (e.g. an unrelated --update call) must
	// not duplicate it or reorder the artifact entry.
	content = setMRLink(content, "https://gitlab.com/x/y/-/merge_requests/9")
	if content != want {
		t.Fatalf("expected idempotent re-assertion:\ngot:  %q\nwant: %q", content, want)
	}
	if strings.Count(content, "merge_requests/9") != 1 {
		t.Errorf("expected exactly one MR entry, got:\n%s", content)
	}
}

// TestApplyNoteBodyEdit_AskAlsoWritesReplyUnlessPresent mirrors what
// Client.SetAsk does: writing the ask block also seeds the reply block
// with its default template if absent, but never clobbers an existing
// reply the user may already be mid-typing.
func TestApplyNoteBodyEdit_AskAlsoWritesReplyUnlessPresent(t *testing.T) {
	askWithReply := func(details, text string) string {
		return applyNoteBodyEdit(details, func(nb *noteBody) {
			nb.Ask = text
			if nb.Reply == "" {
				nb.Reply = defaultReplyBody
			}
		})
	}

	first := askWithReply("Desc.", "Merge or reply?")
	nb, ok := parseNoteBody(first)
	if !ok || nb.Reply != defaultReplyBody {
		t.Fatalf("expected the default reply template seeded, got ok=%v reply=%q", ok, nb.Reply)
	}

	// The user has already started replying; a second `tn ask` (a
	// follow-up question) must not overwrite their in-progress reply.
	withUserReply := applyNoteBodyEdit(first, func(nb *noteBody) { nb.Reply = "@claude: partially done, checking one more thing" })
	second := askWithReply(withUserReply, "Also, can you check X?")
	nb2, ok := parseNoteBody(second)
	if !ok || nb2.Reply != "@claude: partially done, checking one more thing" {
		t.Errorf("expected the user's in-progress reply preserved, got ok=%v reply=%q", ok, nb2.Reply)
	}
	if nb2.Ask != "Also, can you check X?" {
		t.Errorf("expected the new ask text set, got %q", nb2.Ask)
	}
}

// TestApplyNoteBodyEdit_ClearingAskLeavesReply verifies `tn ask --clear`
// (Ask set to "") never touches the reply block — the user may reply
// unprompted on any task, per SPEC-note-layout.md's addendum.
func TestApplyNoteBodyEdit_ClearingAskLeavesReply(t *testing.T) {
	withBoth := applyNoteBodyEdit("Desc.", func(nb *noteBody) {
		nb.Ask = "Merge or reply?"
		nb.Reply = defaultReplyBody
	})
	cleared := applyNoteBodyEdit(withBoth, func(nb *noteBody) { nb.Ask = "" })

	nb, ok := parseNoteBody(cleared)
	if !ok {
		t.Fatalf("expected structured parse, got:\n%s", cleared)
	}
	if nb.Ask != "" {
		t.Errorf("expected ask cleared, got %q", nb.Ask)
	}
	if nb.Reply != defaultReplyBody {
		t.Errorf("expected reply block left in place, got %q", nb.Reply)
	}
}

// --- checkDoneMeans (mechanism 2) ---

func TestCheckDoneMeans_NoBlock_AlwaysSatisfied(t *testing.T) {
	check := checkDoneMeans("Just a plain task description, no DONE MEANS heading anywhere.")
	if check.HasBlock {
		t.Errorf("expected HasBlock false, got %+v", check)
	}
	if !check.Satisfied() {
		t.Errorf("expected a task with no DONE MEANS block to always be satisfied, got %+v", check)
	}
}

func TestCheckDoneMeans_AllEvidencePresent_Satisfied(t *testing.T) {
	details := `Some description.

DONE MEANS:
1. MR merged, not just opened
2. Tests pass in CI

Some other text.

Evidence 1: MR !1154 merged 2026-08-25
Evidence 2: CI run #4821 green
`
	check := checkDoneMeans(details)
	if !check.HasBlock || check.RequiredCount != 2 {
		t.Fatalf("expected HasBlock with 2 required criteria, got %+v", check)
	}
	if !check.Satisfied() {
		t.Errorf("expected satisfied with both evidence lines present, got %+v", check)
	}
}

func TestCheckDoneMeans_MissingEvidence_ReportsWhichNumbers(t *testing.T) {
	details := `DONE MEANS:
1. MR merged
2. Tests pass
3. Docs updated

Evidence 1: MR !1154 merged
Evidence 3: README updated in same MR
`
	check := checkDoneMeans(details)
	if check.RequiredCount != 3 {
		t.Fatalf("expected 3 required criteria, got %+v", check)
	}
	if check.Satisfied() {
		t.Errorf("expected unsatisfied — criterion 2 has no evidence, got %+v", check)
	}
	if len(check.MissingNumbers) != 1 || check.MissingNumbers[0] != 2 {
		t.Errorf("expected missing [2], got %v", check.MissingNumbers)
	}
}

// TestCheckDoneMeans_MechanicalCountOnly_QualityNotVerified pins down the
// honest limit documented on checkDoneMeans: "Evidence 1: done" satisfies
// the check exactly as well as a real paragraph of proof. This is
// deliberate — the function is a floor (forces the ACT of writing
// evidence), not a guarantee.
func TestCheckDoneMeans_MechanicalCountOnly_QualityNotVerified(t *testing.T) {
	details := `DONE MEANS:
1. MR merged

Evidence 1: done
`
	check := checkDoneMeans(details)
	if !check.Satisfied() {
		t.Errorf("expected the mechanical count to pass even with a content-free evidence line, got %+v", check)
	}
}

func TestCheckDoneMeans_HeadingWithNoNumberedItems_TreatedAsNoRequirements(t *testing.T) {
	check := checkDoneMeans("DONE MEANS:\n\nNo numbered list follows this heading.")
	if !check.HasBlock {
		t.Errorf("expected HasBlock true (heading was found), got %+v", check)
	}
	if check.RequiredCount != 0 || !check.Satisfied() {
		t.Errorf("expected zero requirements and satisfied, got %+v", check)
	}
}

func TestCheckDoneMeans_ItemListStopsAtBlankLine(t *testing.T) {
	details := `DONE MEANS:
1. First criterion

2. This is NOT counted — a blank line already ended the list

Evidence 1: present
`
	check := checkDoneMeans(details)
	if check.RequiredCount != 1 {
		t.Errorf("expected the list to stop at the first blank line, got RequiredCount=%d", check.RequiredCount)
	}
}

func TestCheckDoneMeans_EvidenceLineFarFromHeading_StillCounts(t *testing.T) {
	// Evidence is commonly written well after the DONE MEANS list — in
	// Description, or added later as a History entry — so the scan must
	// not be scoped to text immediately following the heading.
	details := `DONE MEANS:
1. Reviewed by a second agent

Unrelated paragraph of description text goes here, several lines of it,
well before any evidence shows up.

**2026-08-25 18:30 — bridge:** Evidence 1: reviewed by zz-reviewer, see MR comment
`
	check := checkDoneMeans(details)
	if !check.Satisfied() {
		t.Errorf("expected evidence found regardless of distance from the heading, got %+v", check)
	}
}

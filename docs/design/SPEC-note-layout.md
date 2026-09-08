# Spec: structured task-note layout

Task bodies are currently a flat append log: the original description followed by every
agent note in chronological order. The actionable part (what is true now, what the user
must decide) is buried under history that only matters forensically.

New layout — anchored blocks the CLI can rewrite reliably, using native Obsidian
callouts so folding works in live preview and reading view:

```markdown
<!-- tn:ask -->
> [!question]+ Needs you
> Merge MR !973 to close this, or reply with `@claude: <instruction>`.
<!-- /tn:ask -->

<!-- tn:brief -->
> [!tip]+ Current
> Fix verified on VM4 across both target specs; MR !973 open. One unrelated
> change-password defect found → [[Investigate change-password generic-error defect]].
<!-- /tn:brief -->

<original task description — never touched by the CLI>

<!-- tn:history -->
> [!abstract]- History (7)
> **2026-08-18 16:01 — apk-build-verify:** …
>
> **2026-08-18 13:59 — claude:** …
<!-- /tn:history -->
```

Rules:
- Block order in the body is always: ask (if any), brief (if any), original description,
  history (if any). HTML comment anchors are invisible in reading view and make the
  blocks machine-editable; never nest blocks.
- `[!question]+` and `[!tip]+` render expanded; `[!abstract]-` renders collapsed —
  history stays out of the way until clicked.
- Every line of block content is `> `-prefixed. Blank separator lines inside a callout
  are `>` alone. The CLI owns this formatting; agents pass plain text.
- Newest history entry first (reverse chronological) — the opposite of today, because
  the fold shows the first lines when expanded and recent work matters most.

## CLI changes

- `tn note <path> <text> [--by N]` — unchanged interface, new behavior: prepends the
  entry inside the history block, creating the block if absent. Header count in the
  summary line (`History (7)`) is recomputed on every write.
- `tn brief <path> <text>` — REPLACES the brief block (create if absent). This is the
  "what is true now" line; rewriting is the point.
- `tn ask <path> <text>` — writes the ask block AND sets status `needs-input` in one
  call. Replaces any existing ask block.
- `tn ask --clear <path>` — removes the ask block (used when a question is answered).
- `tn get` — prints ask, brief, description, then history (most recent 5 entries by
  default, `--history N`/`--history 0` for all/more) — so agents see current state
  without re-reading everything, but a `tn note` write is still visibly confirmed on
  the next `tn get` rather than looking like it silently vanished.

## Migration (must be safe on every existing task)

On the first CLI write to a task whose body has no anchors: find legacy note paragraphs
(lines matching `^\*\*\d{4}-\d{2}-\d{2} \d{2}:\d{2} — .+?:\*\*`) and move them, newest
first, into a new history block; leave everything else in place as the description.
Idempotent: a second run finds anchors and does nothing extra. Never drop text — if
parsing is ambiguous, leave the body untouched and append the new entry to a fresh
history block at the end.

## Daemon changes

- The `@claude`-reply auto-flip (needs-input/review → in-progress) also clears the ask
  block and appends a history entry ("user replied: <first 120 chars>").
- `/status` `needsActionTasks[]` gains `"ask"` (ask-block text, plain, ≤200 chars) and
  `"brief"` (≤200 chars) so the menu-bar app and `/ui` can show what is actually being
  asked rather than only the title.
- MR-watcher auto-transitions write their note through the same history path.

## Tests

Block insert/replace/remove round-trips (each block independently, and all three
present); ordering guarantee; history newest-first with recomputed count; multiline and
`>`-containing text survives round-trip; migration of a legacy body (all entries moved,
description intact, second run is a no-op); migration bail-out leaves body untouched;
`tn ask` sets needs-input; auto-flip clears ask + logs history entry; `/status` carries
ask/brief.

Verification: gofmt/vet, `go test ./... -count=1`, rebuild + codesign.

## Addendum 2026-08-19: reply block + links block

Two more blocks, both driven by real friction: the user had nowhere obvious to write a
reply, and MR links were only visible in frontmatter/dashboard, not in the note itself.

Body order becomes: **ask, reply, links, brief, description, history**.

```markdown
<!-- tn:reply -->
> [!note]+ Your reply
> Write below this line; start the line with `@claude:` and it routes to the agent.
>
> @claude: 
<!-- /tn:reply -->

<!-- tn:links -->
> [!info]+ Links
> - MR: https://gitlab.com/…/merge_requests/973
> - Artifact: [[Claude Artifacts/<task>/screenshot.png]]
<!-- /tn:links -->
```

- The reply block is written by `tn ask` (alongside the ask block) so a question always
  ships with a place to answer, and is left in place after `tn ask --clear` — the user
  may reply unprompted on any task. `tn ask --clear` removes only the ask block.
- **The `@claude:` regex MUST be extended to match inside callouts**: lines are
  `> `-prefixed there, so the current line-anchored pattern would never fire on a reply
  written where we tell the user to write it. Accept optional `>`+space repetitions
  before `@claude`. This is a hard requirement — without it the reply block is a trap.
- The links block is maintained by `tn link <path> <url> [--label L]` (append,
  de-duplicated by URL) and automatically carries the `mr` field's URL as the first
  entry whenever that field is set, so opening a task shows the MR without hunting.
- `tn get` prints reply and links alongside ask/brief.

Tests: reply/links round-trip and ordering with all six blocks present; `@claude:`
extraction from a callout-quoted line (and still from a bare line); `tn link` dedupe;
mr-field auto-entry; `tn ask` creates both ask and reply; `tn ask --clear` leaves reply.

## Addendum 2026-08-25: the line-anchored `@claude:` extraction was the truncation bug

The previous addendum's own requirement — extend the regex to match inside callouts —
was implemented correctly, and still hid a defect for a year: the captured group
(`(.+)$`) is line-anchored, and Go's regexp `.` never crosses a newline. wrapCallout
renders a multi-line reply as one blockquoted markdown line PER source line, so only the
very first line of a multi-paragraph reply ever literally started with `@claude:` — every
line after the first newline was invisible to extraction. The note held the complete
text (wrapCallout was never broken); the ROUTED message was permanently cut to the first
paragraph. Confirmed live: an audit of that day's real production messages found this had
silently truncated 10 of 10 multi-line replies from one sender, including a task's
"DONE MEANS" acceptance criteria and a fail-safe design's actual working requirements —
one closed as done regardless, on 204 characters of a much longer directive.

**Fix**: `extractClaudeDirectives` (serve.go) parses the note into its structured blocks
(`parseNoteBody`) first, then captures each `@claude` directive from its marker to the
next marker or block end — but ONLY within `nb.Reply`. Ask/Links/Brief/Description and
any unanchored body deliberately keep the original single-line extraction
(`extractClaudeLines`), because those blocks can legitimately mix a directive with
unrelated trailing content in the SAME block (an `@claude:` line followed by an unrelated
note on the next line), where capturing to block end would swallow that trailing content
into the directive. Reply is the one block safe to capture whole, because its entire
documented purpose — per this spec — is to hold nothing but the answer.

**The boundary lesson, worth keeping independent of this specific bug**: the first draft
of this fix removed the single-line fallback entirely, on the reasoning that an unanchored
body has no blockquote markup to worry about — which is true, but irrelevant, because the
actual risk was never blockquoting. It was that an unanchored body (and Description) can
still mix a directive with unrelated surrounding prose in the SAME text, same as any
callout can. That over-broadened version broke 4 existing dedup tests
(`TestClaudeLineDedup_*`, `TestWebhookClaudeDiffRoutesOncePerHashChange`) whose fixtures
deliberately construct exactly that mixed shape. The lesson: truncation (a boundary drawn
too narrow) and over-capture (a boundary drawn too wide) are the SAME class of mistake in
opposite directions — swallowed content looks like content that was simply always there,
which makes over-capture harder to notice than truncation, not easier. Those four
fixtures existed because someone had already hit the over-capture case once; a test
suite catching a regression toward a PREVIOUSLY-FIXED failure mode, while fixing a
DIFFERENT one, is exactly what a test suite is for — the failure would otherwise have
looked like a working fix all the way to review.

## Addendum 2026-08-26: `tn update --details` was destroying the managed blocks it never touched

HIGH-priority, real data loss, reported by g34005 and reproduced by team-lead before this
was fixed: `tn update <path> --details TEXT` sent the flag's raw text as a LITERAL
wholesale replacement for the task's entire `details` field. `UpdateTask` (`api.go`)'s
partial-update semantics replace a named field outright, not merge into it — true and
correct for `status`/`priority`/arrays, but `cmdUpdate` (`main.go`) was applying that same
"replace the whole field" behavior to `details`, a field this SPEC's own layout treats as
SIX separately-managed blocks, not one opaque string. Every `--details` call silently
destroyed history/ask/reply/links/brief — anything the caller wasn't specifically trying
to touch — with zero error output. The task that surfaced this in production held 102
history entries across 187,869 bytes, accumulated over many retired generations; one
`--details` write would have destroyed all of it irrecoverably. Reproduced on a throwaway
task first (236 bytes/2 entries → 21 bytes/0 entries) rather than on the real one, which
is the only reason that history still exists.

**The fix reuses this file's own machinery rather than inventing new parsing.**
`applyNoteBodyEdit` (parse-or-migrate, mutate, render) is the exact GET-then-PUT pattern
`tn note`/`tn brief`/`tn ask` already use to edit ONE block safely — `cmdUpdate` now routes
`--details` through it too, setting only `nb.Description`, never touching the other five
fields the parsed `noteBody` carries. This is deliberately NOT the same fix as "split the
raw string at the `<!-- tn:history` marker and keep everything after it" (a plausible-
looking first idea): a marker-only split would still let `--details` destroy an existing
ask/reply/links/brief block that happens to sit before the history marker but isn't itself
history — `applyNoteBodyEdit`'s structured parse protects all five blocks uniformly,
because it already has to know about all of them to render the body back correctly.

**`--replace-history` is the required, explicit opt-out**, printing exactly how many
history entries a write is about to remove — never silent again, even through an
intentional escape hatch (SPEC-serve.md's "PermissionRequest hook detection" addendum
records the same "belt-and-braces, don't let it decide, always report the number" pattern
being applied to a very different feature the same week; it's a repeating shape, not a
coincidence).

**A second, narrower safety net was considered and found to be currently inert — kept
anyway, documented honestly as such.** `applyNoteBodyEdit`'s own fallback
(`parseOrMigrateNoteBody`) dumps an unparseable-and-unmigratable body wholesale into
`Description`, which would reproduce the SAME bug for that one shape. `cmdUpdate` refuses
outright rather than risk it when `looksLikeItHasHistoryContent` finds evidence of real
content at stake. But `migrateLegacyBody`'s own bail-out (returning `ok=false`) is already
documented, in THIS file, as **structurally unreachable through its real code path**
(`TestParseOrMigrateNoteBody_BailOutContract`, `note_layout_test.go`) — so this refusal
branch cannot currently be exercised through `cmdUpdate` by any real fixture, only proven
unreachable-today by direct inspection. Rather than ship a test that LOOKS like it proves
the refusal fires while actually just exercising the safe path — the same shape of
misleading-test risk this file's own 2026-08-25 addendum already warns about —
`TestLooksLikeItHasHistoryContent` tests the DETECTOR in isolation, honestly, and the
refusal branch stays in the code as cheap insurance against a future change to
`migrateLegacyBody` reintroducing a real bail-out, not as a currently-exercised path. A
test that cannot honestly be written should not be written to look like it exists anyway.

**Verified the way the incident deserved, not the way a routine fix would be**: the
regression test (`TestCmdUpdate_DetailsPreservesHistoryAndOtherManagedBlocks`,
`main_test.go`) was written to reproduce the destructive behavior FIRST — confirmed
failing against the unfixed code, with a REALISTIC dense fixture (25 real history entries
via `cmdNote`, plus real ask/brief blocks, not one or two synthetic strings — the
Evidence-line-regex lesson about synthetic vs. realistic fixtures, cited directly in the
test's own comment) — then fixed and confirmed passing. Reverting the core fix back to the
literal bug reproduced the exact production symptom (structured body collapsing to the raw
replacement text) on the first try. Live-verified afterward against the real TaskNotes API
on a throwaway smoke-test task, not just the unit suite: 3 real history entries and a
brief block, all written via real `tn note`/`tn brief` calls, all survived a real
`tn update --details` call end to end.

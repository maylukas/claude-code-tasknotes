# Note layout

The structured format `tn` uses for a task's `details` field, and the CLI/daemon operations that read and write it.

## Overview

A task's `details` is decomposed into up to six blocks: **ask, reply, links, brief, description, history**. Each of ask/reply/links/brief/history is wrapped in an HTML comment anchor pair and rendered as a native Obsidian callout, so the block is invisible markup in reading view but still foldable. The description is the original, freeform task text; the CLI never wraps or anchors it.

## Block anchors

Each block (except description) is wrapped as:

```markdown
<!-- tn:NAME -->
> [!TYPE]+/- Header text
> content, blockquote-prefixed
<!-- /tn:NAME -->
```

| Block | Anchor `NAME` | Callout header | Fold state |
|---|---|---|---|
| Ask | `ask` | `[!question]+ Needs you` | Expanded (`+`) |
| Reply | `reply` | `[!note]+ Your reply` | Expanded (`+`) |
| Links | `links` | `[!info]+ Links` | Expanded (`+`) |
| Brief | `brief` | `[!tip]+ Current` | Expanded (`+`) |
| History | `history` | `[!abstract]- History (N)` | Collapsed (`-`) |

The description block carries no anchor and no callout wrapping; it is plain markdown, exactly as a user or agent wrote it.

Every line of a callout's content is `> `-prefixed; a blank line inside the content becomes a bare `>` (no trailing space). Blocks are never nested. The history callout's header count (`History (N)`) is recomputed from the actual number of entries on every write, never carried over from stored text.

## Block order

When rendered, blocks always appear in this order, with exactly one blank line between adjacent parts, and any block with empty content omitted entirely:

1. Ask (if present)
2. Reply (if present)
3. Links (if present)
4. Brief (if present)
5. Description (if non-empty)
6. History (if it has at least one entry)

## Ask block

Set by `tn ask <path> <text>`, which writes the block and sets the task's status to `needs-input` in the same operation, replacing any existing ask block. `tn ask --clear <path>` removes the ask block only; it leaves the reply block in place. The daemon also clears the ask block automatically the moment the user replies via an `@claude:` line (see [Reply block](#reply-block)), in the same status-flip write that flips status back toward `in-progress`.

`tn ask <path> <text>` also writes a reply block if one does not already exist, using [the default reply content](#reply-block), so a question always ships with an obvious place to answer.

## Reply block

Content is the literal text the user (or an agent) writes for a reply. The default content written alongside a new ask block is:

```text
Write below this line; start the line with `@claude:` and it routes to the agent.

@claude:
```

A line beginning with `@claude:` (optionally preceded by `>`/`> ` quote markers, since this text lives inside a callout in the rendered note) is extracted as a directive and routed to an agent. Unlike every other block, the reply block's `@claude:` extraction captures from the marker to the end of the block (not just to the end of one line). Its entire documented purpose is to hold nothing but the answer, so capturing the whole block is safe there in a way it would not be for Ask, Links, Brief, Description, or an unanchored body, where a directive can legitimately share a block with unrelated trailing content and only single-line extraction is used.

The reply block is left in place by `tn ask --clear`; a user may write a reply on any task at any time, not only one currently carrying an open question.

## Links block

Maintained by `tn link <path> <url> [--label L]`: appends a new `- <label>: <url>` entry (or `- <url>` with no label), or, if an entry for that exact URL already exists, replaces it in place (updating its label) rather than duplicating the line. Whenever a task's `mr` custom property is set (via `tn create --mr`/`tn update --mr`), that URL is automatically kept as the block's **first** entry, labeled `MR`. This is re-asserted on every call that touches the field, not only the first time it's set, and any other existing entry for that same URL is removed first so it's never duplicated.

## Brief block

Set by `tn brief <path> <text>`, which **replaces** the entire block's content (create it if absent). This is deliberately a "what is true now" line meant to be overwritten, not appended to.

## Description block

The original task text. Never touched by `tn note`/`tn brief`/`tn ask`/`tn link`. `tn update <path> --details TEXT` writes here; see [`--details` and `--replace-history`](#--details-and---replace-history) below for exactly what that does and does not affect.

## History block

Entries are newest-first (the opposite of a plain chronological log): the callout's fold shows the first lines when expanded, and the most recent work is what matters most at a glance.

Each entry is rendered as a single-line header followed by its text:

```text
**2026-08-18 16:01 — claude:** Fix verified on VM4 across both target specs.
```

`tn note <path> <text> [--by NAME]` prepends one such entry, creating the history block if it doesn't yet exist, and the header count is recomputed. `--by` sets the attribution name in the header (defaults are set by the caller; the CLI does not infer one). The daemon writes through the same mechanism for two other cases: an `@claude:` reply's auto-clear of the ask block also logs a "user replied: \<first 120 chars\>" entry, and an MR-watcher auto-transition (merge detected, MR closed without merge) logs its own note the same way.

### `tn get` and `--history N`

`tn get <path>` (non-JSON output) prints `ask:`/`brief:`/`reply:`/`links:` lines (whichever are present) above the description, followed by the most recent history entries: 5 by default, controlled by `--history N` (`--history 0` shows all). `tn get --json` always returns the complete, unfiltered `details` string regardless of `--history`; the entry-count limit is a non-JSON display convenience only.

## `--details` and `--replace-history`

`tn update <path> --details TEXT` writes only the **description** block by default, leaving ask/reply/links/brief/history exactly as they were. The flag never wholesale-replaces the raw `details` field the way every other `--field` flag on `update` replaces its own field outright. Passing `--replace-history` opts into the old, wholesale-replacement behavior, and when it does, `tn` prints exactly how many history entries the write is about to discard before doing so.

If a task's body cannot be safely parsed or migrated (see [Migration](#migration) below) and appears to hold real history content, `tn update --details` without `--replace-history` refuses the write outright rather than risk silently destroying it.

## Migration

A task's `details` may still be in the pre-anchor, flat-append format: the original description followed by chronological note paragraphs each starting with a `**YYYY-MM-DD HH:MM — by:**` header. The first CLI write to such a task migrates it automatically:

- Every line matching the legacy entry-header pattern is located; everything before the first match becomes the Description block, and the matched region is split into individual entries and moved into a fresh History block, reordered newest-first.
- The migration is verified lossless before being applied: the non-whitespace characters of (description + entries, concatenated in their original chronological order) must reconstruct exactly the non-whitespace characters of the original body. If that check fails for any reason, the migration is abandoned entirely: the original body is left completely untouched, becoming the Description of a body with an otherwise-empty History block, and the CLI's new write (a `tn note` entry, for example) is appended to a fresh history block instead of touching a single character of the original text.
- Migration is idempotent: a body that already has any anchor at all (even just one of the six blocks) is treated as already-structured, and migration is skipped.
- A body with no legacy entry-header lines at all (a plain task with no notes yet) migrates trivially: the whole body becomes the Description, with no History block.

## Related daemon behavior

- `/status`'s `needsActionTasks[]` entries carry each task's `ask` and `brief` text (truncated to 200 characters) alongside the title, so a menu-bar-style client can show what's actually being asked without opening the note.
- `tn get`'s non-JSON view is the only place `--history N` has any effect; every other read path (JSON output, `/status`, the daemon's own internal parsing) always sees the complete block.

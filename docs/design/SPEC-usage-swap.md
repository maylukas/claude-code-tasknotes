# SPEC: usage-API-driven credential auto-swap (daemon 0.10.0)

Status: implementation spec, 2026-09-09. Supersedes the pane-text half of
"Credential profiles & auto-swap" in SPEC-serve.md (the profile store, Keychain
layout and `tn creds` CLI stay as they are).

## Why

Live incident 2026-09-08/09 (see daemon log 19:23–20:37 and `tn-mris-g73379`):

1. Auto-swap installed a profile (`yamlabs`) whose refresh token was already
   revoked (saved 09-03, then the user `/logout`ed — which revokes server-side).
   tn never probed it. Every Claude session on the machine 401'd ("Login
   expired · Please run /login"), including the user's interactive ones.
2. 74 min later the same pane banner re-wrapped (`8:10pm` → `8:10pm
   (Europe/Paris)`); `stuck.go` keys the episode on the raw string, so it
   fired a second, phantom episode, swapped back, and marked `yamlabs` limited
   until the NEXT day 20:10 (stale clock text + 24h rollover in
   `parseResetTime`).
3. The nudge typed into the pane 500 ms after the Keychain write, while
   Claude's Keychain read cache (~30 s, unofficial) still held the old token
   and the session was mid-turn, so the text queued and never ran.

Root cause of all three: tn inferred account state from pane text. The
account state is one authenticated GET away.

## The API (verified live 2026-09-09 against both profiles; strings extracted
## from the installed Claude Code 2.1.266 binary)

```
GET https://api.anthropic.com/api/oauth/usage
Authorization: Bearer <claudeAiOauth.accessToken>
anthropic-beta: oauth-2025-04-20
```

Response (fields we use; everything else ignored):

```json
{
  "five_hour": {"utilization": 3.0, "resets_at": "2026-09-09T11:39:59.886685+00:00"},
  "seven_day": {"utilization": 0.0, "resets_at": "2026-09-11T03:59:59.886710+00:00"},
  "limits": [
    {"kind": "session",       "group": "session", "percent": 3,  "severity": "normal", "resets_at": "…", "scope": null, "is_active": true},
    {"kind": "weekly_all",    "group": "weekly",  "percent": 0,  "severity": "normal", "resets_at": "…", "scope": null, "is_active": false},
    {"kind": "weekly_scoped", "group": "weekly",  "percent": 1,  "severity": "normal", "resets_at": "…", "scope": {"model": {"display_name": "Fable"}}, "is_active": false}
  ],
  "extra_usage": {"is_enabled": false, "disabled_reason": "out_of_credits"}
}
```

`five_hour`/`seven_day` may be `null`; `resets_at` may be `null`; `limits`
may be absent. Parse defensively: prefer `limits[]` (kind `session` /
`weekly_all` / `weekly_scoped`), fall back to `five_hour`/`seven_day`.
`percent` is 0–100+ (can exceed 100). Claude Code itself polls this every
300 s and re-polls just before the earliest `resets_at`.

401 → token invalid/expired. 403/429/5xx → transient, keep last snapshot.

Token refresh (only for profiles tn exclusively owns — see Ownership):

```
POST https://platform.claude.com/v1/oauth/token
Content-Type: application/json
{"grant_type":"refresh_token","refresh_token":"…","client_id":"9d1c250a-e61b-44d9-88ed-5944d1962f5e","scope":"user:inference user:profile"}
→ {"access_token":"…","refresh_token":"…"|null,"expires_in":<seconds>}
```

(`console.anthropic.com/v1/oauth/token` is the older host; 2.1.266 uses
`platform.claude.com`. Make the host a package-level var so a test can point
it at httptest, and so it can be changed without a code hunt.)

## Ownership rule (non-negotiable)

- **Active profile = the live `Claude Code-credentials` item.** Claude Code
  refreshes it and writes back. tn only READS it. tn never refreshes the
  active profile's token: two refreshers racing on one refresh token leave
  one of them dead.
- **Inactive profiles (`tn-credential-profiles` items) are tn's alone.**
  tn refreshes them when `expiresAt` is within 5 min or a fetch 401s, and
  writes the new pair back into the profile item immediately. A refresh
  that fails with 400/401 marks the profile dead.
- Swap keeps its existing order (write live blob back into the outgoing
  profile, then overwrite the Claude item), and additionally: read the live
  item AGAIN immediately before overwriting, so a refresh Claude did between
  the poll and the swap is not lost.

## New package-level pieces (`internal/tn/usage.go`)

```go
type usageClient interface {
    Fetch(ctx context.Context, accessToken string) (*usageSnapshot, int, error) // snapshot, HTTP status, transport error
    Refresh(ctx context.Context, refreshToken string) (access, refresh string, expiresAt time.Time, status int, err error)
}
type httpUsageClient struct{ http *http.Client } // 10s timeout; real impl
```

```go
type usageWindow struct {
    Percent  float64   `json:"percent"`
    ResetsAt time.Time `json:"resetsAt"` // zero when null
}
type usageSnapshot struct {
    Session      usageWindow  `json:"session"`
    Weekly       usageWindow  `json:"weekly"`
    WeeklyScoped *usageWindow `json:"weeklyScoped,omitempty"` // model-scoped weekly cap, when present
    FetchedAt    time.Time    `json:"fetchedAt"`
}
```

Profile metadata (`credsProfileMeta`, persisted in creds.json) gains:

```go
Dead       bool      `json:"dead,omitempty"`
DeadReason string    `json:"deadReason,omitempty"` // "401 on usage fetch; refresh rejected (400)"
DeadAt     time.Time `json:"deadAt,omitempty"`
Usage      *usageSnapshot `json:"usage,omitempty"` // last good snapshot
UsageError string    `json:"usageError,omitempty"` // last transient failure, cleared on success
```

`credsProfileView` (GET /creds, /status.credentials, `tn creds list`)
exposes the same five fields. `LimitedUntil`/`LastLimitedAt` stay but are now
written ONLY from API `resetsAt` values, never from pane text.

`MarkLimited(label, until time.Time)` replaces `MarkLimited(label, resetRaw
string)`. Delete `parseResetTime` and its tests; `credsLimitFallbackCooldown`
stays as the fallback when the snapshot has no `resetsAt`.

## Poller (`Server.startUsagePoller`)

Ticker `credentials.usagePollInterval` (serve.json, default `60s`, min
`30s`). Each tick, for every profile in creds.json (active included):

1. Blob: active → live Claude item; inactive → profile item. Missing item →
   skip (view already says `stored:false`).
2. Inactive + `expiresAt` < now+5min → Refresh first; on success write the
   updated blob back to the profile item (keep all other JSON fields, update
   `accessToken`, `refreshToken` if non-empty, `expiresAt` = now+expires_in in
   ms epoch, matching Claude's own shape).
3. Fetch. 200 → store snapshot, clear `Dead`/`UsageError`; if
   `Session.Percent >= 100` set `LimitedUntil = Session.ResetsAt` (or
   now+fallback when zero). 401 → inactive: Refresh once, then Fetch once
   more; still 401 or refresh 400/401 → `Dead=true` with reason. Active +
   401 → do NOT mark dead (Claude may be mid-refresh); set `UsageError`,
   mark dead only after 3 consecutive ticks (`usageDeadStrikes`).
   Other status / transport error → `UsageError`, keep last snapshot.
4. After the pass: `s.creds.invalidateCache()`; then run
   `maybeProactiveSwap`.

One `security` subprocess per profile per tick plus one HTTPS call: with 2
profiles at 60 s this is negligible. Log one line per state CHANGE only
(dead↔alive, limited↔clear, swap), never per tick.

`s.creds == nil` (tests, newServer) → poller not started. Only `cmdServe`
wires `httpUsageClient`.

## Swap decision

`Eligible()` becomes: stored, not active, not dead, `LimitedUntil` not in the
future, and (when a snapshot exists) `Session.Percent < swapAtPercent` and
`Weekly.Percent < 100`. Sort: Session.Percent asc, then Weekly.Percent asc,
then LastUsedAt asc, then label.

`maybeProactiveSwap(now)`: runs when `autoSwap` is on. Condition: active
profile's snapshot has `Session.Percent >= credentials.swapAtPercent`
(default `90`) OR `Weekly.Percent >= 100`, AND `Eligible()` is non-empty,
AND the top candidate's `Session.Percent` is at least `swapMarginPercent`
(default `20`) lower than the active one's, AND `MinSwapInterval` has
elapsed since `lastSwap`. Then `Swap(target, "usage "+fmt(percent))`,
`MarkLimited(active, Session.ResetsAt)`, `nudgeRateLimitedPanes` (no panes are
parked on a proactive swap, so this is normally a no-op) and one dialog
"Credentials swapped proactively — <from> at <pct>% (resets <local time>) →
<to> (<pct>%)".

The pane-text path (`stuck.go` → `autoSwapCredentials`) stays as the
reactive fallback and is rewired: it no longer parses the pane's reset text
into a cooldown. It calls the same `Eligible()`/`Swap()`, and `MarkLimited`
with the active profile's snapshot `Session.ResetsAt` (fallback cooldown when
none). Dialog text unchanged except the "Auto-swap:" note wording may
mention "dead" profiles.

## Nudge timing

`credsNudgeDelay` (Escape → text) stays 500 ms. New: `credsSwapSettle`
(serve.json `credentials.nudgeDelay`, default `35s`) waited AFTER the
Keychain write and BEFORE any nudge, in a goroutine so the caller (stuck
detector tick / HTTP handler) returns immediately. Rationale: Claude's
Keychain read cache is ~30 s (unofficial, consistent with two independent
tools and the 2.1.x behaviour observed live). `POST /creds/swap` therefore
returns `{"from","to","nudgePending":[agents]}` and the nudge result is
logged, not returned. Skip the nudge for a pane whose last 30 lines contain
`login expired` or `please run /login` — typing into that pane cannot help
and stacks text in its input box (observed live).

## stuck.go fixes (independent of the API, ship in the same change)

1. Episode identity: `normalizeResetText(s)` = lowercase, strip a trailing
   parenthesised zone, collapse whitespace. Compare `RateLimitDialogResets`
   and `prevRateResets` on the normalised form. Test: `"8:10pm"` vs
   `"8:10pm (Europe/Paris)"` → same episode, one dialog, one swap.
2. `rateLimitClearedMarker` becomes a slice: add `"login expired"` and
   `"please run /login"` — an auth failure ends a usage-limit episode (Claude
   never prints "usage limit has reset" on that path, so the episode was
   sticking for 13+ h).

## CLI

- `tn creds list`: columns `LABEL EMAIL SESSION WEEKLY RESETS LIMITED-UNTIL LAST-USED STATE`
  where SESSION/WEEKLY are `12%`, RESETS is the session reset in local
  `15:04`, STATE is `active` / `` / `DEAD: <reason>` / `stale (no Keychain item)`.
  Uses the daemon's `GET /creds` when reachable (fresh snapshots), falls
  back to the store (metadata only, no HTTP) when not.
- `tn creds use <label>`: refuse a dead profile with a clear error and
  `--force` to override. Output mentions the 35 s settle: "swapped; parked
  panes will be nudged in ~35s".
- `tn creds save <label>`: after saving, one usage Fetch with the saved blob
  and print `session 3% · weekly 0%` or the failure — a save that cannot
  authenticate is reported immediately. Print, once, on every save:
  `note: never /logout to switch accounts — it revokes the token you just
  saved; /login directly into the other account instead.`
- `tn creds usage` = alias of `list` (discoverability).

## Endpoints

- `GET /creds` and `/status.credentials` gain per-profile
  `usage`/`dead`/`deadReason`/`deadAt`/`usageError`, plus top-level
  `usagePollInterval`, `swapAtPercent`, `lastPollAt`, `lastPollError`.
- `POST /creds/swap` → `{from,to,nudgePending:[…]}`; 409 for a dead target
  unless `{"force":true}`.
- `POST /creds/poll` → runs one poll pass synchronously, returns the same
  body as `GET /creds` (for `tn creds list` freshness and for tests).

## Config (serve.json `credentials`)

```json
"credentials": {
  "autoSwap": true,
  "minSwapInterval": "10m",
  "usagePollInterval": "60s",
  "swapAtPercent": 90,
  "swapMarginPercent": 20,
  "nudgeDelay": "35s"
}
```

All optional; defaults above. `swapAtPercent` outside 50–100 → clamp + log.

## Tests (httptest + fakes; never `security`, never network)

- `fakeUsageClient` scripted per token: status/snapshot/err sequence;
  records refresh calls.
- Poll marks an inactive profile dead on 401+refresh-400; a later 200
  revives it; active profile needs 3 strikes.
- Refresh writes the new pair back into the profile item, keeps other blob
  fields.
- Proactive swap fires at ≥90 with margin, respects `MinSwapInterval`, skips
  dead and cooling-down targets, sorts by session percent.
- Reactive path: `MarkLimited` uses snapshot `ResetsAt`, never pane text.
- `stuck.go`: re-wrapped reset text = one episode; "login expired" clears.
- Swap re-reads live blob just before overwrite (fake keychain mutated
  between Eligible and Swap → outgoing profile holds the newer blob).
- `POST /creds/swap` on a dead target → 409, `force` → 200.
- Race: `go test -race ./...` clean (poller + handlers + stuck detector).

## Docs to touch

- `SPEC-serve.md` "Credential profiles & auto-swap": one paragraph pointing
  here, mark pane-parsed cooldowns as removed.
- `CLAUDE.md` file map: add `usage.go`; bump `daemonVersion` to `0.10.0` and
  update the "currently" text.
- `README.md` `tn creds` section if it exists: `list` columns, `usage`
  alias, the `/logout` warning.
- `skills/tasknotes/` and `ORCHESTRATOR.md`: no change (protocol untouched).

## Out of scope (follow-ups, noted so they aren't re-derived)

- `CLAUDE_CONFIG_DIR`-scoped credentials for spawned orchestrators (own
  Keychain item, so a bad swap can never log out the user's interactive
  sessions). Needs a mirrored config dir; separate spec.
- `extra_usage` / spend caps — displayed nowhere yet; fields are parsed and
  dropped.

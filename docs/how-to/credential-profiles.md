# Credential profiles

Save multiple claude.ai logins and swap between them, manually or automatically when a usage limit hits.

> **macOS only.** Credential profiles read and write the macOS Keychain via
> `/usr/bin/security`. There is no equivalent on Linux.

## Background

Claude Code keeps its claude.ai OAuth credentials in one Keychain item (service `Claude Code-credentials`), shared by every Claude Code session on the machine. Signing in via `/login` in any session replaces that item's contents, which every other running session then picks up on its next API call. A "profile" is a saved copy of that blob under a label you choose. The secret itself is copied into its own Keychain item (service `tn-credential-profiles`, account = the label); it never touches disk. Only non-secret bookkeeping (which label is active, timestamps) lives in `~/.local/state/tn/creds.json`.

## Save a profile per account

```bash
/login                      # inside any Claude Code session: sign in to account A
tn creds save personal      # capture the current Keychain contents as "personal"
```

Repeat for each account you want available:

```bash
/login                      # sign in to account B
tn creds save team
```

A label must match `[a-z0-9][a-z0-9._-]{0,31}`: lowercase, digits, dot, underscore, hyphen, up to 32 characters, starting with a letter or digit.

Right after saving, `tn creds save` probes the profile's usage once, so a
token that can't actually authenticate (already revoked, wrong account) is
reported immediately instead of discovered hours later:

```text
saved profile "team" (team@example.com) — now the active profile
session 3% · weekly 0%
note: never /logout to switch accounts — it revokes the token you just saved; /login directly into the other account instead.
```

**Never `/logout` to switch accounts.** `/logout` revokes the token
server-side; `tn creds save` afterward would capture a dead token. `/login`
directly into the other account instead — that's what leaves the old
account's saved profile alone.

## List profiles

```bash
tn creds list      # or: tn creds usage (same command, easier to remember)
```

Expected output (when a daemon is reachable, SESSION/WEEKLY/RESETS come
from the daemon's own usage poller and are always fresh; without a daemon,
those columns are blank and the rest comes from the local store):

```text
   LABEL            EMAIL                            SESSION  WEEKLY   RESETS  LIMITED-UNTIL      LAST-USED          STATE
*  personal         you@example.com                  12%      3%       15:04                     2026-09-01 14:02   active
   team             team@example.com                 96%      10%      15:04   2026-09-02 15:00   2026-08-30 09:15
   old              old@example.com                                                                                  DEAD: 401 on usage fetch; refresh rejected (400)
```

The `*` marks the currently active profile (resolved by matching the live
Keychain item against each stored profile's refresh/access token, not just
trusting the last-recorded label, so a manual `/login` outside `tn` is
reflected correctly). `LIMITED-UNTIL` shows a cooldown from a past
usage-limit episode, if any. `STATE` is `active`, blank, `DEAD: <reason>`
(the daemon's usage poller confirmed this profile can no longer
authenticate — see below), or `stale (no Keychain item)` for a metadata
entry whose Keychain item is gone.

## Manual swap

```bash
tn creds use personal
```

If the daemon is running, this goes through it (`POST /creds/swap`) so any
parked orchestrator panes get nudged to resume automatically, after a short
settle delay (`credentials.nudgeDelay`, default 35s — see below). Without a
daemon reachable, it swaps the Keychain item directly and tells you parked
panes were **not** nudged.

A profile the usage poller has marked dead is refused:

```text
$ tn creds use old
Error: profile "old" is dead: 401 on usage fetch; refresh rejected (400) (pass --force to override)
$ tn creds use --force old
```

## Automatic swap on a usage limit

Enable in `~/.config/tn/serve.json`:

```json
{
  "credentials": {
    "autoSwap": true,
    "minSwapInterval": "10m",
    "usagePollInterval": "60s",
    "swapAtPercent": 90,
    "swapMarginPercent": 20,
    "nudgeDelay": "35s"
  }
}
```

All fields are optional; the values above are the defaults (`usagePollInterval` has a 30s floor). With `autoSwap` on, the daemon polls claude.ai's own usage endpoint for every saved profile (the active one included) every `usagePollInterval`, and can act in two ways:

**Proactively**, before any pane ever parks: once the active profile's session usage reaches `swapAtPercent` (or its weekly usage hits 100%), and a saved profile is at least `swapMarginPercent` lower, the daemon swaps to it on its own — most of the time nobody sees a usage-limit pane at all.

**Reactively**, as a fallback: if a pane still shows a claude.ai usage-limit message (the poller missed the window, or the daemon was just started), the same stuck-prompt detection that flags a permission prompt catches it, and the daemon:

1. **Marks the currently active profile as cooling down** until its own polled usage snapshot's reported reset time (never parsed from pane text — pane text is display-only now).
2. **Picks the best eligible profile**: stored, not the active one, not dead, not itself cooling down, sorted by lowest session usage — and swaps to it. At most one automatic swap per `minSwapInterval` (default 10 minutes), so a swap that didn't actually help doesn't immediately chain into another.
3. **After `nudgeDelay`** (default 35s — Claude Code's own Keychain read cache is roughly 30s, and typing into a pane before that lands on the old token), **types a continuation prompt into every currently-parked orchestrator pane**: they share the account, so they all parked together and all come back together. A pane whose text shows an auth failure (`login expired`, `please run /login`) rather than a usage-limit park is skipped — typing into it can't help.
4. **Shows one native macOS dialog** summarizing what happened.

If no eligible profile exists (none saved, all cooling down, or all dead), or the last swap was too recent, no swap happens; the dialog explains why instead.

A profile the poller can't get a fresh token for (an inactive profile's own refresh is rejected, or the active profile 401s three consecutive polls in a row — Claude Code may be mid-refresh on a single 401) is marked **dead**: excluded from automatic swaps and refused by `tn creds use`/`POST /creds/swap` unless forced. It's revived automatically the moment a usage fetch for it succeeds again (e.g. after a fresh `/login` + `tn creds save`).

## Caveats

Turning on `autoSwap` (or running `tn creds use` manually) means accepting:

- **Every Claude Code session on the machine switches**, including interactive ones you're actively using, not just daemon-spawned orchestrators.
- **Remote Control sessions disconnect** on a swap.
- **`~/.claude.json`'s displayed email lags**: it's deliberately never written by `tn` (running sessions rewrite that file wholesale, so a concurrent edit would race), so `/login`'s displayed account name may not immediately reflect the account actually in use. Swapping is ordered so a refresh-token rotation is never lost: the blob currently in the Claude item is written back into its owning profile before being overwritten, so swapping back later doesn't land on a stale, already-rotated refresh token.
- **Rotating accounts to work around usage limits may not align with Anthropic's usage policy.** Check the terms that apply to your accounts before enabling automatic swapping.

## Endpoints and other surfaces

- `GET /creds`: the same information `tn creds list` shows, as JSON (per-profile `usage`/`dead`/`deadReason`/`usageError`, plus top-level `usagePollInterval`/`swapAtPercent`/`lastPollAt`/`lastPollError`). Never returns secrets.
- `POST /creds/swap {"label": "...", "force": false}`: the manual-swap endpoint `tn creds use` calls when a daemon is reachable — `{"from","to","nudgePending"}`; 409 for a dead target unless `force` is set.
- `POST /creds/poll`: runs one usage-poll pass synchronously and returns the same body as `GET /creds` — for forcing a fresh read (e.g. right after a `/login`) without waiting out `usagePollInterval`; `tn creds list` itself just calls `GET /creds`, which serves whatever the background poller last saw.
- `/status`'s `credentials` field and the `/ui` web UI's own switcher expose the same state for a tray-style client.

## See also

- [Troubleshooting](troubleshooting.md): "usage limit parked sessions."
- [Reference: HTTP API](../reference/http-api.md)

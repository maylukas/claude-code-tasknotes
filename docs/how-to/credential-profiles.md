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

## List profiles

```bash
tn creds list
```

Expected output:

```text
   LABEL            EMAIL                            LIMITED UNTIL          LAST USED
*  personal         you@example.com                                        2026-09-01 14:02
   team             team@example.com                 2026-09-02 15:00      2026-08-30 09:15
```

The `*` marks the currently active profile (resolved by matching the live Keychain item against each stored profile's refresh/access token, not just trusting the last-recorded label, so a manual `/login` outside `tn` is reflected correctly). "LIMITED UNTIL" shows a cooldown from a past usage-limit episode, if any.

## Manual swap

```bash
tn creds use personal
```

If the daemon is running, this goes through it (`POST /creds/swap`) so any parked orchestrator panes get nudged to resume automatically. Without a daemon reachable, it swaps the Keychain item directly and tells you parked panes were **not** nudged.

## Automatic swap on a usage limit

Enable in `~/.config/tn/serve.json`:

```json
{
  "credentials": {
    "autoSwap": true,
    "minSwapInterval": "10m"
  }
}
```

When an orchestrator's pane shows a claude.ai usage-limit message, the daemon detects it via the same stuck-prompt pane capture used elsewhere, then:

1. **Marks the currently active profile as cooling down** until the pane's reported reset time (parsed from text like `2:30pm (Europe/Berlin)`; falls back to a 5-hour cooldown, Claude's session-limit window, if the text can't be parsed).
2. **Picks the oldest-used eligible profile**: stored, not the active one, not itself cooling down, and swaps to it. At most one automatic swap per `minSwapInterval` (default 10 minutes), so a swap that didn't actually help doesn't immediately chain into another.
3. **Types a continuation prompt into every currently-parked orchestrator pane**: they share the account, so they all parked together and all come back together.
4. **Shows one native macOS dialog** summarizing what happened.

If no eligible profile exists (none saved, or all cooling down), or the last swap was too recent, no swap happens; the dialog explains why instead.

## Caveats

Turning on `autoSwap` (or running `tn creds use` manually) means accepting:

- **Every Claude Code session on the machine switches**, including interactive ones you're actively using, not just daemon-spawned orchestrators.
- **Remote Control sessions disconnect** on a swap.
- **`~/.claude.json`'s displayed email lags**: it's deliberately never written by `tn` (running sessions rewrite that file wholesale, so a concurrent edit would race), so `/login`'s displayed account name may not immediately reflect the account actually in use. Swapping is ordered so a refresh-token rotation is never lost: the blob currently in the Claude item is written back into its owning profile before being overwritten, so swapping back later doesn't land on a stale, already-rotated refresh token.
- **Rotating accounts to work around usage limits may not align with Anthropic's usage policy.** Check the terms that apply to your accounts before enabling automatic swapping.

## Endpoints and other surfaces

- `GET /creds`: the same information `tn creds list` shows, as JSON. Never returns secrets.
- `POST /creds/swap {"label": "..."}`: the manual-swap endpoint `tn creds use` calls when a daemon is reachable.
- `/status`'s `credentials` field and the `/ui` web UI's own switcher expose the same state for a tray-style client.

## See also

- [Troubleshooting](troubleshooting.md): "usage limit parked sessions."
- [Reference: HTTP API](../reference/http-api.md)

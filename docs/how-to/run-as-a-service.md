# Run tn serve as a service

Keep the bridge daemon running continuously instead of in a foreground terminal.

> **macOS only.** This guide's launchd instructions, code signing, and the background-QoS
> note apply to macOS. See [Linux](#linux) at the end for what's missing there.

## Prerequisites

- `tn` built and working (`tn health` succeeds).
- `~/.config/tn/config.json` and `~/.config/tn/serve.json` already set up.

Also make sure the daemon can find `ORCHESTRATOR.md`, the operating contract it hands to every spawned orchestrator session. Either copy it from this repository to `~/.config/tn/ORCHESTRATOR.md`, or set `"orchestratorDoc": "/path/to/claude-code-tasknotes/ORCHESTRATOR.md"` in `serve.json`. The startup log prints `serve: orchestrator contract: <path>`; see [the configuration reference](../reference/configuration.md#files-and-directories).

## 1. Write a LaunchAgent plist

Create `~/Library/LaunchAgents/com.example.tn-serve.plist`, replacing the paths and label to match your setup (a reverse-DNS label under your own domain, not `com.example`, is conventional, but any unique label works):

```xml
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN"
  "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key>
  <string>com.example.tn-serve</string>

  <key>ProgramArguments</key>
  <array>
    <string>/Users/you/bin/tn</string>
    <string>serve</string>
  </array>

  <key>RunAtLoad</key>
  <true/>
  <key>KeepAlive</key>
  <true/>

  <key>StandardOutPath</key>
  <string>/Users/you/Library/Logs/tn-serve.log</string>
  <key>StandardErrorPath</key>
  <string>/Users/you/Library/Logs/tn-serve.log</string>

  <!-- Optional: only needed if tn serve should see env vars your login
       shell sets but launchd's own minimal environment doesn't. -->
  <key>EnvironmentVariables</key>
  <dict>
    <key>PATH</key>
    <string>/usr/bin:/bin:/usr/sbin:/sbin:/opt/homebrew/bin:/usr/local/bin</string>
  </dict>
</dict>
</plist>
```

`ProgramArguments` must use the binary's absolute path, since launchd does not consult your shell's `PATH`. This is also why the daemon itself resolves `git`, `tmux`, and `taskpolicy` by absolute path internally rather than trusting `PATH`.

## 2. Load and start it

```bash
launchctl bootstrap gui/$UID ~/Library/LaunchAgents/com.example.tn-serve.plist
launchctl kickstart -k gui/$UID/com.example.tn-serve
```

`bootstrap` registers the job; `kickstart -k` starts (or restarts) it immediately rather than waiting for the next login. To stop it:

```bash
launchctl bootout gui/$UID/com.example.tn-serve
```

## 3. Tail the log

```bash
tail -f ~/Library/Logs/tn-serve.log
```

Expect to see the startup sequence: leaving the background QoS tier, webhook registration, and (once a task routes) spawn activity.

## 4. Code-sign the binary

The daemon shows native macOS permission dialogs via `osascript` (for stuck-prompt approvals and usage-limit swaps). macOS's TCC (Transparency, Consent, and Control) subsystem keys its "would like to access data from other apps" Apple-Events approval on the binary's **code identity**. An unsigned or differently-signed rebuild changes identity every time, which re-triggers the approval prompt on every deploy.

Sign with your own Apple Development identity so one approval stays valid across rebuilds:

```bash
codesign -s "Apple Development: Your Name (TEAMID)" -f --identifier com.example.tn ~/bin/tn
```

Find your identity string with `security find-identity -v -p codesigning`.

If you don't have a code-signing certificate, ad-hoc signing is the fallback:

```bash
codesign -s - -f --identifier com.example.tn ~/bin/tn
```

**Limitation:** an ad-hoc signature (`-s -`) still changes on every rebuild in a way that can retrigger TCC prompts, since it isn't tied to a stable identity the way a real certificate is. It's better than nothing, but if you rebuild frequently and want the one-approval-forever behavior, get a free Apple Development certificate via Xcode.

## 5. The background-QoS note

Under launchd, a spawned process, and everything it execs (`git`, `tmux`), runs in the background QoS tier by default, where disk I/O is throttled hard. The daemon lifts itself out of this tier once at startup via `taskpolicy -B -p <own pid>`; children then inherit the normal tier. Look for this line in the log at startup:

```text
serve: left background QoS tier (taskpolicy -B -p 12345)
```

If it's missing, `/usr/sbin/taskpolicy` wasn't found or refused. The daemon logs a warning and carries on, but git-heavy background work (like the [worktree reaper](worktree-reaper.md)) and periodic timers can then run far slower than expected, or get their own ticks coalesced/deferred while the machine is idle.

## 6. Verify a deploy actually took effect

A successful `codesign`/`go build` does not guarantee the running process picked up the change; see the [self-restart watcher](#self-restart-watcher) below for why. After any rebuild you expect the running daemon to pick up:

```bash
curl -s localhost:8391/status | python3 -c 'import json,sys; d=json.load(sys.stdin)["daemon"]; print(d["version"], d["uptime"])'
```

Compare the printed version against the `daemonVersion` constant in `serve.go` you just built, and the uptime against how long ago you actually built. If they don't match what you expect, the old process is still serving:

```bash
launchctl kickstart -k gui/$UID/com.example.tn-serve
```

## Self-restart watcher

`tn serve` watches its own binary on disk and exits when it changes (polling every 10 seconds), so a `go build -o ~/bin/tn` rebuild is normally picked up automatically: `KeepAlive` in the plist above brings the process straight back with the new binary. Disable this for foreground or development runs (where you don't want the process to unexpectedly exit and, without a supervisor, just stay dead) by setting:

```bash
TN_NO_SELFRESTART=1 tn serve
```

Even with the watcher enabled, don't assume a rebuild took effect without checking. Always verify per step 6 above.

## Linux

No service file is shipped for Linux. The daemon's core (state, HTTP handlers, webhook routing, spawn-via-tmux) is portable Go and should run, but the following are macOS-only and simply won't work:

- Native permission dialogs (`osascript`) for stuck-prompt approvals and usage-limit swaps.
- Credential profiles (`tn creds`): they read/write the macOS Keychain via `/usr/bin/security`.
- The background-QoS lift (`taskpolicy`).
- `codesign`/TCC: irrelevant on Linux, but also means there's no equivalent mechanism to worry about.

A minimal systemd user unit as a starting point (adjust paths, and expect the caveats above):

```ini
# ~/.config/systemd/user/tn-serve.service
[Unit]
Description=tn bridge daemon

[Service]
ExecStart=/home/you/bin/tn serve
Restart=always
Environment=TN_NO_SELFRESTART=1

[Install]
WantedBy=default.target
```

```bash
systemctl --user daemon-reload
systemctl --user enable --now tn-serve
journalctl --user -u tn-serve -f
```

`TN_NO_SELFRESTART=1` is set deliberately here: systemd's own `Restart=always` already handles bringing the process back after a binary swap and a manual restart, so the daemon's own watcher is redundant on this platform and not verified to interact correctly with systemd's process model.

## See also

- [Add a project](add-a-project.md)
- [Troubleshooting](troubleshooting.md)

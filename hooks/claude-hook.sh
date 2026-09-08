#!/bin/bash
# THIS FILENAME IS A PUBLISHED INTERFACE, NOT AN INTERNAL DETAIL — this
# path is what's actually written into the user's real ~/.claude/settings.json
# right now (confirmed 2026-08-26). Renaming this file without first
# grepping ~/.claude/settings.json (and any project-local
# .claude/settings.json under tn-managed repos) for the OLD path is a
# breaking change that fails SILENTLY: the hook simply stops running, the
# daemon receives nothing, and the stuck detector quietly falls back to
# pane-scraping with nobody aware anything regressed — this already
# happened once (a rename from permission-request.sh to this file, caught
# only because someone else happened to check). If you rename this file
# again, update ~/.claude/settings.json (and confirm any project-local
# copies) in the SAME change — see CLAUDE.md's "published interface" note.
#
# Claude Code PermissionRequest + PermissionDenied hooks -> tn serve.
# See SPEC-serve.md's "PermissionRequest hook detection" section for the
# full design, including why PermissionDenied exists as PASSIVE TELEMETRY
# ONLY (no retry, no decision) even though the event is documented to
# support a retry:true response — this script never sends one.
#
# Pure reporting either way: exits 0 with empty stdout UNCONDITIONALLY,
# regardless of what happens below — Claude Code's own documented contract
# is that silence from a hook is never approval, and this script must
# never be the thing standing between a real permission prompt and the
# human who needs to see it, whether the daemon is up, down, slow, or this
# script itself fails outright.
#
# Install by adding BOTH entries to your ~/.claude/settings.json for
# coverage everywhere (this is the CURRENT install on this machine as of
# 2026-08-26, at the user's explicit request — safe only because of the
# TN_AGENT_NAME guard below, see the LOAD-BEARING note on it), OR —
# recommended for a NEW install where global coverage isn't specifically
# wanted — a project-local .claude/settings.json in the root of each
# tn-managed repo, so this only ever activates for daemon-spawned
# orchestrator sessions and never for the user's own ad-hoc Claude Code
# sessions elsewhere. (Skill-frontmatter scoping was considered and ruled
# out: Claude Code's own docs state hooks load only at session start and
# cannot be hot-swapped mid-session, which is in direct tension with a
# skill invoked partway through one — see SPEC-serve.md's "Deployment
# scoping" note for the full reasoning.)
#
#   {
#     "hooks": {
#       "PermissionRequest": [
#         { "hooks": [
#           { "type": "command", "command": "bash /absolute/path/to/tasknotes-cli/hooks/claude-hook.sh", "timeout": 3 }
#         ] }
#       ],
#       "PermissionDenied": [
#         { "hooks": [
#           { "type": "command", "command": "bash /absolute/path/to/tasknotes-cli/hooks/claude-hook.sh", "timeout": 3 }
#         ] }
#       ]
#     }
#   }
#
# One script for both — it reads hook_event_name from its own stdin and
# routes accordingly, rather than needing two separate files kept in sync.
#
# The timeout above is deliberate and should stay short: Claude Code
# appears to wait for a configured hook to return (or time out) before
# rendering the real interactive prompt (every hooks.json example in
# Claude Code's own docs sets a bounded timeout), so a slow or hung daemon
# must never be able to add noticeable latency to a human's actual session
# — see the --max-time on the curl calls below, shorter than this script's
# own configured hook timeout for the same reason.
#
# This only reports for daemon-spawned sessions: $TN_AGENT_NAME is set by
# buildLaunchCommand (serve.go) as a literal env-var prefix on the exact
# shell command that execs `claude` in the tmux pane it spawns — confirmed
# live (2026-08-26 spike) to reach this hook's own subprocess environment.
# An ordinary interactive Claude Code session run directly by a human has
# no TN_AGENT_NAME at all, so this script is a silent no-op for it even if
# installed in a GLOBAL settings.json.
#
# LOAD-BEARING, DO NOT WEAKEN: the user's actual install (2026-08-26) IS
# global, at their explicit request, made safe ONLY by the guard directly
# below. Every ordinary Claude Code session on this machine now runs this
# script on every PermissionRequest/PermissionDenied event. Any future
# change that makes this script act (network call, side effect, anything
# beyond an immediate exit 0) when $TN_AGENT_NAME is unset stops being
# "safe for a global install" and starts affecting every session the user
# runs, not just tn-spawned ones — treat this guard as the one thing in
# this file that must never be relaxed, reordered after other logic, or
# made conditional on anything else.

set -u

if [ -z "${TN_AGENT_NAME:-}" ]; then
  exit 0
fi

bridge_url="${TN_BRIDGE_URL:-http://localhost:8391}"

input=$(cat)

# Minimal, dependency-light JSON field extraction (python3, not jq — this
# repo already assumes python3's presence nowhere else, but it's a safer
# universal default than assuming jq is installed on every machine this
# might get deployed to; the fields extracted here are only for logging/
# telemetry on the daemon side, never for any decision this script makes).
# tool_input.command is included when present (Bash-shaped calls) — the
# PermissionDenied path uses it to say WHAT was blocked, not just that
# something was.
fields=$(python3 - "$input" <<'PY' 2>/dev/null
import json, sys
try:
    d = json.loads(sys.argv[1])
except Exception:
    d = {}
tool_input = d.get("tool_input") or {}
print(d.get("hook_event_name", ""))
print(d.get("session_id", ""))
print(d.get("prompt_id", ""))
print(d.get("tool_name", ""))
print(d.get("permission_mode", ""))
print(tool_input.get("command", "") if isinstance(tool_input, dict) else "")
PY
)
hook_event_name=$(echo "$fields" | sed -n '1p')
session_id=$(echo "$fields" | sed -n '2p')
prompt_id=$(echo "$fields" | sed -n '3p')
tool_name=$(echo "$fields" | sed -n '4p')
permission_mode=$(echo "$fields" | sed -n '5p')
command=$(echo "$fields" | sed -n '6p')

case "$hook_event_name" in
  PermissionRequest)
    endpoint="/hooks/permission-request"
    ;;
  PermissionDenied)
    endpoint="/hooks/permission-denied"
    ;;
  *)
    # Unknown/future event this script wasn't written for — report
    # nothing rather than guess.
    exit 0
    ;;
esac

payload=$(python3 - "$TN_AGENT_NAME" "$session_id" "$prompt_id" "$tool_name" "$permission_mode" "$command" <<'PY' 2>/dev/null
import json, sys
agent_name, session_id, prompt_id, tool_name, permission_mode, command = sys.argv[1:7]
print(json.dumps({
    "agentName": agent_name,
    "sessionId": session_id,
    "promptId": prompt_id,
    "toolName": tool_name,
    "permissionMode": permission_mode,
    "command": command,
}))
PY
)

if [ -n "$payload" ]; then
  curl -fsS --max-time 2 -X POST "$bridge_url$endpoint" \
    -H "Content-Type: application/json" \
    -d "$payload" >/dev/null 2>&1
fi

exit 0

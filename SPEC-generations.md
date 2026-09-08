# Spec: generational orchestrators with drain

Concurrent orchestrators per project with a drain lifecycle, replacing hard rotation:
an orchestrator that has consumed enough of its context stops ACCEPTING new tasks,
finishes what it owns, then exits; a fresh generation runs alongside it taking all new
work. Rolling replacement — no gap, no big-bang handoff.

## Agent model changes

- `Agent` gains `Accepting bool` (JSON `accepting`) — true on register (re-register
  resets to true), and `TmuxSession string` (JSON `tmuxSession`, optional).
- `POST /agents/drain` {name} → sets Accepting=false (idempotent). New CLI:
  `tn drain --name N`. Draining agents keep polling/acking; they just get no NEW
  assignments.
- `GET /agents` + `tn agents` + dashboard show the state: 🟢 accepting / 🌀 draining
  (dashboard status precedence: stuck > unacked backlog > draining > alive > gone).

## Generation naming

- Spawn generates `gen := time.Now().Unix() mod 100000` (collision-safe enough);
  agent name `orchestrator-<slug>-g<gen>`, tmux session `tn-<slug>-g<gen>`.
- Bootstrap prompt must carry BOTH literally: "register with the bridge as
  <agentName>; your tmux session is <tmuxSession>".
- Everything that previously derived the tmux session from the agent name
  (stuck detector, dashboard Attach column, session pages) now uses
  `Agent.TmuxSession` when set, falling back to the old `tn-<project>` derivation for
  legacy agents. `tn register` gains `--tmux-session S` (bootstrap prompt tells the
  orchestrator its session; include it in the register instruction).
- The stuck detector iterates agents with a known tmux session (any name), not just
  `orchestrator-<slug>`-named ones.

## Routing changes

- `aliveAcceptingAgentForProjectLocked(project)` — like the existing helper but
  requires Accepting; use it for NEW task assignments (webhook tag-assign, scanner,
  unblock pass, POST /messages project resolution).
- Task ownership: an agent OWNS a taskPath once it acks an assignment for it (derive:
  latest acked message with text prefix "Task assigned:" for that taskPath → its To).
  Task-scoped messages (@claude lines, tn send --task) route to the alive owner even
  if draining; owner dead/absent → alive accepting agent; none → logical queue (below).
- Logical queue name `orchestrator-<slug>` still used when nothing is alive. On ANY
  register of an accepting agent whose project is <slug>, re-target all queued messages
  addressed to `orchestrator-<slug>` to the new agent's name (log count).
- Spawn trigger everywhere: spawn when no alive ACCEPTING agent exists (draining-only
  = still spawn a new generation).

## Drain trigger (contract side, include in ORCHESTRATOR.md rewrite of the Rotation section)

Replace the Rotation section with "Generations & drain":
- Each session sees a remaining-token indicator in its context. When context usage
  exceeds ~40% (i.e. remaining budget < ~60% of what you started with — check after
  every task pickup), run `tn drain --name <yourName>` immediately. From then on:
  finish and `review`/close what you own, keep answering task-scoped user messages,
  ack everything, and when the last owned task is settled: handoff-check notes,
  `tn log "generation <name> drained — exiting"`, `tn deregister`, exit claude, then
  `tmux kill-session -t <yourTmuxSession>`.
- Also drain immediately on bookkeeping-drift signals (in-progress without live worker,
  acking without scheduling) — drain beats limping.
- Two generations may overlap for hours; that is by design. Contended lanes (emulator
  VMs, staging mutations): the DRAINING generation finishes its lane tasks; the new
  generation must not start same-lane tasks until the old generation's lane work is
  done (check `tn agents` + task notes).

## Keep

- Stale-tmux respawn fix (previous message) applies to generation sessions too.
- All existing dedup/guard passes unchanged except swapping in the accepting-aware
  resolver.

## Tests

Register sets accepting; drain flips it; new-assignment resolution skips draining
agents but task-scoped messages still reach the draining owner; logical-queue
re-target on register (count + names); spawn fires when only draining agents alive;
generation naming propagates to bootstrap prompt + register --tmux-session + stuck
detector session lookup (fixture agent with TmuxSession set); dashboard shows 🌀.

Verification: gofmt/vet, `go test ./... -count=1` green, rebuild ~/bin/tn + codesign.
Update README, SPEC-serve.md cross-refs, ORCHESTRATOR.md (replace Rotation section as
described — keep the handoff-quality bar language), skill one-liner.

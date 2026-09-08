# Contributing to tasknotes-cli

Thanks for your interest in improving tasknotes-cli. This guide covers how to set up
a development environment, the conventions the codebase follows, and how to submit a
change.

## Ways to contribute

- **Bug reports.** Open an issue with reproduction steps. See
  [SUPPORT.md](.github/SUPPORT.md) for what to include.
- **Bug fixes and small improvements.** Look for issues labeled `good first issue` or
  `help wanted`.
- **Features.** Open an issue labeled `enhancement` to discuss the design before
  writing a large amount of code, especially anything that touches the daemon's HTTP
  surface, config shape, or spawn/generation lifecycle.
- **Documentation.** Fixes and additions to anything under `docs/`, the root
  `README.md`, or `ARCHITECTURE.md` are welcome, labeled `docs`.

By participating in this project you agree to abide by the
[Code of Conduct](CODE_OF_CONDUCT.md).

## Development setup

Requirements:

- **Go 1.26** or later.
- **macOS** for full feature testing. The daemon core (task CLI, HTTP bridge, agent
  registry, message bus) is portable Go and builds on Linux, but several features
  shell out to macOS-only tools and cannot be exercised on other platforms:
  `osascript` approval dialogs, Keychain-backed credential profiles (`/usr/bin/security`),
  `taskpolicy`, launchd supervision, and `codesign`. If you're on Linux you can still
  build, run `go vet`, and run the non-macOS-gated tests, but plan to have someone
  verify macOS-only paths before merging.
- **Node 22** and **pnpm 11** if you're changing anything under `webui/`.

Clone the repository and build the binary:

```bash
go build -o ~/bin/tn .
```

On macOS, sign the binary after every build so the daemon's Apple Events approval
(used for `osascript` dialogs) stays valid instead of re-prompting on every rebuild:

```bash
codesign -s "Apple Development: Your Name (TEAMID)" -f --identifier com.example.tn ~/bin/tn
```

## The pre-PR gate

Before opening a pull request, run:

```bash
gofmt -l . && go vet ./... && go test ./... && go test -race ./...
```

All four must be clean. `gofmt -l .` should print nothing (any output means a file is
misformatted); `go vet`, `go test`, and `go test -race` must all exit zero.

**`go test -race` is not optional.** It has already caught two real concurrency bugs
that the non-race test run missed entirely: a shared message struct being
JSON-encoded outside its lock while a concurrent inbox poll mutated it (a genuine
production race, not a test artifact), and an unsynchronized slice in a debouncer
test. Without `-race`, this whole class of bug stays latent until it causes a hard-to-
reproduce failure in the field. Always run it, even for changes that look
single-threaded.

## Test conventions

- Tests use `net/http/httptest` for the daemon's HTTP surface, never a real running
  daemon.
- Side effects that would touch the outside world are injected as function values so
  tests can fake them: `spawnFunc` (tmux spawning), plus similar injection points for
  output capture, dialog display, key-sending, Keychain access, and git operations.
  **Tests must never touch a real tmux session, the real Obsidian vault, the real
  macOS Keychain, or the user's real `~/.config/tn`.** If you're adding code with an
  external side effect, add an injectable seam for it rather than calling the real
  thing directly.
- Functional smoke runs (starting a real daemon process for a test) use explicit high
  ports (the `18xxx` range) and a scratch `$HOME`/dashboard path. They never use the
  real config directory or vault.
- Set `TN_NO_SELFRESTART=1` when starting a real daemon in a test or smoke run, so it
  doesn't try to restart itself mid-test.

## Code style

- Format with `gofmt`; the pre-PR gate enforces this.
- **Stdlib only.** This project deliberately has zero external Go dependencies. Don't
  add one without discussing it in an issue first: the bar is high.
- Write doc comments that explain *why*, not just what. Several comments in this
  codebase document a specific live incident that motivated a piece of logic; that
  context is what keeps the next person from "simplifying" away a fix for a bug they
  can't see. Follow that pattern for non-obvious changes.
- When you change the orchestrator spawn protocol, keep the following in sync in the
  same change: the prompt-building code (`buildOrchestratorPrompt` /
  `buildLaunchCommand`), their tests, `ORCHESTRATOR.md`, and the `skills/tasknotes/`
  skill. These four describe the same contract from different angles; letting them
  drift is a common source of confusing bugs.

## Changing the web UI

Changes under `webui/src` require an extra build step that is easy to forget:

```bash
cd webui && pnpm build
cd .. && go build -o ~/bin/tn .
```

`go build` embeds whatever is currently on disk in `webui/dist/`. It has no way to
know your TypeScript source has changed. **Always run `pnpm build` and commit the
resulting `webui/dist/` changes in the same pull request** as your `webui/src`
changes, or the daemon will silently keep serving the old UI.

## Bumping `daemonVersion`

If your change alters the daemon's observable behavior (the shape of `/status` or
`/health`, a new endpoint, or a behavior change a user or the web UI could notice),
bump the `daemonVersion` constant in `serve.go`. It's surfaced in `/health`, `/status`, and
the web UI footer, and it's the only reliable way to tell whether a running daemon
actually picked up a given change.

## Submitting a pull request

1. Create a branch off `main` for your change.
2. Keep pull requests small and focused on one thing. A large, mixed-purpose PR is
   harder to review and more likely to hide a mistake.
3. In the PR description, describe how you verified the change (which tests you ran,
   and any manual verification you did. See `docs/how-to/troubleshooting.md` if
   you're not sure what to check for daemon changes).
4. Update relevant documentation alongside code changes, and add an entry under
   `## [Unreleased]` in [CHANGELOG.md](CHANGELOG.md).
5. If you touched `webui/`, confirm `webui/dist/` is rebuilt and committed. If you
   changed daemon behavior, confirm `daemonVersion` was bumped.

The pull request template in `.github/PULL_REQUEST_TEMPLATE.md` has a checklist
covering all of the above.

### Commit messages

Write an imperative-mood subject line, no more than 72 characters (e.g. "Fix stranded
message reassignment on daemon restart", not "Fixed a bug" or "Fixes stranded
messages."). Add a body if the *why* isn't obvious from the subject and the diff.

## Contributor license

This project does not require a CLA or a DCO sign-off. By submitting a pull request,
you agree to license your contribution under the project's [LICENSE](LICENSE).

## Issue labels

| Label | Meaning |
|---|---|
| `good first issue` | Small, self-contained, good entry point for a new contributor. |
| `help wanted` | Maintainer would like help on this one. |
| `bug` | Something is broken. |
| `enhancement` | A new feature or improvement. |
| `docs` | Documentation-only change. |

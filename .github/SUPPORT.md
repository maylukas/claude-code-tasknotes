# Support

## Where to ask

- **Questions, usage help, "how do I…":** use
  [GitHub Discussions](https://github.com/maylukas/claude-code-tasknotes/discussions) (or, if
  Discussions isn't enabled on this repository, open an issue and label it
  `question`).
- **Something is broken:** open an
  [issue](https://github.com/maylukas/claude-code-tasknotes/issues/new/choose) using the bug
  report template.
- **Security issue:** do not open a public issue. See [SECURITY.md](../SECURITY.md).

## What to include

The faster we can reproduce your problem, the faster we can fix it. Please include:

- **Daemon version.** With the daemon running: `curl localhost:8391/health`.
- **Operating system** and version (macOS or Linux; note that several features are
  macOS-only, see [SECURITY.md](../SECURITY.md) and the docs for details).
- **A log excerpt.** On macOS with the launchd service installed, the daemon log is
  at `~/Library/Logs/tn-serve.log`. Include the lines around when the problem
  happened.
- **`tn health` output**, if the CLI itself is involved.

## Before you ask

Check the [troubleshooting guide](../docs/how-to/troubleshooting.md). It covers the
most common failure modes (stale web UI after a `webui/` change, a daemon that hasn't
picked up a rebuilt binary, spawn failures, and more).

# Security policy

This document describes how to report a security vulnerability in tasknotes-cli, and
what's in and out of scope.

## Supported versions

This project is pre-1.0. Only the **latest release** receives security fixes. There
is no long-term support branch yet; if you're running an older tag, upgrade before
filing a report.

## Reporting a vulnerability

Please report security vulnerabilities privately, not in a public issue.

- Preferred: use [GitHub's private vulnerability reporting](https://docs.github.com/en/code-security/security-advisories/guidance-on-reporting-and-writing/privately-reporting-a-security-vulnerability)
  on this repository (Security tab → "Report a vulnerability").
- Alternative: email `[INSERT SECURITY CONTACT EMAIL]`.

Include what you'd include in any good bug report: affected version, a description of
the issue, and reproduction steps if you have them.

### What to expect

- Acknowledgement within a few business days.
- An assessment of severity and, if confirmed, a rough timeline for a fix.
- Credit in the fix's release notes, if you'd like it.

Please give us a reasonable amount of time to address a confirmed issue before any
public disclosure.

## Scope notes

A few things about this project's design are relevant to thinking about its security
model, and are documented in more depth in
[docs/explanation/security-model.md](docs/explanation/security-model.md):

- **The bridge daemon (`tn serve`) binds to localhost and has no authentication, by
  design.** It's built to be a trusted, single-user local process, similar to a
  database or dev server bound to `127.0.0.1`. It is not a service exposed to a
  network or to other local users you don't trust. Any local process that can reach
  its port can call its API. Don't expose the daemon's port beyond localhost.
- **The credential-profile feature stores secrets in the macOS Keychain**, not in a
  plaintext file, using the system's own access-control model for that credential
  store.

If you find a way to escalate access, bypass a trust boundary, or exfiltrate
credentials against these design assumptions, that's exactly the kind of report this
policy wants. Please report it privately as above.

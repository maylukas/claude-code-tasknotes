## Summary

<!-- What does this change do, and why? -->

## Checklist

- [ ] Ran the pre-PR gate and it's clean: `gofmt -l . && go vet ./... && go test ./... && go test -race ./...`
- [ ] Added or updated tests for this change
- [ ] Updated relevant documentation (`docs/`, `README.md`, `ARCHITECTURE.md`)
- [ ] Added an entry under `## [Unreleased]` in `CHANGELOG.md`
- [ ] If `webui/src` changed: ran `pnpm build` and committed the resulting `webui/dist/` changes
- [ ] If daemon behavior changed (new endpoint, changed `/status`/`/health` shape, other observable behavior): bumped `daemonVersion` in `internal/tn/serve.go`

## How was this verified?

<!-- Which tests did you run? Any manual verification (e.g. against a real daemon)? -->

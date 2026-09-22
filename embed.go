// Package tasknotescli is the module root. Its only job is to embed
// ORCHESTRATOR.md, the operating contract handed to every daemon-spawned
// orchestrator session, so a bare `tn` binary carries the contract with it
// instead of depending on this checkout being on disk at runtime. The file
// stays at the repo root because that path is documented and linked from
// the skill, the docs, and users' own configs; `//go:embed` can't reach a
// parent directory, so the embed lives here rather than in internal/tn.
//
// `tn serve` writes this copy to ~/.config/tn/ORCHESTRATOR.md at startup
// (see ensureManagedOrchestratorDoc in internal/tn/spawn.go); that managed
// file is generated, and the TN_ORCHESTRATOR_DOC / serve.json
// "orchestratorDoc" overrides are the way to run a customised contract.
package tasknotescli

import _ "embed"

// OrchestratorDoc is the byte-exact content of ORCHESTRATOR.md as of the
// build. TestEmbeddedOrchestratorDoc asserts it matches the file on disk.
//
//go:embed ORCHESTRATOR.md
var OrchestratorDoc []byte

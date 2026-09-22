package tn

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	tasknotescli "tasknotes-cli"
)

// TestEmbeddedOrchestratorDoc proves the binary actually carries the
// repo's ORCHESTRATOR.md: the embedded bytes must be byte-identical to the
// file at the repo root, so a stale or empty embed is a test failure, not a
// silently different contract handed to spawned sessions.
func TestEmbeddedOrchestratorDoc(t *testing.T) {
	onDisk, err := os.ReadFile(filepath.Join("..", "..", "ORCHESTRATOR.md"))
	if err != nil {
		t.Fatalf("read repo ORCHESTRATOR.md: %v", err)
	}
	if len(tasknotescli.OrchestratorDoc) == 0 {
		t.Fatal("embedded ORCHESTRATOR.md is empty")
	}
	if !bytes.HasPrefix(tasknotescli.OrchestratorDoc, []byte("# TaskNotes orchestrator protocol")) {
		t.Errorf("embedded doc does not start with the contract heading; got %q", firstLine(tasknotescli.OrchestratorDoc))
	}
	if !bytes.Equal(tasknotescli.OrchestratorDoc, onDisk) {
		t.Error("embedded ORCHESTRATOR.md differs from the repo-root file")
	}
}

func firstLine(b []byte) string {
	if i := bytes.IndexByte(b, '\n'); i >= 0 {
		return string(b[:i])
	}
	return string(b)
}

// TestEnsureManagedOrchestratorDoc covers the startup sync that lets a
// bare `tn` binary spawn orchestrators without a checkout on disk:
// ~/.config/tn/ORCHESTRATOR.md is written from the embedded copy when
// absent, refreshed when its content differs (an older binary's copy, or
// hand edits — the managed path is generated, the overrides are for
// customising), left alone when identical, and never touched when an
// override explicitly claims that same path as the user's own copy.
func TestEnsureManagedOrchestratorDoc(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("TN_ORCHESTRATOR_DOC", "")
	managed := filepath.Join(home, ".config", "tn", "ORCHESTRATOR.md")
	embedded := []byte("# contract v2\n")

	// Absent: written.
	path, written, err := ensureManagedOrchestratorDoc(ServeConfig{}, embedded)
	if err != nil {
		t.Fatalf("first sync: %v", err)
	}
	if path != managed {
		t.Errorf("expected managed path %q, got %q", managed, path)
	}
	if !written {
		t.Error("expected the managed doc to be written when absent")
	}
	got, err := os.ReadFile(managed)
	if err != nil {
		t.Fatalf("managed doc not on disk after sync: %v", err)
	}
	if !bytes.Equal(got, embedded) {
		t.Errorf("managed doc content = %q, want embedded %q", got, embedded)
	}

	// Identical: not rewritten.
	_, written, err = ensureManagedOrchestratorDoc(ServeConfig{}, embedded)
	if err != nil {
		t.Fatalf("second sync: %v", err)
	}
	if written {
		t.Error("expected no write when the managed doc already matches the embedded copy")
	}

	// Differs (stale copy from an older binary): refreshed.
	if err := os.WriteFile(managed, []byte("# contract v1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, written, err = ensureManagedOrchestratorDoc(ServeConfig{}, embedded)
	if err != nil {
		t.Fatalf("refresh sync: %v", err)
	}
	if !written {
		t.Error("expected a stale managed doc to be refreshed")
	}
	got, _ = os.ReadFile(managed)
	if !bytes.Equal(got, embedded) {
		t.Errorf("managed doc after refresh = %q, want %q", got, embedded)
	}

	// serve.json's orchestratorDoc pointing AT the managed path claims it
	// as the user's own edited copy: left untouched even though it differs.
	custom := []byte("# my edited contract\n")
	if err := os.WriteFile(managed, custom, 0o644); err != nil {
		t.Fatal(err)
	}
	_, written, err = ensureManagedOrchestratorDoc(ServeConfig{OrchestratorDoc: managed}, embedded)
	if err != nil {
		t.Fatalf("claimed-by-config sync: %v", err)
	}
	if written {
		t.Error("expected no write when serve.json orchestratorDoc claims the managed path")
	}
	got, _ = os.ReadFile(managed)
	if !bytes.Equal(got, custom) {
		t.Errorf("user-claimed managed doc was overwritten: got %q", got)
	}

	// Same via env TN_ORCHESTRATOR_DOC, spelled with an unclean path so
	// the comparison has to normalise rather than string-match.
	t.Setenv("TN_ORCHESTRATOR_DOC", filepath.Join(home, ".config", "tn", "..", "tn", "ORCHESTRATOR.md"))
	_, written, err = ensureManagedOrchestratorDoc(ServeConfig{}, embedded)
	if err != nil {
		t.Fatalf("claimed-by-env sync: %v", err)
	}
	if written {
		t.Error("expected no write when TN_ORCHESTRATOR_DOC claims the managed path")
	}
	got, _ = os.ReadFile(managed)
	if !bytes.Equal(got, custom) {
		t.Errorf("env-claimed managed doc was overwritten: got %q", got)
	}

	// An override pointing ELSEWHERE does not stop the managed copy from
	// being kept fresh as the fallback.
	t.Setenv("TN_ORCHESTRATOR_DOC", filepath.Join(home, "elsewhere.md"))
	_, written, err = ensureManagedOrchestratorDoc(ServeConfig{}, embedded)
	if err != nil {
		t.Fatalf("override-elsewhere sync: %v", err)
	}
	if !written {
		t.Error("expected the managed doc to be refreshed when the override points elsewhere")
	}
}

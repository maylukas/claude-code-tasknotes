package tn

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
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

// TestManagedDocHeaderRoundTrip pins the on-disk shape of a pristine
// managed copy: one header line carrying the embed's sha256, then the
// embed byte for byte, and parseManagedDoc recovering both.
func TestManagedDocHeaderRoundTrip(t *testing.T) {
	embedded := []byte("# contract\n\nbody\n")
	rendered := renderManagedDoc(embedded)
	if !bytes.HasPrefix(rendered, []byte(managedDocHeaderPrefix)) {
		t.Fatalf("rendered copy does not start with the header prefix: %q", firstLine(rendered))
	}
	if !bytes.HasSuffix(rendered, embedded) {
		t.Fatalf("rendered copy does not end with the embed: %q", rendered)
	}
	if !bytes.HasPrefix(rendered, []byte("<!--")) || !bytes.Contains([]byte(firstLine(rendered)), []byte("-->")) {
		t.Errorf("header is not a single-line HTML comment: %q", firstLine(rendered))
	}
	base, body, ok := parseManagedDoc(rendered)
	if !ok {
		t.Fatal("parseManagedDoc rejected its own output")
	}
	// A header written by a build with different wording after the hash
	// still parses: only prefix, hash, and a closed comment matter.
	reworded := []byte(managedDocHeaderPrefix + sha256Hex(string(embedded)) + " (older wording here) -->\n" + string(embedded))
	if b2, body2, ok2 := parseManagedDoc(reworded); !ok2 || b2 != base || !bytes.Equal(body2, embedded) {
		t.Errorf("reworded header not accepted: ok=%v base=%s body=%q", ok2, b2, body2)
	}
	if base != sha256Hex(string(embedded)) {
		t.Errorf("base = %s, want sha256 of the embed", base)
	}
	if !bytes.Equal(body, embedded) {
		t.Errorf("body = %q, want %q", body, embedded)
	}

	for name, in := range map[string][]byte{
		"headerless":                     embedded,
		"empty":                          {},
		"truncated hash":                 []byte(managedDocHeaderPrefix + "abc" + managedDocHeaderSuffix + "x"),
		"non-hex hash":                   []byte(managedDocHeaderPrefix + strings.Repeat("z", 64) + managedDocHeaderSuffix + "x"),
		"unterminated":                   []byte(managedDocHeaderPrefix + strings.Repeat("a", 64)),
		"comment not closed on the line": []byte(managedDocHeaderPrefix + strings.Repeat("a", 64) + " no close\nbody\n"),
	} {
		if _, body, ok := parseManagedDoc(in); ok {
			t.Errorf("%s: parseManagedDoc accepted %q", name, in)
		} else if !bytes.Equal(body, in) {
			t.Errorf("%s: body on reject = %q, want whole input", name, body)
		}
	}
}

// managedDocFixture puts HOME in a temp dir and returns the managed path,
// its .new sibling, and a reader that fails the test on any read error.
func managedDocFixture(t *testing.T) (managed, newPath string, read func(string) []byte) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	managed = filepath.Join(home, ".config", "tn", "ORCHESTRATOR.md")
	newPath = managed + managedDocNewSuffix
	read = func(p string) []byte {
		t.Helper()
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatalf("read %s: %v", p, err)
		}
		return b
	}
	return managed, newPath, read
}

func mustNotExist(t *testing.T, p string) {
	t.Helper()
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Errorf("expected %s to be absent, stat err = %v", p, err)
	}
}

func syncDoc(t *testing.T, embedded []byte) managedDocAction {
	t.Helper()
	_, action, err := ensureManagedOrchestratorDoc(embedded)
	if err != nil {
		t.Fatalf("ensureManagedOrchestratorDoc: %v", err)
	}
	return action
}

// TestEnsureManagedOrchestratorDoc_Pristine covers the paths where the
// user has not touched the file: it is created when absent, left alone
// when current, and refreshed when it is an untouched copy of an older
// contract — including the headerless copy the previous build wrote.
func TestEnsureManagedOrchestratorDoc_Pristine(t *testing.T) {
	managed, newPath, read := managedDocFixture(t)
	v1, v2 := []byte("# contract v1\n"), []byte("# contract v2\n")

	path, action, err := ensureManagedOrchestratorDoc(v1)
	if err != nil {
		t.Fatal(err)
	}
	if path != managed {
		t.Errorf("path = %q, want %q", path, managed)
	}
	if action != managedDocWritten {
		t.Errorf("absent: action = %v, want written", action)
	}
	if got := read(managed); !bytes.Equal(got, renderManagedDoc(v1)) {
		t.Errorf("absent: content = %q, want rendered v1", got)
	}

	if action := syncDoc(t, v1); action != managedDocUpToDate {
		t.Errorf("current: action = %v, want up to date", action)
	}

	if action := syncDoc(t, v2); action != managedDocWritten {
		t.Errorf("older pristine: action = %v, want written", action)
	}
	if got := read(managed); !bytes.Equal(got, renderManagedDoc(v2)) {
		t.Errorf("older pristine: content = %q, want rendered v2", got)
	}

	// The previous build wrote the raw embed with no header. Identical to
	// the current embed: it is pristine, and gains the header.
	if err := os.WriteFile(managed, v2, 0o644); err != nil {
		t.Fatal(err)
	}
	if action := syncDoc(t, v2); action != managedDocWritten {
		t.Errorf("headerless identical: action = %v, want written", action)
	}
	if got := read(managed); !bytes.Equal(got, renderManagedDoc(v2)) {
		t.Errorf("headerless identical: content = %q, want rendered v2", got)
	}

	// A stale .new beside a pristine file is removed on sync.
	if err := os.WriteFile(newPath, []byte("stale"), 0o644); err != nil {
		t.Fatal(err)
	}
	syncDoc(t, v2)
	mustNotExist(t, newPath)
}

// TestEnsureManagedOrchestratorDoc_Edited covers the user's side: an
// edited file is never overwritten, a newer contract is offered beside it
// as .new only once the embed has moved on from the base the edits were
// made on, and the .new is not churned once it is current.
func TestEnsureManagedOrchestratorDoc_Edited(t *testing.T) {
	managed, newPath, read := managedDocFixture(t)
	v1, v2, v3 := []byte("# contract v1\n"), []byte("# contract v2\n"), []byte("# contract v3\n")

	syncDoc(t, v1)
	// Edit the body but keep the header: base still says v1.
	edited := append(append([]byte{}, renderManagedDoc(v1)...), []byte("my extra rule\n")...)
	if err := os.WriteFile(managed, edited, 0o644); err != nil {
		t.Fatal(err)
	}

	// Same contract as the edits were based on: kept, no .new.
	if action := syncDoc(t, v1); action != managedDocKept {
		t.Errorf("edited on current: action = %v, want kept", action)
	}
	if got := read(managed); !bytes.Equal(got, edited) {
		t.Errorf("edited on current: file was changed to %q", got)
	}
	mustNotExist(t, newPath)

	// Contract moved on: kept, and .new carries the rendered new contract
	// so copying it over the managed file yields a pristine copy again.
	if action := syncDoc(t, v2); action != managedDocKeptNewer {
		t.Errorf("edited, newer embed: action = %v, want kept-newer", action)
	}
	if got := read(managed); !bytes.Equal(got, edited) {
		t.Errorf("edited, newer embed: file was changed to %q", got)
	}
	if got := read(newPath); !bytes.Equal(got, renderManagedDoc(v2)) {
		t.Errorf(".new = %q, want rendered v2", got)
	}

	// Same embed again: .new already current, still reported, untouched.
	before, _ := os.Stat(newPath)
	if action := syncDoc(t, v2); action != managedDocKeptNewer {
		t.Errorf("repeat: action = %v, want kept-newer", action)
	}
	after, _ := os.Stat(newPath)
	if !before.ModTime().Equal(after.ModTime()) {
		t.Error("repeat: .new was rewritten although already current")
	}

	// Yet another contract: .new is replaced.
	if action := syncDoc(t, v3); action != managedDocKeptNewer {
		t.Errorf("third contract: action = %v, want kept-newer", action)
	}
	if got := read(newPath); !bytes.Equal(got, renderManagedDoc(v3)) {
		t.Errorf(".new = %q, want rendered v3", got)
	}

	// The user resolves by copying .new over the file: pristine again, and
	// the next sync clears the leftover .new.
	if err := os.WriteFile(managed, read(newPath), 0o644); err != nil {
		t.Fatal(err)
	}
	if action := syncDoc(t, v3); action != managedDocUpToDate {
		t.Errorf("resolved: action = %v, want up to date", action)
	}
	mustNotExist(t, newPath)

	// A headerless file that is NOT the current embed (hand-made, or the
	// previous build's copy of an older contract — indistinguishable) is
	// treated as edited with an unknown base: kept, .new written.
	if err := os.WriteFile(managed, []byte("# hand-made contract\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if action := syncDoc(t, v3); action != managedDocKeptNewer {
		t.Errorf("headerless different: action = %v, want kept-newer", action)
	}
	if got := read(managed); string(got) != "# hand-made contract\n" {
		t.Errorf("headerless different: file was changed to %q", got)
	}
	if got := read(newPath); !bytes.Equal(got, renderManagedDoc(v3)) {
		t.Errorf("headerless different: .new = %q, want rendered v3", got)
	}

	// An override elsewhere does not change any of this.
	t.Setenv("TN_ORCHESTRATOR_DOC", filepath.Join(filepath.Dir(managed), "elsewhere.md"))
	if action := syncDoc(t, v3); action != managedDocKeptNewer {
		t.Errorf("with override: action = %v, want kept-newer", action)
	}
}

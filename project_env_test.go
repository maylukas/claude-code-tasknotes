package main

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

// TestEnvEntryUnmarshalJSON_BothShapes verifies serve.json accepts either a
// plain string (treated as Value, no Description) or the
// {"value":..., "description":...} object form for each env entry.
func TestEnvEntryUnmarshalJSON_BothShapes(t *testing.T) {
	var cfg struct {
		Env map[string]EnvEntry `json:"env"`
	}
	raw := `{
		"env": {
			"PLAIN_STRING": "sk-abc123",
			"OBJECT_WITH_VALUE": {"value": "sk-def456", "description": "injected secret"},
			"ADVISORY_ONLY": {"description": "run: op read op://vault/item/field"}
		}
	}`
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	plain, ok := cfg.Env["PLAIN_STRING"]
	if !ok || plain.Value != "sk-abc123" || plain.Description != "" {
		t.Errorf("PLAIN_STRING = %+v, want Value=sk-abc123 Description=\"\"", plain)
	}

	obj, ok := cfg.Env["OBJECT_WITH_VALUE"]
	if !ok || obj.Value != "sk-def456" || obj.Description != "injected secret" {
		t.Errorf("OBJECT_WITH_VALUE = %+v", obj)
	}

	advisory, ok := cfg.Env["ADVISORY_ONLY"]
	if !ok || advisory.Value != "" || advisory.Description != "run: op read op://vault/item/field" {
		t.Errorf("ADVISORY_ONLY = %+v, want empty Value", advisory)
	}
}

// TestEnvEntryUnmarshalJSON_ViaServeFileConfig verifies the shapes parse
// correctly through the real serve.json load path (ProjectConfig.Env inside
// serveFileConfig.Projects), not just in isolation.
func TestEnvEntryUnmarshalJSON_ViaServeFileConfig(t *testing.T) {
	var fc serveFileConfig
	raw := `{
		"projects": {
			"myapp": {
				"autoSpawn": true,
				"cwd": "/repos/myapp",
				"env": {
					"API_KEY": "sk-live-123",
					"DB_PASSWORD": {"description": "fetch: op read op://vault/db/password"}
				}
			}
		}
	}`
	if err := json.Unmarshal([]byte(raw), &fc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	cfg, ok := fc.Projects["myapp"]
	if !ok {
		t.Fatalf("expected project 'myapp' in %+v", fc.Projects)
	}
	if cfg.Env["API_KEY"].Value != "sk-live-123" {
		t.Errorf("API_KEY = %+v", cfg.Env["API_KEY"])
	}
	if cfg.Env["DB_PASSWORD"].Value != "" || cfg.Env["DB_PASSWORD"].Description == "" {
		t.Errorf("DB_PASSWORD = %+v, want empty value with a description", cfg.Env["DB_PASSWORD"])
	}
}

// TestInjectedEnv verifies only entries with a non-empty Value are
// returned; advisory (empty-value) entries are excluded.
func TestInjectedEnv(t *testing.T) {
	cfg := ProjectConfig{
		Env: map[string]EnvEntry{
			"INJECTED_ONE":   {Value: "v1"},
			"INJECTED_TWO":   {Value: "v2", Description: "has a description too"},
			"ADVISORY_EMPTY": {Description: "fetch this yourself"},
		},
	}
	got := injectedEnv(cfg)
	if len(got) != 2 {
		t.Fatalf("expected 2 injected entries, got %d: %+v", len(got), got)
	}
	if got["INJECTED_ONE"] != "v1" || got["INJECTED_TWO"] != "v2" {
		t.Errorf("unexpected injected values: %+v", got)
	}
	if _, ok := got["ADVISORY_EMPTY"]; ok {
		t.Errorf("expected ADVISORY_EMPTY to be excluded, got %+v", got)
	}
}

// TestBuildLaunchCommand_EnvInjection verifies injected env vars are
// prefixed onto the launch line in sorted order, each shell-quoted,
// preceding TN_HEADLESS=1.
func TestBuildLaunchCommand_EnvInjection(t *testing.T) {
	env := map[string]string{
		"ZEBRA_VAR": "z-value",
		"ALPHA_VAR": "a value with spaces",
		"BETA_VAR":  "b's quote",
	}
	cmd := buildLaunchCommand("claude", "orchestrator-proj-g1", "the prompt", env)

	want := `ALPHA_VAR='a value with spaces' BETA_VAR='b'\''s quote' ZEBRA_VAR='z-value' TN_HEADLESS=1 TN_AGENT_NAME='orchestrator-proj-g1' claude --permission-mode auto 'the prompt'`
	if cmd != want {
		t.Errorf("got %q, want %q", cmd, want)
	}
}

// TestBuildLaunchCommand_NoEnvUnaffected verifies an empty/nil env produces
// exactly the same output as before this feature (no stray leading space).
func TestBuildLaunchCommand_NoEnvUnaffected(t *testing.T) {
	cmd := buildLaunchCommand("claude", "orchestrator-proj-g1", "hi", map[string]string{})
	want := "TN_HEADLESS=1 TN_AGENT_NAME='orchestrator-proj-g1' claude --permission-mode auto 'hi'"
	if cmd != want {
		t.Errorf("got %q, want %q", cmd, want)
	}
}

// TestHandleProjectsEnv_SingleProjectNeverReturnsValues verifies the
// endpoint returns injected/advisory flags and descriptions but never a
// value field, for a single project.
func TestHandleProjectsEnv_SingleProjectNeverReturnsValues(t *testing.T) {
	cfg := ServeConfig{
		Port: 0,
		Projects: map[string]ProjectConfig{
			"myapp": {
				AutoSpawn: true,
				Env: map[string]EnvEntry{
					"API_KEY":     {Value: "sk-super-secret"},
					"DB_PASSWORD": {Description: "fetch: op read op://vault/db/password"},
				},
			},
		},
	}
	ts := newTestServerWithConfig(t, cfg, func(project, cwd string, env map[string]string) error { return nil })

	resp, err := http.Get(ts.URL + "/projects/env?project=myapp")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	body := string(bodyBytes)
	if strings.Contains(body, "sk-super-secret") {
		t.Fatalf("response leaked a secret value: %s", body)
	}

	var out struct {
		Env []envItem `json:"env"`
	}
	if err := json.Unmarshal(bodyBytes, &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Env) != 2 {
		t.Fatalf("expected 2 entries, got %d: %+v", len(out.Env), out.Env)
	}

	byName := map[string]envItem{}
	for _, e := range out.Env {
		byName[e.Name] = e
	}
	if !byName["API_KEY"].Injected {
		t.Errorf("expected API_KEY injected=true, got %+v", byName["API_KEY"])
	}
	if byName["DB_PASSWORD"].Injected {
		t.Errorf("expected DB_PASSWORD injected=false, got %+v", byName["DB_PASSWORD"])
	}
	if byName["DB_PASSWORD"].Description == "" {
		t.Errorf("expected DB_PASSWORD to keep its description, got %+v", byName["DB_PASSWORD"])
	}
	for _, e := range out.Env {
		if e.Project != "myapp" {
			t.Errorf("expected project=myapp on every entry, got %+v", e)
		}
	}
}

// TestHandleProjectsEnv_NoProjectListsAll verifies omitting the project
// query param returns entries for every configured project, tagged with
// their project slug.
func TestHandleProjectsEnv_NoProjectListsAll(t *testing.T) {
	cfg := ServeConfig{
		Port: 0,
		Projects: map[string]ProjectConfig{
			"myapp": {Env: map[string]EnvEntry{"A": {Value: "1"}}},
			"foo":   {Env: map[string]EnvEntry{"B": {Description: "advisory"}}},
		},
	}
	ts := newTestServerWithConfig(t, cfg, func(project, cwd string, env map[string]string) error { return nil })

	resp, err := http.Get(ts.URL + "/projects/env")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	var out struct {
		Env []envItem `json:"env"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if len(out.Env) != 2 {
		t.Fatalf("expected 2 entries across both projects, got %d: %+v", len(out.Env), out.Env)
	}
	projects := map[string]bool{}
	for _, e := range out.Env {
		projects[e.Project] = true
	}
	if !projects["myapp"] || !projects["foo"] {
		t.Errorf("expected entries from both projects, got %+v", out.Env)
	}
}

// TestCmdEnvWiring verifies BridgeClient.ProjectsEnv reaches
// GET /projects/env and the response decodes into the expected shape.
func TestCmdEnvWiring(t *testing.T) {
	cfg := ServeConfig{
		Port: 0,
		Projects: map[string]ProjectConfig{
			"myapp": {Env: map[string]EnvEntry{
				"API_KEY": {Value: "sk-abc"},
			}},
		},
	}
	ts := newTestServerWithConfig(t, cfg, func(project, cwd string, env map[string]string) error { return nil })

	b := NewBridgeClient(ts.URL)
	body, err := b.ProjectsEnv("myapp")
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		Env []envItem `json:"env"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Env) != 1 || out.Env[0].Name != "API_KEY" || !out.Env[0].Injected {
		t.Fatalf("unexpected response: %+v", out.Env)
	}
}

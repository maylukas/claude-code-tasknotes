// Client helpers and subcommands for talking to the `tn serve` bridge
// daemon (as opposed to the TaskNotes HTTP API itself, see api.go).
package tn

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// resolveBridgeURL returns the configured bridge base URL: env
// TN_BRIDGE_URL, or http://localhost:8391 by default.
func resolveBridgeURL() string {
	if v := os.Getenv("TN_BRIDGE_URL"); v != "" {
		return strings.TrimRight(v, "/")
	}
	return "http://localhost:8391"
}

// bridgeUnreachableError represents a failure to reach the bridge daemon.
type bridgeUnreachableError struct {
	URL string
}

func (e *bridgeUnreachableError) Error() string {
	return fmt.Sprintf("bridge not reachable at %s — start it with 'tn serve'", e.URL)
}

// exitCodeError carries a specific process exit code alongside a message,
// for cases (like `tn send --wait` timing out) that need something other
// than the default exit 1.
type exitCodeError struct {
	code int
	msg  string
}

func (e *exitCodeError) Error() string { return e.msg }

// BridgeClient is a minimal HTTP client for the tn serve bridge daemon.
// Timeout is generous since /inbox long-polls for up to 55s server-side.
type BridgeClient struct {
	BaseURL string
	HTTP    *http.Client
}

func NewBridgeClient(baseURL string) *BridgeClient {
	return &BridgeClient{BaseURL: baseURL, HTTP: &http.Client{Timeout: 65 * time.Second}}
}

func (b *BridgeClient) request(method, path string, body any) ([]byte, error) {
	var reader io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		reader = bytes.NewReader(buf)
	}

	req, err := http.NewRequest(method, b.BaseURL+path, reader)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := b.HTTP.Do(req)
	if err != nil {
		if isConnRefused(err) {
			return nil, &bridgeUnreachableError{URL: b.BaseURL}
		}
		return nil, err
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, &httpError{Code: resp.StatusCode, Body: string(respBody)}
	}
	return respBody, nil
}

func (b *BridgeClient) Register(name, project, sessionID, tmuxSession string) ([]byte, error) {
	body := map[string]any{"name": name, "project": project}
	if sessionID != "" {
		body["sessionId"] = sessionID
	}
	if tmuxSession != "" {
		body["tmuxSession"] = tmuxSession
	}
	return b.request(http.MethodPost, "/agents/register", body)
}

// Drain marks name as no longer accepting new task assignments (generations
// & drain, see SPEC-generations.md) — idempotent.
func (b *BridgeClient) Drain(name string) ([]byte, error) {
	return b.request(http.MethodPost, "/agents/drain", map[string]any{"name": name})
}

// Retire implements daemon-mediated self-termination (SPEC-generations.md):
// the daemon verifies no pending messages remain, marks the agent
// deregistered, and kills its tmux session shortly after responding.
// Returns an *httpError (via request) on a 409 ("pending messages" — ack
// everything first) or 404 (unknown agent).
func (b *BridgeClient) Retire(name string) ([]byte, error) {
	return b.request(http.MethodPost, "/agents/retire", map[string]any{"name": name})
}

// SetSpawnPaused implements the manual spawn kill-switch: POST
// /spawn/pause {"paused": bool}. While paused, every spawn path skips
// actually spawning; everything else keeps working normally.
func (b *BridgeClient) SetSpawnPaused(paused bool) ([]byte, error) {
	return b.request(http.MethodPost, "/spawn/pause", map[string]any{"paused": paused})
}

func (b *BridgeClient) Inbox(name string, wait int) ([]byte, error) {
	q := url.Values{}
	q.Set("name", name)
	if wait > 0 {
		q.Set("wait", strconv.Itoa(wait))
	}
	return b.request(http.MethodGet, "/inbox?"+q.Encode(), nil)
}

func (b *BridgeClient) Ack(name, msgID, response string) ([]byte, error) {
	body := map[string]any{"name": name, "msgId": msgID}
	if response != "" {
		body["response"] = response
	}
	return b.request(http.MethodPost, "/ack", body)
}

// WorkerStart declares name is running a worker on taskPath — see
// SPEC-ui-v2.md #2. Idempotent: calling it again for the same
// (name, taskPath) just refreshes the started-at time, not a duplicate.
func (b *BridgeClient) WorkerStart(name, taskPath string) ([]byte, error) {
	return b.request(http.MethodPost, "/workers/start", map[string]any{"name": name, "taskPath": taskPath})
}

// WorkerEnd retracts a previously declared worker.
func (b *BridgeClient) WorkerEnd(name, taskPath string) ([]byte, error) {
	return b.request(http.MethodPost, "/workers/end", map[string]any{"name": name, "taskPath": taskPath})
}

func (b *BridgeClient) SendMessage(to, project, task, from, text string) ([]byte, error) {
	body := map[string]any{"text": text}
	if to != "" {
		body["to"] = to
	}
	if project != "" {
		body["project"] = project
	}
	if task != "" {
		body["taskPath"] = task
	}
	if from != "" {
		body["from"] = from
	}
	return b.request(http.MethodPost, "/messages", body)
}

// CreateTaskViaBridge calls POST /tasks/create — the daemon proxies to
// TaskNotes' own POST /api/tasks, but with idempotency: a repeat call with
// the same non-empty idempotencyKey returns the ORIGINAL task instead of
// creating a second one, making a `tn create` retry after a client-side
// timeout harmless (see handleCreateTask). Uses the same generous 65s
// client timeout as every other bridge call — the daemon's own call to
// TaskNotes is the one that can be slow, and this local, always-up
// daemon round trip is not.
func (b *BridgeClient) CreateTaskViaBridge(idempotencyKey string, fields map[string]any) ([]byte, error) {
	body := map[string]any{"fields": fields}
	if idempotencyKey != "" {
		body["idempotencyKey"] = idempotencyKey
	}
	return b.request(http.MethodPost, "/tasks/create", body)
}

// CloseTaskViaBridge calls POST /tasks/close — mechanism 2's synchronous
// check: the daemon fetches path, checks any DONE MEANS criteria against
// Evidence lines already in its Details, and only writes status=done if
// they're satisfied (or there's no DONE MEANS block at all). A non-nil
// *httpError with Code 422 means the criteria were checked and found
// unmet — the close did NOT happen; the caller should surface that
// rejection, not fall back to a direct write (see handleCloseTask,
// closeTaskChecked). Same generous 65s timeout as every other bridge call.
func (b *BridgeClient) CloseTaskViaBridge(path string) ([]byte, error) {
	return b.request(http.MethodPost, "/tasks/close", map[string]any{"path": path})
}

func (b *BridgeClient) GetMessage(id string) ([]byte, error) {
	q := url.Values{}
	q.Set("id", id)
	return b.request(http.MethodGet, "/messages?"+q.Encode(), nil)
}

func (b *BridgeClient) ListAgents() ([]byte, error) {
	return b.request(http.MethodGet, "/agents", nil)
}

// ProjectsEnv fetches env entries for project, or for every configured
// project when project is empty.
func (b *BridgeClient) ProjectsEnv(project string) ([]byte, error) {
	path := "/projects/env"
	if project != "" {
		q := url.Values{}
		q.Set("project", project)
		path += "?" + q.Encode()
	}
	return b.request(http.MethodGet, path, nil)
}

func (b *BridgeClient) LogActivity(name, text string) ([]byte, error) {
	return b.request(http.MethodPost, "/activity", map[string]any{"name": name, "text": text})
}

func (b *BridgeClient) RepoSettings(slug string) ([]byte, error) {
	q := url.Values{}
	q.Set("slug", slug)
	return b.request(http.MethodGet, "/repos?"+q.Encode(), nil)
}

// --- Subcommands ---

// validatePositionalText enforces that a positional text argument (`tn
// note`'s <text>, `tn log`'s "text", `tn send`'s TEXT) is present,
// non-empty after trimming whitespace, and doesn't look like a flag. A
// flag-like positional almost always means a malformed call — a required
// text argument got omitted and a flag/its value ended up parsed into the
// text slot instead — and continuing silently would store or send garbage
// content rather than fail loudly. (Live incident: `tn note <path> --by X`
// with no text at all stored the literal string "--by" as the note body,
// swallowing two review-evidence notes.)
func validatePositionalText(text string) error {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return fmt.Errorf("text argument is required")
	}
	if strings.HasPrefix(trimmed, "--") {
		return fmt.Errorf("text argument %q looks like a flag, not text — check your argument order", trimmed)
	}
	return nil
}

// extractStringFlag pulls a "--name value" or "--name=value" pair out of
// args wherever it appears (not just at the front), returning its value
// (or "" if absent) and the remaining args in their original relative
// order. Go's stdlib flag package only supports flags BEFORE positionals —
// this is what let `tn note <path> --by X` (with no text argument at all)
// silently store "--by" as the note body: positional slicing grabbed
// args[1] before flag parsing ever got a chance to see it. Extracting the
// one optional flag manually lets it appear either before or after the
// positional text.
func extractStringFlag(args []string, name string) (value string, rest []string) {
	prefix := "--" + name
	rest = make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == prefix && i+1 < len(args) {
			value = args[i+1]
			i++
			continue
		}
		if v, ok := strings.CutPrefix(a, prefix+"="); ok {
			value = v
			continue
		}
		rest = append(rest, a)
	}
	return value, rest
}

func cmdRegister(args []string) error {
	fs := flag.NewFlagSet("register", flag.ExitOnError)
	name := fs.String("name", "", "agent name (required)")
	project := fs.String("project", "", "project slug (required)")
	session := fs.String("session", "", "session id")
	tmuxSession := fs.String("tmux-session", "", "tmux session this agent is running in")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *name == "" || *project == "" {
		return fmt.Errorf("usage: tn register --name N --project P [--session ID] [--tmux-session S]")
	}

	b := NewBridgeClient(resolveBridgeURL())
	body, err := b.Register(*name, *project, *session, *tmuxSession)
	if err != nil {
		return err
	}
	printRawJSON(body)
	return nil
}

// cmdDrain implements `tn drain --name N`: stop accepting new task
// assignments (generations & drain, see SPEC-generations.md) — finish what
// you own, keep answering task-scoped messages and acking, then exit.
func cmdDrain(args []string) error {
	fs := flag.NewFlagSet("drain", flag.ExitOnError)
	name := fs.String("name", "", "agent name (required)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *name == "" {
		return fmt.Errorf("usage: tn drain --name N")
	}

	b := NewBridgeClient(resolveBridgeURL())
	body, err := b.Drain(*name)
	if err != nil {
		return err
	}
	printRawJSON(body)
	return nil
}

// cmdRetire implements `tn retire --name N`: your FINAL command as a
// draining generation (SPEC-generations.md). Fails if you still have
// pending (unacked) messages — ack everything first.
func cmdRetire(args []string) error {
	fs := flag.NewFlagSet("retire", flag.ExitOnError)
	name := fs.String("name", "", "agent name (required)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *name == "" {
		return fmt.Errorf("usage: tn retire --name N")
	}

	b := NewBridgeClient(resolveBridgeURL())
	body, err := b.Retire(*name)
	if err != nil {
		return err
	}
	printRawJSON(body)
	return nil
}

// cmdPause implements `tn pause`: stop the daemon from spawning any new
// orchestrator generations, project-wide, until `tn resume`. Everything
// else (routing, drain, retire, MR watcher, stuck-prompt dialogs) keeps
// working normally.
func cmdPause(args []string) error {
	b := NewBridgeClient(resolveBridgeURL())
	body, err := b.SetSpawnPaused(true)
	if err != nil {
		return err
	}
	printRawJSON(body)
	return nil
}

// cmdResume implements `tn resume`: undo `tn pause`.
func cmdResume(args []string) error {
	b := NewBridgeClient(resolveBridgeURL())
	body, err := b.SetSpawnPaused(false)
	if err != nil {
		return err
	}
	printRawJSON(body)
	return nil
}

// inboxSummaryMaxRunes bounds how much of a message's text `tn inbox`
// prints per line. Agents consume the inbox through a persistent Monitor,
// and a Monitor event line is a NOTIFICATION channel, not a data channel —
// an unbounded line (team-lead messages run 1500-1900 chars) gets
// truncated before the model ever sees it, so the agent acts on a
// mid-sentence fragment while believing it received the whole message.
// `tn inbox` must never emit an unbounded line; `tn msg <id>` is the way
// to fetch the rest.
const inboxSummaryMaxRunes = 120

// summarizeInboxText renders text for a single `tn inbox` line: unchanged
// if it already fits inboxSummaryMaxRunes, otherwise cut to that many
// runes plus an explicit "…(N more chars, run `tn msg <id>`)" marker — the
// marker names the exact command to run, not just that more exists.
func summarizeInboxText(id, text string) string {
	r := []rune(text)
	if len(r) <= inboxSummaryMaxRunes {
		return text
	}
	more := len(r) - inboxSummaryMaxRunes
	return string(r[:inboxSummaryMaxRunes]) + fmt.Sprintf("…(%d more chars, run `tn msg %s`)", more, id)
}

func cmdInbox(args []string) error {
	fs := flag.NewFlagSet("inbox", flag.ExitOnError)
	name := fs.String("name", "", "agent name (required)")
	wait := fs.Int("wait", 0, "long-poll wait seconds (default 25, max 55)")
	asJSON := fs.Bool("json", false, "print raw JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *name == "" {
		return fmt.Errorf("usage: tn inbox --name N [--wait S] [--json]")
	}

	b := NewBridgeClient(resolveBridgeURL())
	body, err := b.Inbox(*name, *wait)
	if err != nil {
		return err
	}
	if *asJSON {
		printRawJSON(body)
		return nil
	}

	var resp struct {
		Messages []*Message `json:"messages"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return err
	}
	for _, m := range resp.Messages {
		fromOrProject := m.From
		if fromOrProject == "" {
			fromOrProject = m.Project
		}
		taskPath := valueOrDash(m.TaskPath)
		fmt.Printf("%s\t%s\t%s\t%s\n", m.ID, fromOrProject, taskPath, summarizeInboxText(m.ID, m.Text))
	}
	return nil
}

// cmdMsg implements `tn msg <id>`: prints one message's full, untruncated
// fields — the companion to `tn inbox`'s summary line, for whenever a line
// carries the "…(N more chars…)" marker or an agent otherwise needs the
// complete text/response.
func cmdMsg(args []string) error {
	fs := flag.NewFlagSet("msg", flag.ExitOnError)
	asJSON := fs.Bool("json", false, "print raw JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("usage: tn msg [--json] <id>")
	}
	id := fs.Arg(0)

	b := NewBridgeClient(resolveBridgeURL())
	body, err := b.GetMessage(id)
	if err != nil {
		return err
	}
	if *asJSON {
		printRawJSON(body)
		return nil
	}

	var resp struct {
		Message Message `json:"message"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return err
	}
	m := resp.Message
	fromOrProject := m.From
	if fromOrProject == "" {
		fromOrProject = m.Project
	}
	fmt.Printf("id: %s\n", m.ID)
	fmt.Printf("from: %s\n", valueOrDash(fromOrProject))
	fmt.Printf("to: %s\n", valueOrDash(m.To))
	fmt.Printf("taskPath: %s\n", valueOrDash(m.TaskPath))
	fmt.Printf("status: %s\n", m.Status)
	fmt.Printf("text: %s\n", m.Text)
	if m.Response != "" {
		fmt.Printf("response: %s\n", m.Response)
	}
	return nil
}

func cmdAck(args []string) error {
	fs := flag.NewFlagSet("ack", flag.ExitOnError)
	name := fs.String("name", "", "agent name (required)")
	msgID := fs.String("msg", "", "message id (required)")
	response := fs.String("response", "", "response text")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *name == "" || *msgID == "" {
		return fmt.Errorf(`usage: tn ack --name N --msg ID [--response "text"]`)
	}

	b := NewBridgeClient(resolveBridgeURL())
	_, err := b.Ack(*name, *msgID, *response)
	return err
}

// cmdWorker implements `tn worker start|end --name N --task P`: an
// orchestrator declaring (or retracting) a worker subtask, purely for
// /status visibility (SPEC-ui-v2.md #2) — the daemon has no way to
// observe a worker subagent on its own.
func cmdWorker(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf(`usage: tn worker start|end --name N --task P`)
	}
	action, rest := args[0], args[1:]
	if action != "start" && action != "end" {
		return fmt.Errorf(`usage: tn worker start|end --name N --task P (got %q)`, action)
	}

	fs := flag.NewFlagSet("worker "+action, flag.ExitOnError)
	name := fs.String("name", "", "orchestrator agent name (required)")
	task := fs.String("task", "", "worker's task path (required)")
	if err := fs.Parse(rest); err != nil {
		return err
	}
	if *name == "" || *task == "" {
		return fmt.Errorf(`usage: tn worker %s --name N --task P`, action)
	}

	b := NewBridgeClient(resolveBridgeURL())
	if action == "start" {
		_, err := b.WorkerStart(*name, *task)
		return err
	}
	_, err := b.WorkerEnd(*name, *task)
	return err
}

func cmdSend(args []string) error {
	fs := flag.NewFlagSet("send", flag.ExitOnError)
	to := fs.String("to", "", "target agent name")
	project := fs.String("project", "", "target project")
	task := fs.String("task", "", "task path")
	from := fs.String("from", "", "sender name")
	wait := fs.Int("wait", 0, "poll for an ack up to S seconds")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() < 1 {
		return fmt.Errorf("usage: tn send [--to AGENT | --project P] [--task PATH] [--from NAME] [--wait S] TEXT")
	}
	text := strings.Join(fs.Args(), " ")
	if err := validatePositionalText(text); err != nil {
		return fmt.Errorf("usage: tn send [--to AGENT | --project P] [--task PATH] [--from NAME] [--wait S] TEXT: %v", err)
	}

	b := NewBridgeClient(resolveBridgeURL())
	body, err := b.SendMessage(*to, *project, *task, *from, text)
	if err != nil {
		return err
	}

	var resp struct {
		Message *Message `json:"message"`
		Warning string   `json:"warning,omitempty"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return err
	}
	if resp.Warning != "" {
		fmt.Fprintf(os.Stderr, "tn: %s\n", resp.Warning)
	}
	if resp.Message == nil {
		return nil
	}
	fmt.Println(resp.Message.ID)

	if *wait <= 0 {
		return nil
	}
	return waitForAck(b, resp.Message.ID, *wait)
}

// waitForAck polls GET /messages?id= every 2s until the message is acked or
// wait seconds elapse.
func waitForAck(b *BridgeClient, msgID string, wait int) error {
	deadline := time.Now().Add(time.Duration(wait) * time.Second)
	for {
		body, err := b.GetMessage(msgID)
		if err != nil {
			return err
		}
		var resp struct {
			Message *Message `json:"message"`
		}
		if err := json.Unmarshal(body, &resp); err != nil {
			return err
		}
		if resp.Message != nil && resp.Message.Status == "acked" {
			fmt.Println(resp.Message.Response)
			return nil
		}
		if time.Now().After(deadline) {
			return &exitCodeError{code: 3, msg: "timed out waiting for ack"}
		}
		time.Sleep(2 * time.Second)
	}
}

func cmdLog(args []string) error {
	fs := flag.NewFlagSet("log", flag.ExitOnError)
	name := fs.String("name", "", "agent name (required)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() < 1 {
		return fmt.Errorf(`usage: tn log --name N "text"`)
	}
	if *name == "" {
		return fmt.Errorf(`usage: tn log --name N "text"`)
	}
	text := strings.Join(fs.Args(), " ")
	if err := validatePositionalText(text); err != nil {
		return fmt.Errorf(`usage: tn log --name N "text": %v`, err)
	}

	b := NewBridgeClient(resolveBridgeURL())
	_, err := b.LogActivity(*name, text)
	return err
}

func cmdAgents(args []string) error {
	fs := flag.NewFlagSet("agents", flag.ExitOnError)
	asJSON := fs.Bool("json", false, "print raw JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}

	b := NewBridgeClient(resolveBridgeURL())
	body, err := b.ListAgents()
	if err != nil {
		return err
	}
	if *asJSON {
		printRawJSON(body)
		return nil
	}

	var resp struct {
		Agents []agentView `json:"agents"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return err
	}
	for _, a := range resp.Agents {
		status := "🟢 accepting"
		if !a.Accepting {
			status = "🌀 draining"
		}
		if a.ContextPct != nil {
			status = fmt.Sprintf("%s (%.0f%%)", status, *a.ContextPct)
		}
		fmt.Printf("%s\t%s\t%t\t%s\t%s\n", a.Name, a.Project, a.Alive, status, a.LastSeenAt.Format(time.RFC3339))
	}
	return nil
}

func cmdEnv(args []string) error {
	fs := flag.NewFlagSet("env", flag.ExitOnError)
	project := fs.String("project", "", "limit to one project slug")
	asJSON := fs.Bool("json", false, "print raw JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}

	b := NewBridgeClient(resolveBridgeURL())
	body, err := b.ProjectsEnv(*project)
	if err != nil {
		return err
	}
	if *asJSON {
		printRawJSON(body)
		return nil
	}

	var resp struct {
		Env []envItem `json:"env"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return err
	}
	for _, e := range resp.Env {
		status := "advisory"
		if e.Injected {
			status = "injected"
		}
		fmt.Printf("%s\t%s\t%s\t%s\n", e.Project, e.Name, status, e.Description)
	}
	return nil
}

// cmdRepo implements `tn repo <slug>`: the daemon's resolved view of a
// project's repo-note parallelism settings (repoSettingsFor) plus its
// actual configured cwd/autoSpawn — surfacing state the daemon already
// holds, structured, rather than an agent parsing frontmatter out of a
// vault note it may not even be able to read (TCC). See repoSettingsView
// for why the *Set fields matter: a resolved value alone can't tell you
// whether the repo note set it or you're looking at a fallback/default.
func cmdRepo(args []string) error {
	fs := flag.NewFlagSet("repo", flag.ExitOnError)
	asJSON := fs.Bool("json", false, "print raw JSON")

	// slug is the leading positional argument; flags may appear after it
	// — same pattern as cmdGet, since flag.Parse stops at the first
	// non-flag token.
	if len(args) < 1 {
		return fmt.Errorf("usage: tn repo <slug> [--json]")
	}
	slug := args[0]
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}

	b := NewBridgeClient(resolveBridgeURL())
	body, err := b.RepoSettings(slug)
	if err != nil {
		return err
	}
	if *asJSON {
		printRawJSON(body)
		return nil
	}

	var view repoSettingsView
	if err := json.Unmarshal(body, &view); err != nil {
		return err
	}
	fmt.Printf("slug:            %s\n", view.Slug)
	fmt.Printf("repoPath:        %s\n", view.RepoPath)
	fmt.Printf("autoSpawn:       %t\n", view.AutoSpawn)
	fmt.Printf("maxOrchestrators: %d%s\n", view.MaxOrchestrators, setSuffix(view.MaxOrchestratorsSet))
	fmt.Printf("maxWorkers:      %d%s\n", view.MaxWorkers, setSuffix(view.MaxWorkersSet))
	if view.MaxWorkersServeJSONFallback != "" {
		fmt.Printf("  (serve.json TN_MAX_WORKERS fallback, used when the repo note doesn't set max-workers: %s)\n", view.MaxWorkersServeJSONFallback)
	}
	return nil
}

// setSuffix renders " (set by repo note)" or " (unset, default)" — the
// human-readable form of a repoSettingsView *Set field, so `tn repo`'s
// plain-text output carries the same unset-vs-default distinction its
// JSON does.
func setSuffix(set bool) string {
	if set {
		return " (set by repo note)"
	}
	return " (unset, default)"
}

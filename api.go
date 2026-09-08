package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// Task is the JSON representation of a TaskNotes task. Fields are tolerant
// of being absent in the API response (they just take their zero value).
type Task struct {
	Path             string            `json:"path,omitempty"`
	Title            string            `json:"title,omitempty"`
	Status           string            `json:"status,omitempty"`
	Priority         string            `json:"priority,omitempty"`
	Due              string            `json:"due,omitempty"`
	Scheduled        string            `json:"scheduled,omitempty"`
	Tags             []string          `json:"tags,omitempty"`
	Projects         []string          `json:"projects,omitempty"`
	Contexts         []string          `json:"contexts,omitempty"`
	Details          string            `json:"details,omitempty"`
	CustomProperties map[string]string `json:"customProperties,omitempty"`
	BlockedBy        []TaskDependency  `json:"blockedBy,omitempty"`
	// IsBlocked reflects TaskNotes' own computed dependency state (whether
	// any entry in BlockedBy is itself incomplete). Only meaningful when
	// the task came from a live fetch (GetTask/QueryTasksRaw) — a webhook
	// payload may or may not include it.
	IsBlocked bool `json:"isBlocked,omitempty"`
	// DateCreated is TaskNotes' own creation timestamp (RFC3339 with
	// fractional seconds, e.g. "2026-08-21T11:02:46.319+02:00" — parse
	// with time.RFC3339Nano, not time.RFC3339). Only meaningful on a live
	// fetch (GetTask/QueryTasksRaw); used by the create-idempotency
	// ambiguous-failure lookup to bound how recent a "same title" match
	// must be to count as the SAME create, not a coincidentally
	// identically-titled older task.
	DateCreated string `json:"dateCreated,omitempty"`
}

// TaskDependency is one entry in Task.BlockedBy: a reference to another
// task this one depends on.
type TaskDependency struct {
	UID     string `json:"uid"`
	Reltype string `json:"reltype"`
}

// FilterCondition is a single leaf condition in a TaskNotes FilterQuery.
type FilterCondition struct {
	Type     string `json:"type"`
	ID       string `json:"id"`
	Property string `json:"property"`
	Operator string `json:"operator"`
	Value    string `json:"value,omitempty"`
}

// FilterGroup is the top-level (or nested) group of a TaskNotes FilterQuery.
type FilterGroup struct {
	Type          string            `json:"type"`
	ID            string            `json:"id"`
	Conjunction   string            `json:"conjunction"`
	Children      []FilterCondition `json:"children"`
	SortKey       string            `json:"sortKey,omitempty"`
	SortDirection string            `json:"sortDirection,omitempty"`
	GroupKey      string            `json:"groupKey,omitempty"`
}

// Config holds the resolved connection settings for the TaskNotes API.
type Config struct {
	URL   string
	Token string
}

type fileConfig struct {
	URL   string `json:"url"`
	Token string `json:"token"`
}

// resolveConfig applies the documented precedence:
//  1. env TASKNOTES_URL / TASKNOTES_TOKEN
//  2. ~/.config/tn/config.json {"url":"...","token":"..."} (ignored if missing)
//  3. default url http://localhost:8390, empty token
func resolveConfig() Config {
	cfg := Config{URL: "http://localhost:8390", Token: ""}

	if home, err := os.UserHomeDir(); err == nil {
		fc := readFileConfig(filepath.Join(home, ".config", "tn", "config.json"))
		if fc != nil {
			if fc.URL != "" {
				cfg.URL = fc.URL
			}
			if fc.Token != "" {
				cfg.Token = fc.Token
			}
		}
	}

	if v := os.Getenv("TASKNOTES_URL"); v != "" {
		cfg.URL = v
	}
	if v := os.Getenv("TASKNOTES_TOKEN"); v != "" {
		cfg.Token = v
	}

	cfg.URL = strings.TrimRight(cfg.URL, "/")
	return cfg
}

// readFileConfig reads the config file, returning nil (not an error) if it
// does not exist.
func readFileConfig(path string) *fileConfig {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var fc fileConfig
	if err := json.Unmarshal(b, &fc); err != nil {
		return nil
	}
	return &fc
}

// Client is a minimal HTTP client for the TaskNotes API.
type Client struct {
	BaseURL string
	Token   string
	HTTP    *http.Client
}

// NewClient builds a Client with the standard 10s timeout.
func NewClient(cfg Config) *Client {
	return &Client{
		BaseURL: cfg.URL,
		Token:   cfg.Token,
		HTTP:    &http.Client{Timeout: 10 * time.Second},
	}
}

// httpError represents a non-2xx HTTP response from the TaskNotes API.
type httpError struct {
	Code int
	Body string
}

func (e *httpError) Error() string {
	return fmt.Sprintf("HTTP %d: %s", e.Code, strings.TrimSpace(e.Body))
}

// connRefusedError represents a failure to reach the TaskNotes API at all.
type connRefusedError struct {
	URL string
}

func (e *connRefusedError) Error() string {
	return fmt.Sprintf("TaskNotes API not reachable at %s — enable HTTP API in TaskNotes settings", e.URL)
}

func isConnRefused(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, syscall.ECONNREFUSED) {
		return true
	}
	return strings.Contains(err.Error(), "connection refused")
}

// request performs an HTTP call against path (which must start with "/api")
// and returns the raw response body. body, if non-nil, is JSON-encoded as
// the request body. A non-2xx response yields *httpError; a connection
// failure yields *connRefusedError.
func (c *Client) request(method, path string, body any) ([]byte, error) {
	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		reader = bytes.NewReader(b)
	}

	req, err := http.NewRequest(method, c.BaseURL+path, reader)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}

	resp, err := c.HTTP.Do(req)
	if err != nil {
		if isConnRefused(err) {
			return nil, &connRefusedError{URL: c.BaseURL}
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

// taskPath returns the API path for a given vault-relative task path,
// escaping it so that slashes and spaces are correctly percent-encoded.
func taskPath(id string) string {
	return "/api/tasks/" + url.PathEscape(id)
}

// envelope models the {"data": ...} wrapper common to all responses.
type envelope struct {
	Data json.RawMessage `json:"data"`
}

// extractTask pulls a Task out of a data payload that may be either the
// task itself, or an object with a "task" key holding the task.
func extractTask(data json.RawMessage) (Task, error) {
	var task Task
	if len(data) == 0 {
		return task, nil
	}

	var m map[string]json.RawMessage
	if err := json.Unmarshal(data, &m); err == nil {
		if t, ok := m["task"]; ok {
			err := json.Unmarshal(t, &task)
			return task, err
		}
	}

	err := json.Unmarshal(data, &task)
	return task, err
}

// Health calls GET /api/health and returns the raw response body.
func (c *Client) Health() ([]byte, error) {
	return c.request(http.MethodGet, "/api/health", nil)
}

// Stats calls GET /api/stats and returns the raw response body.
func (c *Client) Stats() ([]byte, error) {
	return c.request(http.MethodGet, "/api/stats", nil)
}

// GetTask calls GET /api/tasks/:id and returns the task.
func (c *Client) GetTask(id string) (Task, error) {
	body, err := c.request(http.MethodGet, taskPath(id), nil)
	if err != nil {
		return Task{}, err
	}
	var env envelope
	if err := json.Unmarshal(body, &env); err != nil {
		return Task{}, err
	}
	return extractTask(env.Data)
}

// CreateTask calls POST /api/tasks and returns the created task.
func (c *Client) CreateTask(fields map[string]any) (Task, error) {
	body, err := c.request(http.MethodPost, "/api/tasks", fields)
	if err != nil {
		return Task{}, err
	}
	var env envelope
	if err := json.Unmarshal(body, &env); err != nil {
		return Task{}, err
	}
	return extractTask(env.Data)
}

// UpdateTask calls PUT /api/tasks/:id with a partial update.
func (c *Client) UpdateTask(id string, fields map[string]any) (Task, error) {
	body, err := c.request(http.MethodPut, taskPath(id), fields)
	if err != nil {
		return Task{}, err
	}
	var env envelope
	if err := json.Unmarshal(body, &env); err != nil {
		return Task{}, err
	}
	return extractTask(env.Data)
}

// queryData models the {"data": {"tasks": [...], "total": N, "filtered": N}}
// shape returned by POST /api/tasks/query.
type queryData struct {
	Tasks    []Task `json:"tasks"`
	Total    int    `json:"total"`
	Filtered int    `json:"filtered"`
}

// QueryTasks calls POST /api/tasks/query with the given filter and returns
// the matching tasks.
func (c *Client) QueryTasks(fg FilterGroup) ([]Task, error) {
	return c.QueryTasksRaw(fg)
}

// QueryTasksRaw calls POST /api/tasks/query with an arbitrary pre-built
// query body and returns the matching tasks. Use this instead of QueryTasks
// when the query needs a shape FilterGroup/FilterCondition can't express —
// e.g. a nested OR group (see filterNode) — since FilterQuery has no "in"
// operator for matching one of several values.
func (c *Client) QueryTasksRaw(query any) ([]Task, error) {
	body, err := c.request(http.MethodPost, "/api/tasks/query", query)
	if err != nil {
		return nil, err
	}
	var env struct {
		Data queryData `json:"data"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return nil, err
	}
	return env.Data.Tasks, nil
}

// filterNode is a generic TaskNotes FilterQuery node — either a leaf
// condition or a (possibly nested) group — for queries whose structure
// FilterGroup/FilterCondition can't represent (their Children field only
// holds leaf conditions, not nested groups).
type filterNode struct {
	Type          string       `json:"type"`
	ID            string       `json:"id"`
	Property      string       `json:"property,omitempty"`
	Operator      string       `json:"operator,omitempty"`
	Value         string       `json:"value,omitempty"`
	Conjunction   string       `json:"conjunction,omitempty"`
	Children      []filterNode `json:"children,omitempty"`
	SortKey       string       `json:"sortKey,omitempty"`
	SortDirection string       `json:"sortDirection,omitempty"`
	GroupKey      string       `json:"groupKey,omitempty"`
}

// buildListQuery constructs the FilterQuery body for `tn list`.
func buildListQuery(status, project, tag, context string, all, startable bool) FilterGroup {
	children := []FilterCondition{}
	if !all {
		children = append(children,
			FilterCondition{Type: "condition", ID: "archived", Property: "archived", Operator: "is-not-checked"},
			FilterCondition{Type: "condition", ID: "completed", Property: "status.isCompleted", Operator: "is-not-checked"},
		)
	}
	if startable {
		// dependencies.isBlocked is-not-checked: verified live against the
		// real TaskNotes API — correctly excludes a task with an
		// incomplete blockedBy entry. (A plain top-level "isBlocked"
		// property silently matches nothing; don't use it.)
		children = append(children,
			FilterCondition{Type: "condition", ID: "not-blocked", Property: "dependencies.isBlocked", Operator: "is-not-checked"},
		)
	}
	n := 0
	add := func(property, operator, value string) {
		n++
		children = append(children, FilterCondition{
			Type: "condition", ID: fmt.Sprintf("c%d", n),
			Property: property, Operator: operator, Value: value,
		})
	}
	if status != "" {
		add("status", "is", status)
	}
	if project != "" {
		add("projects", "contains", project)
	}
	if tag != "" {
		add("tags", "contains", tag)
	}
	if context != "" {
		add("contexts", "contains", context)
	}

	return FilterGroup{
		Type:          "group",
		ID:            "root",
		Conjunction:   "and",
		Children:      children,
		SortKey:       "due",
		SortDirection: "asc",
		GroupKey:      "none",
	}
}

// resolveNoteBy returns the "by" attribution for `tn note`.
func resolveNoteBy(flagBy string) string {
	if flagBy != "" {
		return flagBy
	}
	if v := os.Getenv("TASKNOTES_AGENT"); v != "" {
		return v
	}
	return "claude"
}

// AddNote performs the GET-then-PUT round trip for `tn note`: prepends a
// new history entry (see SPEC-note-layout.md / note_layout.go) rather than
// the old flat append — migrating a legacy (anchor-less) body safely on
// first write via applyNoteBodyEdit.
func (c *Client) AddNote(path, text, by string) (Task, error) {
	task, err := c.GetTask(path)
	if err != nil {
		return Task{}, err
	}
	entry := formatHistoryEntry(resolveNoteBy(by), text, time.Now())
	newDetails := applyNoteBodyEdit(task.Details, func(nb *noteBody) {
		nb.History = append([]string{entry}, nb.History...)
	})
	return c.UpdateTask(path, map[string]any{"details": newDetails})
}

// SetBrief performs the GET-then-PUT round trip for `tn brief`: REPLACES
// the brief block (creating it if absent), migrating a legacy body safely
// on first write.
func (c *Client) SetBrief(path, text string) (Task, error) {
	task, err := c.GetTask(path)
	if err != nil {
		return Task{}, err
	}
	newDetails := applyNoteBodyEdit(task.Details, func(nb *noteBody) { nb.Brief = text })
	return c.UpdateTask(path, map[string]any{"details": newDetails})
}

// SetAsk performs the GET-then-PUT round trip for `tn ask`: replaces the
// ask block AND sets status needs-input, in one PUT. Also writes the reply
// block with its default template if one isn't already present — per
// SPEC-note-layout.md's addendum, a question always ships with an obvious
// place to answer — but never overwrites an EXISTING reply block, since
// the user may already be mid-reply there.
func (c *Client) SetAsk(path, text string) (Task, error) {
	task, err := c.GetTask(path)
	if err != nil {
		return Task{}, err
	}
	newDetails := applyNoteBodyEdit(task.Details, func(nb *noteBody) {
		nb.Ask = text
		if nb.Reply == "" {
			nb.Reply = defaultReplyBody
		}
	})
	return c.UpdateTask(path, map[string]any{"details": newDetails, "status": "needs-input"})
}

// ClearAsk performs the GET-then-PUT round trip for `tn ask --clear`:
// removes ONLY the ask block; the reply block is left in place (the user
// may reply unprompted on any task, per SPEC-note-layout.md's addendum),
// and status is left untouched (clearing the question doesn't by itself
// imply any particular status).
func (c *Client) ClearAsk(path string) (Task, error) {
	task, err := c.GetTask(path)
	if err != nil {
		return Task{}, err
	}
	newDetails := applyNoteBodyEdit(task.Details, func(nb *noteBody) { nb.Ask = "" })
	return c.UpdateTask(path, map[string]any{"details": newDetails})
}

// AddLink performs the GET-then-PUT round trip for `tn link`: appends url
// (with an optional label) to the links block, de-duplicated by URL — see
// upsertLink.
func (c *Client) AddLink(path, url, label string) (Task, error) {
	task, err := c.GetTask(path)
	if err != nil {
		return Task{}, err
	}
	newDetails := applyNoteBodyEdit(task.Details, func(nb *noteBody) {
		nb.Links = upsertLink(nb.Links, label, url)
	})
	return c.UpdateTask(path, map[string]any{"details": newDetails})
}

// ListWebhooks calls GET /api/webhooks and returns the raw response body.
func (c *Client) ListWebhooks() ([]byte, error) {
	return c.request(http.MethodGet, "/api/webhooks", nil)
}

// CreateWebhook calls POST /api/webhooks to register a webhook subscription.
func (c *Client) CreateWebhook(hookURL string, events []string) error {
	_, err := c.request(http.MethodPost, "/api/webhooks", map[string]any{
		"url":    hookURL,
		"events": events,
	})
	return err
}

// DeleteWebhook calls DELETE /api/webhooks/:id to remove a webhook
// subscription — used to replace a stale registration (wrong/missing
// events) rather than leaving a duplicate: TaskNotes has no update
// endpoint, only create/list/delete.
func (c *Client) DeleteWebhook(id string) error {
	_, err := c.request(http.MethodDelete, "/api/webhooks/"+id, nil)
	return err
}

// webhookEntry is the subset of a TaskNotes webhook subscription we care
// about: enough to tell whether ours is registered AND subscribed to the
// right events (a URL match alone isn't sufficient — see tryRegisterWebhook).
type webhookEntry struct {
	ID     string   `json:"id"`
	URL    string   `json:"url"`
	Events []string `json:"events"`
}

// extractWebhookEntries pulls the subscribed webhook entries out of a GET
// /api/webhooks data payload, tolerating either a bare array or an
// {"webhooks": [...]} wrapper.
func extractWebhookEntries(data json.RawMessage) []webhookEntry {
	var arr []webhookEntry
	if err := json.Unmarshal(data, &arr); err == nil {
		return arr
	}
	var wrapped struct {
		Webhooks []webhookEntry `json:"webhooks"`
	}
	if err := json.Unmarshal(data, &wrapped); err == nil {
		return wrapped.Webhooks
	}
	return nil
}

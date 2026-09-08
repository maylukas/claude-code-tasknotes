// Command tn is a minimal CLI wrapper for the Obsidian TaskNotes HTTP API.
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"
)

const usage = `tn - CLI for the Obsidian TaskNotes HTTP API

Usage:
  tn health
  tn list [--status v] [--project v] [--tag v] [--context v] [--all] [--startable] [--json]
  tn get <path> [--json] [--history N]                 N most-recent entries (default 5, 0=all)
  tn create --title T [--details D] [--status S] [--priority P] [--due YYYY-MM-DD]
            [--scheduled YYYY-MM-DD] [--project P]... [--tag T]... [--context C]...
            [--claude-project SLUG] [--mr URL] [--integration-branch B]
            [--integration-role owner] [--jira K[,K2]] [--jira-nc K]
            [--from PARENT-PATH] [--blocked-by PATH]...
  tn update <path> [--title T] [--details D] [--status S] [--priority P]
            [--due YYYY-MM-DD] [--scheduled YYYY-MM-DD]
            [--project P]... [--tag T]... [--context C]... [--claude-project SLUG]
            [--mr URL] [--integration-branch B] [--integration-role owner]
            [--jira K[,K2]] [--jira-nc K] [--blocked-by PATH]...
            [--replace-history]     --details replaces ONLY the description by default,
                                    preserving history/ask/reply/links/brief; this flag
                                    is REQUIRED to let --details replace those too
  tn status <path> <value>
  tn done <path>
  tn note <path> <text> [--by NAME]                    prepend a history entry
  tn brief <path> <text>                               replace the "Current" block
  tn ask <path> <text>                                 set "Needs you" + status needs-input
  tn ask --clear <path>                                 remove the "Needs you" block
  tn link <path> <url> [--label L]                     add/update a links-block entry
  tn stats
  tn help

  tn serve [--port N]                                  run the bridge daemon
  tn register --name N --project P [--session ID]
             [--tmux-session S]                        register/revive an agent session
  tn drain --name N                                    stop accepting new task assignments
  tn retire --name N                                    final command: retire & kill your session
  tn pause                                              stop the daemon from spawning new orchestrators
  tn resume                                             undo tn pause
  tn inbox --name N [--wait S] [--json]                long-poll for queued messages
                                                          (long text is summarized; see tn msg)
  tn msg [--json] ID                                    print one message's full text/response
  tn ack --name N --msg ID [--response "text"]          acknowledge a message
  tn worker start|end --name N --task P                declare/retract a worker subtask (for /status)
  tn send [--to AGENT | --project P] [--task PATH]
          [--from NAME] [--wait S] TEXT                  send a message via the bridge
  tn agents [--json]                                    list registered agents
  tn log --name N "text"                                append to N's session activity log
  tn env [--project P] [--json]                         list configured per-project env entries
  tn repo <slug> [--json]                               resolved repo-note settings for a project
  tn creds save|list|use|rm [label]                    claude.ai credential profiles (Keychain-backed):
                                                          save = capture the current /login as <label>,
                                                          use = swap ALL Claude sessions to <label>
  tn worktrees [--project P] [--apply] [--json]         report (and with --apply remove) finished agent
                                                          worktrees under <cwd>/.claude/worktrees via the daemon

Configuration (first match wins):
  1. env TASKNOTES_URL / TASKNOTES_TOKEN
  2. ~/.config/tn/config.json  {"url": "...", "token": "..."}
  3. default http://localhost:8390, no token

Bridge daemon configuration:
  1. env TN_BRIDGE_URL (client commands) / TN_BRIDGE_PORT (serve default port)
  2. ~/.config/tn/serve.json  {"port": 8391, "projects": {"<slug>": {"autoSpawn": true, "cwd": "/path"}}}
  3. default http://localhost:8391, port 8391
`

// stringSliceFlag implements flag.Value to collect repeatable flags.
type stringSliceFlag []string

func (s *stringSliceFlag) String() string {
	if s == nil {
		return ""
	}
	return strings.Join(*s, ",")
}

func (s *stringSliceFlag) Set(v string) error {
	*s = append(*s, v)
	return nil
}

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	if len(args) == 0 || args[0] == "help" || args[0] == "-h" || args[0] == "--help" {
		fmt.Print(usage)
		return 0
	}

	cfg := resolveConfig()
	client := NewClient(cfg)

	cmd, rest := args[0], args[1:]
	var err error
	switch cmd {
	case "health":
		err = cmdHealth(client, rest)
	case "list":
		err = cmdList(client, rest)
	case "get":
		err = cmdGet(client, rest)
	case "create":
		err = cmdCreate(client, rest)
	case "update":
		err = cmdUpdate(client, rest)
	case "status":
		err = cmdStatus(client, rest)
	case "done":
		err = cmdDone(client, rest)
	case "note":
		err = cmdNote(client, rest)
	case "brief":
		err = cmdBrief(client, rest)
	case "ask":
		err = cmdAsk(client, rest)
	case "link":
		err = cmdLink(client, rest)
	case "stats":
		err = cmdStats(client, rest)
	case "serve":
		err = cmdServe(rest)
	case "register":
		err = cmdRegister(rest)
	case "drain":
		err = cmdDrain(rest)
	case "retire":
		err = cmdRetire(rest)
	case "pause":
		err = cmdPause(rest)
	case "resume":
		err = cmdResume(rest)
	case "inbox":
		err = cmdInbox(rest)
	case "msg":
		err = cmdMsg(rest)
	case "ack":
		err = cmdAck(rest)
	case "worker":
		err = cmdWorker(rest)
	case "send":
		err = cmdSend(rest)
	case "agents":
		err = cmdAgents(rest)
	case "log":
		err = cmdLog(rest)
	case "env":
		err = cmdEnv(rest)
	case "repo":
		err = cmdRepo(rest)
	case "creds":
		err = cmdCreds(rest)
	case "worktrees":
		err = cmdWorktrees(rest)
	default:
		fmt.Fprintf(os.Stderr, "tn: unknown command %q\n\n", cmd)
		fmt.Fprint(os.Stderr, usage)
		return 1
	}

	if err != nil {
		return reportErr(err)
	}
	return 0
}

func reportErr(err error) int {
	var ece *exitCodeError
	if errors.As(err, &ece) {
		fmt.Fprintf(os.Stderr, "tn: %s\n", ece.msg)
		return ece.code
	}
	var he *httpError
	if errors.As(err, &he) {
		fmt.Fprintf(os.Stderr, "tn: HTTP %d: %s\n", he.Code, he.Body)
		return 1
	}
	var ce *connRefusedError
	if errors.As(err, &ce) {
		fmt.Fprintln(os.Stderr, ce.Error())
		return 1
	}
	fmt.Fprintf(os.Stderr, "tn: %v\n", err)
	return 1
}

func printJSON(v any) {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		fmt.Fprintf(os.Stderr, "tn: %v\n", err)
		return
	}
	fmt.Println(string(b))
}

func cmdHealth(c *Client, args []string) error {
	body, err := c.Health()
	if err != nil {
		return err
	}
	printRawJSON(body)
	return nil
}

func cmdStats(c *Client, args []string) error {
	body, err := c.Stats()
	if err != nil {
		return err
	}
	printRawJSON(body)
	return nil
}

func printRawJSON(body []byte) {
	var v any
	if err := json.Unmarshal(body, &v); err != nil {
		fmt.Println(string(body))
		return
	}
	printJSON(v)
}

func cmdList(c *Client, args []string) error {
	fs := flag.NewFlagSet("list", flag.ExitOnError)
	status := fs.String("status", "", "filter by status")
	project := fs.String("project", "", "filter by project")
	tag := fs.String("tag", "", "filter by tag")
	context := fs.String("context", "", "filter by context")
	all := fs.Bool("all", false, "include archived/completed tasks")
	startable := fs.Bool("startable", false, "exclude tasks blocked by an incomplete dependency")
	asJSON := fs.Bool("json", false, "print raw JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}

	fg := buildListQuery(*status, *project, *tag, *context, *all, *startable)
	tasks, err := c.QueryTasks(fg)
	if err != nil {
		return err
	}

	if *asJSON {
		printJSON(tasks)
		return nil
	}
	for _, t := range tasks {
		priority := valueOrDash(t.Priority)
		due := valueOrDash(t.Due)
		fmt.Printf("%s\t%s\t%s\t%s\t%s\n", t.Path, t.Status, priority, due, t.Title)
	}
	return nil
}

func valueOrDash(v string) string {
	if v == "" {
		return "-"
	}
	return v
}

func cmdGet(c *Client, args []string) error {
	fs := flag.NewFlagSet("get", flag.ExitOnError)
	asJSON := fs.Bool("json", false, "print raw JSON")
	historyN := fs.Int("history", 5, "number of most-recent history entries to print (0 = all)")

	// path is the leading positional argument; flags may appear after it
	// (as the usage text documents) — same pattern as cmdUpdate, since
	// flag.Parse stops at the first non-flag token and would otherwise
	// silently ignore every flag placed after <path>.
	if len(args) < 1 {
		return fmt.Errorf("usage: tn get <path> [--json] [--history N]")
	}
	path := args[0]
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}

	task, err := c.GetTask(path)
	if err != nil {
		return err
	}

	if *asJSON {
		printJSON(task)
		return nil
	}
	fmt.Printf("title: %s\n", task.Title)
	fmt.Printf("status: %s\n", task.Status)
	fmt.Printf("priority: %s\n", task.Priority)
	fmt.Printf("due: %s\n", task.Due)
	fmt.Printf("scheduled: %s\n", task.Scheduled)
	fmt.Printf("tags: %s\n", strings.Join(task.Tags, ", "))
	fmt.Printf("projects: %s\n", strings.Join(task.Projects, ", "))
	fmt.Printf("contexts: %s\n", strings.Join(task.Contexts, ", "))
	if mr := task.CustomProperties["mr"]; mr != "" {
		fmt.Printf("mr: %s\n", mr)
	}
	if branch := integrationBranchFor(task); branch != "" {
		fmt.Printf("integration-branch: %s\n", branch)
	}
	if role := integrationRoleFor(task); role != "" {
		fmt.Printf("integration-role: %s\n", role)
	}
	if jira := task.CustomProperties["jira"]; jira != "" {
		fmt.Printf("jira: %s\n", jira)
	}
	if jiraNC := task.CustomProperties["jira-nc"]; jiraNC != "" {
		fmt.Printf("jira-nc: %s\n", jiraNC)
	}
	fmt.Printf("path: %s\n", task.Path)

	// Ask/reply/links/brief (if any) print above the details, in the same
	// order they're stored, so agents see current state without re-reading
	// (folded, forensic-only) history — see SPEC-note-layout.md. A legacy
	// (unstructured) body has none of these; its Details print unchanged,
	// exactly as before this feature.
	nb, structured := parseNoteBody(task.Details)
	if structured {
		if nb.Ask != "" {
			fmt.Printf("ask: %s\n", nb.Ask)
		}
		if nb.Reply != "" {
			fmt.Printf("reply: %s\n", nb.Reply)
		}
		if nb.Links != "" {
			fmt.Printf("links:\n%s\n", nb.Links)
		}
		if nb.Brief != "" {
			fmt.Printf("brief: %s\n", nb.Brief)
		}
	}
	fmt.Println()
	if structured {
		fmt.Println(nb.Description)
	} else {
		fmt.Println(task.Details)
	}

	// History is printed by default (not just on --json): an agent that
	// appends a note and reads the task back to confirm needs to actually
	// see it, or a `tn note` write that succeeded looks indistinguishable
	// from one that silently vanished — this is what caused a real
	// incident (SPEC-note-layout.md follow-up). Newest-first, matching the
	// stored order; truncated to --history entries (default 5, 0 = all).
	if structured && len(nb.History) > 0 {
		shown := nb.History
		n := *historyN
		if n > 0 && n < len(shown) {
			shown = shown[:n]
		}
		fmt.Println()
		if hidden := len(nb.History) - len(shown); hidden > 0 {
			fmt.Printf("history (%d of %d):\n\n", len(shown), len(nb.History))
			fmt.Println(strings.Join(shown, "\n\n"))
			fmt.Printf("\n… (%d more, see the note in Obsidian)\n", hidden)
		} else {
			fmt.Printf("history (%d):\n\n", len(shown))
			fmt.Println(strings.Join(shown, "\n\n"))
		}
	}
	return nil
}

// buildBlockedByField turns repeatable --blocked-by values into the
// blockedBy body field: one {"uid": <value verbatim>, "reltype":
// "FINISHTOSTART"} entry per value. Values are passed through exactly as
// given (a plain path or an explicit "[[wikilink]]") — TaskNotes normalizes
// the stored uid itself.
func buildBlockedByField(uids []string) []map[string]any {
	deps := make([]map[string]any, 0, len(uids))
	for _, uid := range uids {
		deps = append(deps, map[string]any{"uid": uid, "reltype": "FINISHTOSTART"})
	}
	return deps
}

func cmdCreate(c *Client, args []string) error {
	fs := flag.NewFlagSet("create", flag.ExitOnError)
	title := fs.String("title", "", "task title (required)")
	details := fs.String("details", "", "task details")
	status := fs.String("status", "", "task status")
	priority := fs.String("priority", "", "task priority")
	due := fs.String("due", "", "due date YYYY-MM-DD")
	scheduled := fs.String("scheduled", "", "scheduled date YYYY-MM-DD")
	claudeProject := fs.String("claude-project", "", "bridge routing slug (sets customProperties.claude-project)")
	mr := fs.String("mr", "", "merge/pull request URL (sets customProperties.mr)")
	integrationBranch := fs.String("integration-branch", "", "target branch for this workstream (sets customProperties.integration-branch)")
	integrationRole := fs.String("integration-role", "", `workstream role: "owner" holds the consolidated MR (sets customProperties.integration-role) — never inherited via --from`)
	jira := fs.String("jira", "", "comma-separated Jira issue key(s) (sets customProperties.jira; normal transition policy)")
	jiraNC := fs.String("jira-nc", "", "Jira nonconformity issue key (sets customProperties.jira-nc; regulated record — comment-only, never auto-transitioned)")
	from := fs.String("from", "", "parent task path — inherits integration-branch/claude-project from it (explicit flags on this command still win)")
	var projects, tags, contexts, blockedBy stringSliceFlag
	fs.Var(&projects, "project", "project (repeatable)")
	fs.Var(&tags, "tag", "tag (repeatable)")
	fs.Var(&contexts, "context", "context (repeatable)")
	fs.Var(&blockedBy, "blocked-by", "path or [[wikilink]] of a task this depends on (repeatable)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *title == "" {
		return fmt.Errorf("--title is required")
	}

	// --from inherits claude-project/integration-branch from the PARENT
	// task's own values — never a repo-note or other fallback (see
	// integrationBranchFor) — so a child task of a workstream keeps
	// targeting the same integration branch after the orchestrator that
	// knew about it has retired. Explicit flags on this same command line
	// still win; a missing/unreadable parent fails loudly with no task
	// created rather than silently creating an un-inherited task.
	//
	// integration-role is DELIBERATELY NOT inherited: a child task must
	// never inherit ownership of its parent's consolidated MR — only an
	// explicit --integration-role owner on THIS command sets it.
	effectiveClaudeProject := *claudeProject
	effectiveIntegrationBranch := *integrationBranch
	if *from != "" {
		parent, err := c.GetTask(*from)
		if err != nil {
			return fmt.Errorf("--from %q: failed to fetch parent task: %w", *from, err)
		}
		if effectiveClaudeProject == "" {
			effectiveClaudeProject = parent.CustomProperties["claude-project"]
		}
		if effectiveIntegrationBranch == "" {
			effectiveIntegrationBranch = parent.CustomProperties["integration-branch"]
		}
	}

	fields := map[string]any{"title": *title}
	// --mr additionally seeds the links block with an "MR" entry as the
	// first link (SPEC-note-layout.md addendum), so opening the task shows
	// it without hunting through customProperties — applied to whatever
	// --details text was given (or "" for a brand-new plain task).
	if *mr != "" {
		fields["details"] = applyNoteBodyEdit(*details, func(nb *noteBody) {
			nb.Links = setMRLink(nb.Links, *mr)
		})
	} else if *details != "" {
		fields["details"] = *details
	}
	if *status != "" {
		fields["status"] = *status
	}
	if *priority != "" {
		fields["priority"] = *priority
	}
	if *due != "" {
		fields["due"] = *due
	}
	if *scheduled != "" {
		fields["scheduled"] = *scheduled
	}
	if len(tags) > 0 {
		fields["tags"] = []string(tags)
	}
	if len(contexts) > 0 {
		fields["contexts"] = []string(contexts)
	}
	if len(projects) > 0 {
		fields["projects"] = []string(projects)
	}
	customProps := map[string]any{}
	if effectiveClaudeProject != "" {
		customProps["claude-project"] = effectiveClaudeProject
	}
	if *mr != "" {
		customProps["mr"] = *mr
	}
	if effectiveIntegrationBranch != "" {
		customProps["integration-branch"] = effectiveIntegrationBranch
	}
	if *integrationRole != "" {
		customProps["integration-role"] = *integrationRole
	}
	if *jira != "" {
		customProps["jira"] = *jira
	}
	if *jiraNC != "" {
		customProps["jira-nc"] = *jiraNC
	}
	if len(customProps) > 0 {
		fields["customProperties"] = customProps
	}
	if len(blockedBy) > 0 {
		fields["blockedBy"] = buildBlockedByField(blockedBy)
	}

	// Idempotency key: a retry of this exact command (same title/project/
	// details) must derive the SAME key every time, so a client-side
	// timeout on the first attempt — TaskNotes' own create handler can be
	// slow enough to trip it even though the create actually lands — can
	// be recognized as a repeat by the daemon rather than creating a
	// second task. Routed through the bridge specifically because the
	// daemon is what holds the dedup map; a direct TaskNotes call (the old
	// path, still used as a fallback below) has no way to dedup at all.
	idempotencyKey := sha256Hex(*title + "|" + effectiveClaudeProject + "|" + *details)

	task, err := createTaskIdempotent(idempotencyKey, fields, c)
	if err != nil {
		return err
	}
	fmt.Println(task.Path)
	return nil
}

// createTaskIdempotent routes task creation through the bridge daemon
// (POST /tasks/create) so a `tn create` retry after a client-side timeout
// is deduped rather than creating a second task — see handleCreateTask.
// Falls back to calling TaskNotes directly (the old behavior, no dedup)
// ONLY when the bridge is unreachable (connection refused — "tn serve
// isn't running"), never on any other bridge-side error: an ambiguous
// failure (the daemon's OWN call to TaskNotes timing out, say) must not
// trigger a second create attempt, which would risk the exact duplicate
// this exists to prevent.
func createTaskIdempotent(idempotencyKey string, fields map[string]any, c *Client) (Task, error) {
	b := NewBridgeClient(resolveBridgeURL())
	body, err := b.CreateTaskViaBridge(idempotencyKey, fields)
	if err == nil {
		var resp struct {
			Task Task `json:"task"`
		}
		if jsonErr := json.Unmarshal(body, &resp); jsonErr != nil {
			return Task{}, jsonErr
		}
		return resp.Task, nil
	}

	var unreachable *bridgeUnreachableError
	if !errors.As(err, &unreachable) {
		return Task{}, fmt.Errorf("create via bridge failed: %w", err)
	}
	// Bridge genuinely not running — fall back to the old direct path.
	// No idempotency in this case, same as it always was.
	return c.CreateTask(fields)
}

func cmdUpdate(c *Client, args []string) error {
	fs := flag.NewFlagSet("update", flag.ExitOnError)
	title := fs.String("title", "", "task title")
	details := fs.String("details", "", "task details")
	status := fs.String("status", "", "task status")
	priority := fs.String("priority", "", "task priority")
	due := fs.String("due", "", "due date YYYY-MM-DD")
	scheduled := fs.String("scheduled", "", "scheduled date YYYY-MM-DD")
	claudeProject := fs.String("claude-project", "", "bridge routing slug (sets customProperties.claude-project)")
	mr := fs.String("mr", "", "merge/pull request URL (sets customProperties.mr)")
	integrationBranch := fs.String("integration-branch", "", "target branch for this workstream (sets customProperties.integration-branch)")
	integrationRole := fs.String("integration-role", "", `workstream role: "owner" holds the consolidated MR (sets customProperties.integration-role)`)
	jira := fs.String("jira", "", "comma-separated Jira issue key(s) (sets customProperties.jira; normal transition policy)")
	jiraNC := fs.String("jira-nc", "", "Jira nonconformity issue key (sets customProperties.jira-nc; regulated record — comment-only, never auto-transitioned)")
	replaceHistory := fs.Bool("replace-history", false, "REQUIRED to let --details replace the managed history/ask/brief/links blocks too, not just the description — see cmdUpdate's own comment")
	var projects, tags, contexts, blockedBy stringSliceFlag
	fs.Var(&projects, "project", "project (repeatable, replaces array)")
	fs.Var(&tags, "tag", "tag (repeatable, replaces array)")
	fs.Var(&contexts, "context", "context (repeatable, replaces array)")
	fs.Var(&blockedBy, "blocked-by", "path or [[wikilink]] of a task this depends on (repeatable, replaces array)")

	// path is the leading positional argument; flags may appear after it.
	if len(args) < 1 {
		return fmt.Errorf("usage: tn update <path> [flags]")
	}
	path := args[0]
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}

	fields := map[string]any{}
	customProps := map[string]any{}
	detailsExplicit := false
	mrSet := false
	fs.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "title":
			fields["title"] = *title
		case "details":
			fields["details"] = *details
			detailsExplicit = true
		case "status":
			fields["status"] = *status
		case "priority":
			fields["priority"] = *priority
		case "due":
			fields["due"] = *due
		case "scheduled":
			fields["scheduled"] = *scheduled
		case "project":
			fields["projects"] = []string(projects)
		case "tag":
			fields["tags"] = []string(tags)
		case "context":
			fields["contexts"] = []string(contexts)
		case "claude-project":
			customProps["claude-project"] = *claudeProject
		case "mr":
			customProps["mr"] = *mr
			mrSet = true
		case "integration-branch":
			customProps["integration-branch"] = *integrationBranch
		case "integration-role":
			customProps["integration-role"] = *integrationRole
		case "jira":
			customProps["jira"] = *jira
		case "jira-nc":
			customProps["jira-nc"] = *jiraNC
		case "blocked-by":
			fields["blockedBy"] = buildBlockedByField(blockedBy)
		}
	})
	if len(customProps) > 0 {
		fields["customProperties"] = customProps
	}

	if len(fields) == 0 {
		return fmt.Errorf("no fields to update; pass at least one flag")
	}

	// --details used to send the flag's raw text as a LITERAL wholesale
	// replacement for the task's entire `details` field — UpdateTask's
	// partial-update semantics replace a field outright, not merge it —
	// which silently destroyed the managed history/ask/reply/links/brief
	// blocks on every call. LIVE INCIDENT (2026-08-26, reported by
	// g34005, reproduced by team-lead): a task holding 102 history
	// entries across 187,869 bytes was one --details write away from
	// losing all of it irrecoverably. Fixed by treating --details as
	// setting ONLY the free-form Description block (via
	// applyNoteBodyEdit, the same GET-then-PUT round trip tn note/tn
	// brief/tn ask already use) — every other managed block, not just
	// history, survives untouched by construction, since the edit
	// closure below never touches them.
	//
	// --replace-history is the required, explicit opt-out for a caller
	// that genuinely wants the old destructive behavior (SPEC-serve.md's
	// "anyone genuinely wanting to remove history can be made to say so
	// explicitly") — it prints exactly how many history entries the
	// write is about to remove, since silent irreversible data loss is
	// the entire failure mode being closed here and must never recur
	// even through an intentional escape hatch.
	if detailsExplicit {
		task, err := c.GetTask(path)
		if err != nil {
			return err
		}
		if *replaceHistory {
			if nb, ok := parseNoteBody(task.Details); ok && len(nb.History) > 0 {
				fmt.Printf("warning: this write removed %d history entries\n", len(nb.History))
			}
			fields["details"] = *details
		} else {
			// Belt-and-braces, not the primary fix: a body that's neither
			// already-structured (parseNoteBody) nor cleanly migratable
			// (migrateLegacyBody, SPEC-note-layout.md — documented to bail
			// out when the stripped-whitespace round trip can't be
			// verified lossless) is the one shape applyNoteBodyEdit's own
			// fallback would dump wholesale into Description and then
			// overwrite outright — the same bug for that narrower case.
			// migrateLegacyBody's own bail-out is documented as
			// STRUCTURALLY UNREACHABLE through its real code path (see
			// TestParseOrMigrateNoteBody_BailOutContract,
			// note_layout_test.go) — this branch is cheap insurance
			// against a future change reintroducing a real one, not a
			// path expected to fire today. Only refuses when there's
			// evidence of real content at risk (a history marker or a
			// legacy note-entry-shaped paragraph); a genuinely bare task
			// has nothing structured to protect, and replacing its plain
			// description is the ordinary, expected behavior.
			_, structuredOK := parseNoteBody(task.Details)
			_, migratedOK := migrateLegacyBody(task.Details)
			if !structuredOK && !migratedOK && looksLikeItHasHistoryContent(task.Details) {
				return fmt.Errorf("refusing --details on %s: existing content could not be safely parsed and may contain history that would be destroyed — pass --replace-history to override (it will tell you how many entries), or edit via tn note / tn get --json", path)
			}
			fields["details"] = applyNoteBodyEdit(task.Details, func(nb *noteBody) {
				nb.Description = *details
			})
		}
	}

	// --mr additionally seeds the links block with an "MR" entry as the
	// first link (SPEC-note-layout.md addendum), applied on top of
	// whatever fields["details"] already holds — the safe merge above
	// when --details was also given, or a fresh fetch below otherwise.
	if mrSet {
		base, _ := fields["details"].(string)
		if !detailsExplicit {
			task, err := c.GetTask(path)
			if err != nil {
				return err
			}
			base = task.Details
		}
		fields["details"] = applyNoteBodyEdit(base, func(nb *noteBody) {
			nb.Links = setMRLink(nb.Links, *mr)
		})
	}

	_, err := c.UpdateTask(path, fields)
	return err
}

// cmdLink implements `tn link <path> <url> [--label L]`: appends url to the
// links block (creating it if absent), de-duplicated by URL — a second
// call for the same URL updates its label in place instead of adding a
// duplicate line. --label may appear before or after the positionals, same
// as tn note's --by (see extractStringFlag).
func cmdLink(c *Client, args []string) error {
	label, rest := extractStringFlag(args, "label")
	if len(rest) < 2 {
		return fmt.Errorf("usage: tn link <path> <url> [--label L]")
	}
	path, url := rest[0], rest[1]
	if err := validatePositionalText(url); err != nil {
		return fmt.Errorf("usage: tn link <path> <url> [--label L]: %v", err)
	}

	_, err := c.AddLink(path, url, label)
	return err
}

func cmdStatus(c *Client, args []string) error {
	if len(args) < 2 {
		return fmt.Errorf("usage: tn status <path> <value>")
	}
	path, value := args[0], args[1]
	if value == "done" {
		return closeTaskChecked(c, path)
	}
	_, err := c.UpdateTask(path, map[string]any{"status": value})
	return err
}

func cmdDone(c *Client, args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: tn done <path>")
	}
	return closeTaskChecked(c, args[0])
}

// closeTaskChecked routes a transition to "done" through the bridge
// daemon's POST /tasks/close (mechanism 2) rather than writing status
// directly: a task carrying a DONE MEANS block gets its criteria checked
// BEFORE the write, so a rejection never touches TaskNotes at all — no
// flicker, nothing to revert.
//
// Falls back to the old direct-TaskNotes write ONLY when the bridge is
// genuinely unreachable (connection refused — "tn serve isn't running"),
// mirroring createTaskIdempotent's fallback rule. But unlike that fallback
// (which just loses dedup), silently skipping the DONE MEANS check here
// would be invisible — and this daemon restarts often, sometimes several
// times an hour. So the fallback path does NOT skip silently: it writes a
// visible marker into the task's own history saying the check did not
// run, so nothing reads as "checked and passed" when nobody actually
// checked (team-lead, 2026-08-25: "an unchecked close that looks identical
// to a checked one is the thing to avoid; an unchecked close that says so
// is fine").
//
// A 422 from the daemon means the criteria WERE checked and are unmet —
// that rejection is surfaced as-is, never retried as a fallback close
// (doing so would defeat the entire synchronous check).
func closeTaskChecked(c *Client, path string) error {
	b := NewBridgeClient(resolveBridgeURL())
	_, err := b.CloseTaskViaBridge(path)
	if err == nil {
		return nil
	}

	var unreachable *bridgeUnreachableError
	if errors.As(err, &unreachable) {
		note := fmt.Sprintf("closed without a DONE MEANS criteria check: bridge unreachable at %s", time.Now().Format(time.RFC3339))
		_, txErr := bridgeTransitionTask(c, path, "done", note)
		return txErr
	}

	var he *httpError
	if errors.As(err, &he) && he.Code == http.StatusUnprocessableEntity {
		return fmt.Errorf("close rejected — DONE MEANS criteria unmet: %s", strings.TrimSpace(he.Body))
	}

	return fmt.Errorf("close via bridge failed: %w", err)
}

func cmdNote(c *Client, args []string) error {
	// --by may appear either before or after the positional text (see
	// extractStringFlag) — Go's stdlib flag package only supports flags
	// before positionals, which is exactly what let `tn note <path> --by X`
	// (no text argument at all) silently store the literal "--by" as the
	// note body.
	by, rest := extractStringFlag(args, "by")
	if len(rest) < 2 {
		return fmt.Errorf("usage: tn note <path> <text> [--by NAME]")
	}
	path, text := rest[0], rest[1]
	if err := validatePositionalText(text); err != nil {
		return fmt.Errorf("usage: tn note <path> <text> [--by NAME]: %v", err)
	}

	_, err := c.AddNote(path, text, by)
	return err
}

// cmdBrief implements `tn brief <path> <text>` — REPLACES the brief
// block (see SPEC-note-layout.md), creating it if absent.
func cmdBrief(c *Client, args []string) error {
	if len(args) < 2 {
		return fmt.Errorf("usage: tn brief <path> <text>")
	}
	path, text := args[0], args[1]
	if err := validatePositionalText(text); err != nil {
		return fmt.Errorf("usage: tn brief <path> <text>: %v", err)
	}

	_, err := c.SetBrief(path, text)
	return err
}

// cmdAsk implements `tn ask <path> <text>` (writes the ask block and sets
// status needs-input) and `tn ask --clear <path>` (removes the ask block,
// used when a question has been answered) — see SPEC-note-layout.md.
func cmdAsk(c *Client, args []string) error {
	fs := flag.NewFlagSet("ask", flag.ExitOnError)
	clear := fs.Bool("clear", false, "remove the ask block instead of setting it")
	if err := fs.Parse(args); err != nil {
		return err
	}

	if *clear {
		if fs.NArg() < 1 {
			return fmt.Errorf("usage: tn ask --clear <path>")
		}
		_, err := c.ClearAsk(fs.Arg(0))
		return err
	}

	if fs.NArg() < 2 {
		return fmt.Errorf("usage: tn ask <path> <text>")
	}
	path, text := fs.Arg(0), fs.Arg(1)
	if err := validatePositionalText(text); err != nil {
		return fmt.Errorf("usage: tn ask <path> <text>: %v", err)
	}

	_, err := c.SetAsk(path, text)
	return err
}

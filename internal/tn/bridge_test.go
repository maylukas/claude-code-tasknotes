package tn

import (
	"strings"
	"testing"
)

// TestValidatePositionalText covers the shared guard directly: present,
// non-empty after trim, and not flag-like.
func TestValidatePositionalText(t *testing.T) {
	cases := []struct {
		name    string
		text    string
		wantErr bool
	}{
		{"empty", "", true},
		{"whitespace only", "   ", true},
		{"flag-like", "--by", true},
		{"flag-like with value-looking suffix", "--project=foo", true},
		{"normal text", "actual text", false},
		{"text that merely contains a dash", "use -v for verbose", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := validatePositionalText(c.text)
			if (err != nil) != c.wantErr {
				t.Errorf("validatePositionalText(%q) error = %v, wantErr %v", c.text, err, c.wantErr)
			}
		})
	}
}

// TestExtractStringFlag verifies the flag can appear anywhere in args (not
// just at the front), in both "--name value" and "--name=value" form, and
// that the remaining positional args keep their relative order.
func TestExtractStringFlag(t *testing.T) {
	cases := []struct {
		name     string
		args     []string
		wantVal  string
		wantRest []string
	}{
		{"absent", []string{"path", "text"}, "", []string{"path", "text"}},
		{"flag after", []string{"path", "text", "--by", "reviewer"}, "reviewer", []string{"path", "text"}},
		{"flag before", []string{"path", "--by", "reviewer", "text"}, "reviewer", []string{"path", "text"}},
		{"flag equals form", []string{"path", "--by=reviewer", "text"}, "reviewer", []string{"path", "text"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			val, rest := extractStringFlag(c.args, "by")
			if val != c.wantVal {
				t.Errorf("value = %q, want %q", val, c.wantVal)
			}
			if strings.Join(rest, ",") != strings.Join(c.wantRest, ",") {
				t.Errorf("rest = %v, want %v", rest, c.wantRest)
			}
		})
	}
}

// TestCmdSend_MissingTextErrors verifies `tn send --to AGENT` with no text
// argument at all fails before ever contacting the bridge (all flags here
// are recognized so flag.Parse succeeds; NArg()==0 is what fails it —
// deliberately not testing an actually-unrecognized flag, since this
// FlagSet uses flag.ExitOnError and would os.Exit the test process).
func TestCmdSend_MissingTextErrors(t *testing.T) {
	if err := cmdSend([]string{"--to", "agent-1"}); err == nil {
		t.Error("expected an error for a missing text argument, got nil")
	}
}

// TestCmdLog_MissingTextErrors mirrors the send case for `tn log`.
func TestCmdLog_MissingTextErrors(t *testing.T) {
	if err := cmdLog([]string{"--name", "agent-1"}); err == nil {
		t.Error("expected an error for a missing text argument, got nil")
	}
}

// TestCmdWorker_NoActionErrors verifies `tn worker` with no start/end
// subcommand fails before ever contacting the bridge.
func TestCmdWorker_NoActionErrors(t *testing.T) {
	if err := cmdWorker(nil); err == nil {
		t.Error("expected an error with no action given, got nil")
	}
}

// TestCmdWorker_UnknownActionErrors verifies an action other than
// start/end is rejected.
func TestCmdWorker_UnknownActionErrors(t *testing.T) {
	if err := cmdWorker([]string{"pause"}); err == nil {
		t.Error("expected an error for an unknown action, got nil")
	}
}

// TestCmdWorker_MissingFlagsErrors verifies both --name and --task are
// required for either action.
func TestCmdWorker_MissingFlagsErrors(t *testing.T) {
	if err := cmdWorker([]string{"start", "--name", "g1"}); err == nil {
		t.Error("expected an error for a missing --task, got nil")
	}
	if err := cmdWorker([]string{"end", "--task", "Tasks/A.md"}); err == nil {
		t.Error("expected an error for a missing --name, got nil")
	}
}

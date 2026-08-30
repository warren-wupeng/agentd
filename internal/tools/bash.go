package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/warren-wupeng/agentd/internal/policy"
	"github.com/warren-wupeng/agentd/internal/sandbox"
)

// Bash runs one shell command in the session sandbox. Exit codes and
// stderr are data — the model reads them and adapts.
type Bash struct{}

func NewBash() *Bash { return &Bash{} }

func (t *Bash) Name() string { return "bash" }

func (t *Bash) Description() string {
	return "Run a shell command in the session workspace and return stdout, stderr, and the exit code. " +
		"Working directory persists as the filesystem, but process state does not survive between calls — " +
		"chain dependent commands with && in one call when that matters."
}

func (t *Bash) Schema() json.RawMessage {
	return objSchema(map[string]any{
		"command":         prop("The shell command to run", "string"),
		"timeout_seconds": propInt("Optional timeout in seconds (default 120, max 600)"),
	}, "command")
}

func (t *Bash) PolicyDefault() policy.Verdict {
	return policy.Verdict{Decision: policy.Allow}
}

func (t *Bash) Execute(ctx context.Context, h sandbox.Handle, input json.RawMessage) (string, error) {
	var in struct {
		Command        string `json:"command"`
		TimeoutSeconds *int   `json:"timeout_seconds"`
	}
	if err := json.Unmarshal(input, &in); err != nil {
		return "", fmt.Errorf("parse input: %w", err)
	}
	if in.Command == "" {
		return "error: command is required", nil
	}
	timeout := 120 * time.Second
	if in.TimeoutSeconds != nil {
		if *in.TimeoutSeconds <= 0 || *in.TimeoutSeconds > 600 {
			return "error: timeout_seconds must be between 1 and 600", nil
		}
		timeout = time.Duration(*in.TimeoutSeconds) * time.Second
	}

	res, err := h.Exec(ctx, in.Command, timeout)
	if err != nil {
		return "", err // infrastructure: timeout kill, sandbox failure
	}
	out := ""
	if res.Stdout != "" {
		out += res.Stdout
	}
	if res.Stderr != "" {
		out += "\n[stderr]\n" + res.Stderr
	}
	if res.Truncated {
		out += "\n[output truncated]"
	}
	out += fmt.Sprintf("\n[exit code: %d]", res.ExitCode)
	return trimLeadingNewline(out), nil
}

func trimLeadingNewline(s string) string {
	if len(s) > 0 && s[0] == '\n' {
		return s[1:]
	}
	return s
}

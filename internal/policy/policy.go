// Package policy decides whether a tool call may run. Verdicts are
// recorded on tool.requested events — an unlogged decision is a decision
// that never happened (G3's audit half). M2 ships allow/deny; `ask`
// (escalation to a human) lands with the escalation flow in M3.
package policy

import (
	"encoding/json"
	"regexp"
	"strings"
)

// Decision is what the engine says about one tool call.
type Decision string

const (
	Allow Decision = "allow"
	Deny  Decision = "deny"
)

// Verdict is one decision plus its reason — the reason is what the model
// reads when denied, so it states a remediation, not a scolding.
type Verdict struct {
	Decision Decision `json:"decision"`
	Reason   string   `json:"reason,omitempty"`
}

// Engine evaluates a tool call. It must be pure and fast — no I/O; if a
// future rule needs context (budgets, rate limits), that data arrives as
// arguments, not as engine state.
type Engine interface {
	Check(toolName string, input json.RawMessage) Verdict
}

// Static is the M2 engine: per-tool defaults from the tool itself, plus
// a hard bash denylist. Honest about scope — this is plumbing with a
// real deny path, not a governance story.
type Static struct{}

func NewStatic() *Static { return &Static{} }

// sudoRe: sudo in command position (start of command or after ; && ||).
var sudoRe = regexp.MustCompile(`(^|[;&|]\s*)sudo\b`)

func (s *Static) Check(toolName string, input json.RawMessage) Verdict {
	if toolName != "bash" {
		return Verdict{Decision: Allow}
	}
	var in struct {
		Command string `json:"command"`
	}
	_ = json.Unmarshal(input, &in) // unparseable input is the tool's error, not policy's
	cmd := strings.TrimSpace(in.Command)
	if sudoRe.MatchString(cmd) {
		return Verdict{Decision: Deny,
			Reason: "sudo is not available in the sandbox; run the command without it"}
	}
	if reason, denied := rootDelete(cmd); denied {
		return Verdict{Decision: Deny, Reason: reason}
	}
	return Verdict{Decision: Allow}
}

// rootDelete catches `rm` with recursive+force flags aimed at / — the one
// command a model must never be able to run. Whitespace-level scan, not a
// shell parser: good enough to be a backstop, never the only defense
// (the sandbox is the boundary; this is governance).
func rootDelete(cmd string) (string, bool) {
	fields := strings.Fields(cmd)
	for i := 0; i < len(fields)-1; i++ {
		if !strings.HasSuffix(fields[i], "rm") {
			continue
		}
		j := i + 1
		recursive, force := false, false
		for j < len(fields) && strings.HasPrefix(fields[j], "-") && fields[j] != "--" {
			flags := strings.TrimLeft(fields[j], "-")
			recursive = recursive || strings.ContainsAny(flags, "rR")
			force = force || strings.ContainsAny(flags, "fF")
			j++
		}
		if recursive && force && j < len(fields) &&
			(fields[j] == "/" || fields[j] == "/*" || fields[j] == "/*/") {
			return "recursive delete of / is never allowed; delete specific paths under the workspace instead", true
		}
	}
	return "", false
}

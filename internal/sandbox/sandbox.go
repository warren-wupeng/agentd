// Package sandbox is where untrusted, model-generated code runs
// (ADR-001). Providers: docker (real dev isolation, enforceable network
// policy), e2b (Firecracker production tier, experimental), and exec
// (no-Docker dev/test fallback — zero isolation, said out loud).
package sandbox

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// MaxOutputBytes caps captured stdout/stderr per execution — the model
// does not need a 40MB log, and the event log must not hold one.
const MaxOutputBytes = 64 << 10

// Egress is the network policy for a sandbox (ADR-001: egress is the
// governance floor that applies to ALL providers). v0 is the binary an
// enterprise review asks about first; domain allowlists extend this
// type additively.
type Egress string

const (
	// EgressNone: no network access from inside the sandbox.
	EgressNone Egress = "none"
	// EgressAllow: unrestricted outbound network.
	EgressAllow Egress = "allow"
)

// Policy is what a provider is asked to enforce. Providers that cannot
// enforce an aspect MUST say so (see Handle.CanEnforceEgress) — honesty
// about capability beats silent non-enforcement.
type Policy struct {
	Egress Egress
}

func DefaultPolicy() Policy { return Policy{Egress: EgressNone} }

// ExecResult is one command execution. ExitCode != 0 is DATA for the
// model, not an error — tool semantic failures ride in results; only
// infrastructure failures (no sandbox, timeout kill, bad input) are
// errors from Exec.
type ExecResult struct {
	Command   string        `json:"command"`
	ExitCode  int           `json:"exit_code"`
	Stdout    string        `json:"stdout"`
	Stderr    string        `json:"stderr"`
	Truncated bool          `json:"truncated"`
	Duration  time.Duration `json:"-"`
}

// Handle is a session's execution context: a stable working directory
// that survives across steps and turns, plus a way to run commands in it.
// File access goes through the handle too — the workdir may live inside
// a remote sandbox (e2b), so os.* on Workdir() is wrong for some tiers.
type Handle interface {
	SessionID() uuid.UUID
	// Workdir is informational (paths in outputs); file access must use
	// ResolvePath + ReadFile/WriteFile below.
	Workdir() string
	// ResolvePath anchors a model-supplied path under the workdir and
	// rejects traversal outside it.
	ResolvePath(modelPath string) (string, error)
	Exec(ctx context.Context, command string, timeout time.Duration) (ExecResult, error)
	// ReadFile/WriteFile operate on sandbox-resolved paths (pass the
	// model-relative path; the handle anchors it).
	ReadFile(ctx context.Context, path string) ([]byte, error)
	WriteFile(ctx context.Context, path string, content []byte) error
	// CanEnforceEgress reports whether THIS provider actually isolates
	// the network (docker: yes; exec: never — no root, no namespaces).
	// Schedulers and the escape suite branch on this, not on hope.
	CanEnforceEgress() bool
}

// Provider creates (and memoizes) handles per session under a policy.
type Provider interface {
	Handle(sessionID uuid.UUID) (Handle, error)
	// SetPolicy sets the policy for ALL handles this provider creates
	// (process-level in M5; per-session when the config surface needs it).
	SetPolicy(p Policy)
	Policy() Policy
}

// truncateBytes caps output and flags it.
func truncateBytes(b []byte) (string, bool) {
	if len(b) <= MaxOutputBytes {
		return string(b), false
	}
	return string(b[:MaxOutputBytes]) + "\n...[output truncated]", true
}

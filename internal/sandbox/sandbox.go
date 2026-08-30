// Package sandbox is where untrusted, model-generated code runs
// (ADR-001). Two providers: docker (real dev/prod isolation) and exec
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
type Handle interface {
	SessionID() uuid.UUID
	// Workdir is for trusted code only (our file tools); model-generated
	// paths must go through ResolvePath.
	Workdir() string
	// ResolvePath anchors a model-supplied path under the workdir and
	// rejects traversal outside it.
	ResolvePath(modelPath string) (string, error)
	Exec(ctx context.Context, command string, timeout time.Duration) (ExecResult, error)
}

// Provider creates (and memoizes) handles per session.
type Provider interface {
	Handle(sessionID uuid.UUID) (Handle, error)
}

// truncateBytes caps output and flags it.
func truncateBytes(b []byte) (string, bool) {
	if len(b) <= MaxOutputBytes {
		return string(b), false
	}
	return string(b[:MaxOutputBytes]) + "\n...[output truncated]", true
}

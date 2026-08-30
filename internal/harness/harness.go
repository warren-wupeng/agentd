// Package harness is the seam where runtimes plug in (ADR-004): the
// native loop is the reference implementation, external harnesses
// (OpenCode first) are first-class peers. Adapters translate
// harness-native activity into the agentd event vocabulary; the log
// stays canonical and unchanged.
package harness

import (
	"context"
	"encoding/json"

	"github.com/google/uuid"
)

// CapabilitySet states what a harness can do — asymmetry is permanent
// and exposed honestly; schedulers match tasks to capabilities.
type CapabilitySet struct {
	// Hooks: the harness supports in-flight interception (native policy
	// hooks, OpenCode permission.ask delegation). Without it, governance
	// is launch config + sandbox egress + audit only.
	Hooks bool
	// Streaming: the harness can emit ephemeral deltas while a turn runs.
	Streaming bool
	// PermissionDelegate: the harness can delegate its permission asks
	// to the agentd policy engine (strongest governance surface).
	PermissionDelegate bool
}

// WorkerSpec is everything a harness needs to run one session's turn,
// fixed at launch from the session row and its pinned agent version.
type WorkerSpec struct {
	SessionID    uuid.UUID
	AgentID      uuid.UUID
	AgentVersion int
	// Config is the pinned agent-version config (model, system prompt,
	// enabled tools) — the same JSON the native loop reads.
	Config json.RawMessage
	// ResumeFrom optionally carries a checkpoint token from another
	// worker (M5 worker handoff; in-process round-trips work today).
	ResumeFrom *CheckpointToken
}

// CheckpointToken is opaque to everyone except the harness that minted
// it. It must be JSON — tokens are stored in events.
type CheckpointToken struct {
	Harness string          `json:"harness"` // which harness minted it
	Data    json.RawMessage `json:"data"`
}

// Handle is a launched worker: a harness-side execution context for one
// session. Handles are cheap to re-create — Launch is idempotent and
// replay-backed (harnesses persist their mapping in harness.launched
// events, so a restarted process recovers it from the log).
type Handle struct {
	Spec WorkerSpec
	// HarnessState is the harness's own durable pointer (e.g. the
	// OpenCode session id), recovered from the harness.launched event.
	HarnessState json.RawMessage
}

// Harness drives sessions of one runtime. Run advances ONE turn to its
// park point, appending normalized events; it is synchronous — the
// dispatcher owns concurrency. Like the native Step, Run is reentrant:
// correctness comes from the event log, not from harness-local state.
type Harness interface {
	Name() string
	Capabilities() CapabilitySet
	// Launch attaches (creating if needed) the harness-side worker and
	// returns the handle. Idempotent.
	Launch(ctx context.Context, spec WorkerSpec) (Handle, error)
	// Run drives one turn: reads unprocessed input from the log, does the
	// harness's work, appends normalized events, parks the session (G1
	// transitions via store).
	Run(ctx context.Context, h Handle) error
	// Checkpoint mints an opaque token sufficient to resume the session.
	Checkpoint(ctx context.Context, h Handle) (CheckpointToken, error)
	// Resume continues from a token minted by this harness.
	Resume(ctx context.Context, spec WorkerSpec, tok CheckpointToken) (Handle, error)
	// Interrupt asks an in-flight Run to stop at its next boundary.
	Interrupt(sessionID uuid.UUID)
}

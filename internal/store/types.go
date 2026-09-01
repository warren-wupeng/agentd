package store

import (
	"encoding/json"
	"time"

	"github.com/google/uuid"
)

// Agent is a named, versioned agent config family.
type Agent struct {
	ID          uuid.UUID `json:"id"`
	Name        string    `json:"name"`
	Description string    `json:"description"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// AgentVersion is one immutable config snapshot. jsonb normalizes bytes
// (key order, whitespace); equality is semantic, not bytewise.
type AgentVersion struct {
	ID        uuid.UUID       `json:"id"`
	AgentID   uuid.UUID       `json:"agent_id"`
	Version   int             `json:"version"`
	Config    json.RawMessage `json:"config"`
	CreatedAt time.Time       `json:"created_at"`
}

// SessionState is the session lifecycle (README design principle 5).
type SessionState string

const (
	StateRescheduling SessionState = "rescheduling"
	StateRunning      SessionState = "running"
	StateIdle         SessionState = "idle"
	StateTerminated   SessionState = "terminated"
)

// StopReason distinguishes "waiting on you" from "done" (README principle 5).
type StopReason string

const (
	StopRequiresAction   StopReason = "requires_action"
	StopEndTurn          StopReason = "end_turn"
	StopRetriesExhausted StopReason = "retries_exhausted"
)

// Session is one stateful run of an agent on a harness.
type Session struct {
	ID           uuid.UUID    `json:"id"`
	AgentID      uuid.UUID    `json:"agent_id"`
	AgentVersion int          `json:"agent_version"`
	Harness      string       `json:"harness"`
	State        SessionState `json:"state"`
	StopReason   *StopReason  `json:"stop_reason"`
	CreatedAt    time.Time    `json:"created_at"`
	UpdatedAt    time.Time    `json:"updated_at"`
}

// legalTransitions is the session state machine. terminated is final.
// rescheduling → idle exists so a kick that finds nothing to do (or a
// legacy row from the old creation default) can park honestly.
var legalTransitions = map[SessionState][]SessionState{
	StateRescheduling: {StateRunning, StateIdle, StateTerminated},
	StateRunning:      {StateIdle, StateTerminated},
	StateIdle:         {StateRunning, StateTerminated},
	StateTerminated:   {},
}

// LegalTargets reports the states a session may move to next. Read-only
// view of legalTransitions for clients and remediation hints.
func LegalTargets(from SessionState) []SessionState { return legalTransitions[from] }

// Actor identifies who produced an event.
type Actor string

const (
	ActorUser   Actor = "user"
	ActorAgent  Actor = "agent"
	ActorSystem Actor = "system"
)

// Event types known so far. The vocabulary grows with the milestones (see
// docs/design/agent-loop.md); unknown types are rejected at the store so a
// typo can never enter the log.
const (
	EventSessionCreated      = "session.created"
	EventSessionStateChanged = "session.state_changed"
	EventMessageUser         = "message.user"
	EventMessageAssistant    = "message.assistant"
	EventToolRequested       = "tool.requested"
	EventToolCompleted       = "tool.completed"
	EventToolFailed          = "tool.failed"
	EventTurnCompleted       = "turn.completed"
	EventEscalationRequested = "escalation.requested"
	EventHarnessLaunched     = "harness.launched"
)

// Event is one appended fact in a session's log (ADR-003).
type Event struct {
	ID          uuid.UUID       `json:"id"`
	SessionID   uuid.UUID       `json:"session_id"`
	Seq         int64           `json:"seq"`
	Type        string          `json:"type"`
	Actor       Actor           `json:"actor"`
	Payload     json.RawMessage `json:"payload"`
	ProcessedAt *time.Time      `json:"processed_at"`
	CreatedAt   time.Time       `json:"created_at"`
}

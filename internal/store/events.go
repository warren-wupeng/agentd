package store

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/warren-wupeng/agentd/internal/agentderr"
)

const eventCols = "id, session_id, seq, type, actor, payload, processed_at, created_at"

// knownEventTypes is the closed vocabulary. Closed on purpose: a typo'd type
// in the log would poison replay, so the store refuses unknown types.
var knownEventTypes = map[string]bool{
	EventSessionCreated:      true,
	EventSessionStateChanged: true,
	EventMessageUser:         true,
	EventMessageAssistant:    true,
	EventToolRequested:       true,
	EventToolCompleted:       true,
	EventToolFailed:          true,
	EventTurnCompleted:       true,
}

// AppendEvent adds one event and bumps the session's updated_at, atomically.
// Terminated sessions reject new events — a terminated log is immutable.
func (s *Store) AppendEvent(ctx context.Context, sessionID uuid.UUID, eventType string, actor Actor, payload json.RawMessage) (*Event, error) {
	if !knownEventTypes[eventType] {
		return nil, agentderr.InvalidInput(
			"unknown event type "+eventType,
			"known types: session.created, session.state_changed, message.user, message.assistant, tool.requested, tool.completed, tool.failed, turn.completed")
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, agentderr.Internal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var state SessionState
	err = tx.QueryRow(ctx, `SELECT state FROM sessions WHERE id = $1 FOR UPDATE`, sessionID).Scan(&state)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, sessionNotFound(sessionID)
	}
	if err != nil {
		return nil, agentderr.Internal(err)
	}
	if state == StateTerminated {
		return nil, agentderr.Conflict(
			"session is terminated; its event log is immutable",
			"create a new session via POST /v1/sessions")
	}

	ev, err := insertEvent(ctx, tx, sessionID, eventType, actor, payload)
	if err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, `UPDATE sessions SET updated_at = now() WHERE id = $1`, sessionID); err != nil {
		return nil, agentderr.Internal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, agentderr.Internal(err)
	}
	return ev, nil
}

func insertEvent(ctx context.Context, tx pgx.Tx, sessionID uuid.UUID, eventType string, actor Actor, payload json.RawMessage) (*Event, error) {
	var ev Event
	err := tx.QueryRow(ctx,
		`INSERT INTO events (session_id, type, actor, payload) VALUES ($1, $2, $3, $4) RETURNING `+eventCols,
		sessionID, eventType, actor, payload,
	).Scan(&ev.ID, &ev.SessionID, &ev.Seq, &ev.Type, &ev.Actor, &ev.Payload, &ev.ProcessedAt, &ev.CreatedAt)
	if err != nil {
		return nil, agentderr.Internal(err)
	}
	return &ev, nil
}

// ListEvents is the replay primitive (ADR-003): everything after afterSeq,
// in order. Re-reading the same range always yields the same rows.
func (s *Store) ListEvents(ctx context.Context, sessionID uuid.UUID, afterSeq int64, limit int) ([]Event, error) {
	if _, err := s.GetSession(ctx, sessionID); err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx,
		`SELECT `+eventCols+` FROM events WHERE session_id = $1 AND seq > $2 ORDER BY seq LIMIT $3`,
		sessionID, afterSeq, limit)
	if err != nil {
		return nil, agentderr.Internal(err)
	}
	defer rows.Close()

	out := []Event{}
	for rows.Next() {
		var ev Event
		if err := rows.Scan(&ev.ID, &ev.SessionID, &ev.Seq, &ev.Type, &ev.Actor,
			&ev.Payload, &ev.ProcessedAt, &ev.CreatedAt); err != nil {
			return nil, agentderr.Internal(err)
		}
		out = append(out, ev)
	}
	if err := rows.Err(); err != nil {
		return nil, agentderr.Internal(err)
	}
	return out, nil
}

// ClaimEvent sets processed_at exactly once (ADR-003's queue gate). A second
// claim returns the event unchanged — idempotent by construction.
func (s *Store) ClaimEvent(ctx context.Context, sessionID, eventID uuid.UUID) (*Event, error) {
	var ev Event
	err := s.pool.QueryRow(ctx,
		`UPDATE events SET processed_at = now()
		 WHERE id = $1 AND session_id = $2 AND processed_at IS NULL
		 RETURNING `+eventCols, eventID, sessionID,
	).Scan(&ev.ID, &ev.SessionID, &ev.Seq, &ev.Type, &ev.Actor, &ev.Payload, &ev.ProcessedAt, &ev.CreatedAt)
	if err == nil {
		return &ev, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return nil, agentderr.Internal(err)
	}

	// Already claimed (or wrong session): return the row as-is if it exists.
	err = s.pool.QueryRow(ctx,
		`SELECT `+eventCols+` FROM events WHERE id = $1 AND session_id = $2`,
		eventID, sessionID,
	).Scan(&ev.ID, &ev.SessionID, &ev.Seq, &ev.Type, &ev.Actor, &ev.Payload, &ev.ProcessedAt, &ev.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, agentderr.NotFound(
			"event "+eventID.String()+" not found in session "+sessionID.String(),
			"replay the log at GET /v1/sessions/{id}/events to discover event ids")
	}
	if err != nil {
		return nil, agentderr.Internal(err)
	}
	return &ev, nil
}

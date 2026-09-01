package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/warren-wupeng/agentd/internal/agentderr"
)

const sessionCols = "id, agent_id, agent_version, harness, state, stop_reason, created_at, updated_at"

// validHarnessRe: harness names are DNS-ish — the registry of known
// harnesses lives at the API layer (injected from the harness registry);
// the store validates format only. Mechanics here, policy above.
var validHarnessRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)

// CreateSession pins an agent version and opens the log with a
// session.created event, in one transaction. version 0 means "latest".
func (s *Store) CreateSession(ctx context.Context, agentID uuid.UUID, version int, harness string) (*Session, *Event, error) {
	if !validHarnessRe.MatchString(harness) {
		return nil, nil, agentderr.InvalidInput(
			fmt.Sprintf("invalid harness %q", harness),
			"harness names are lowercase letters, digits, and dashes (max 32); the known set is listed by the API layer")
	}

	resolved := version
	if resolved == 0 {
		latest, err := s.LatestAgentVersion(ctx, agentID)
		if err != nil {
			return nil, nil, err
		}
		resolved = latest.Version
	} else if _, err := s.GetAgentVersion(ctx, agentID, resolved); err != nil {
		return nil, nil, err
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, nil, agentderr.Internal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var sess Session
	err = tx.QueryRow(ctx,
		`INSERT INTO sessions (agent_id, agent_version, harness) VALUES ($1, $2, $3) RETURNING `+sessionCols,
		agentID, resolved, harness,
	).Scan(&sess.ID, &sess.AgentID, &sess.AgentVersion, &sess.Harness, &sess.State,
		&sess.StopReason, &sess.CreatedAt, &sess.UpdatedAt)
	if err != nil {
		return nil, nil, agentderr.Internal(err)
	}

	payload, _ := json.Marshal(map[string]any{
		"agent_id": agentID, "agent_version": resolved, "harness": harness,
	})
	ev, err := insertEvent(ctx, tx, sess.ID, EventSessionCreated, ActorSystem, payload)
	if err != nil {
		return nil, nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, nil, agentderr.Internal(err)
	}
	return &sess, ev, nil
}

// GetSession returns one session by id.
func (s *Store) GetSession(ctx context.Context, id uuid.UUID) (*Session, error) {
	var sess Session
	err := s.pool.QueryRow(ctx, `SELECT `+sessionCols+` FROM sessions WHERE id = $1`, id).
		Scan(&sess.ID, &sess.AgentID, &sess.AgentVersion, &sess.Harness, &sess.State,
			&sess.StopReason, &sess.CreatedAt, &sess.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, sessionNotFound(id)
	}
	if err != nil {
		return nil, agentderr.Internal(err)
	}
	return &sess, nil
}

// ListSessions returns sessions newest-first, optionally filtered.
func (s *Store) ListSessions(ctx context.Context, state SessionState, agentID *uuid.UUID, limit int) ([]Session, error) {
	var conds []string
	var args []any
	if state != "" {
		args = append(args, state)
		conds = append(conds, fmt.Sprintf("state = $%d", len(args)))
	}
	if agentID != nil {
		args = append(args, *agentID)
		conds = append(conds, fmt.Sprintf("agent_id = $%d", len(args)))
	}
	args = append(args, limit)
	where := ""
	if len(conds) > 0 {
		where = " WHERE " + strings.Join(conds, " AND ")
	}

	rows, err := s.pool.Query(ctx,
		`SELECT `+sessionCols+` FROM sessions`+where+
			fmt.Sprintf(" ORDER BY created_at DESC LIMIT $%d", len(args)), args...)
	if err != nil {
		return nil, agentderr.Internal(err)
	}
	defer rows.Close()

	out := []Session{}
	for rows.Next() {
		var sess Session
		if err := rows.Scan(&sess.ID, &sess.AgentID, &sess.AgentVersion, &sess.Harness, &sess.State,
			&sess.StopReason, &sess.CreatedAt, &sess.UpdatedAt); err != nil {
			return nil, agentderr.Internal(err)
		}
		out = append(out, sess)
	}
	if err := rows.Err(); err != nil {
		return nil, agentderr.Internal(err)
	}
	return out, nil
}

// TransitionSession is the ONLY way session state changes (G1): it validates
// the edge, updates the row, and appends session.state_changed — atomically.
// stopReason is meaningful only when transitioning to idle.
func (s *Store) TransitionSession(ctx context.Context, id uuid.UUID, to SessionState, stopReason *StopReason) (*Session, *Event, error) {
	if _, known := legalTransitions[to]; !known {
		return nil, nil, agentderr.InvalidInput(
			fmt.Sprintf("unknown state %q", to),
			"valid states: rescheduling, running, idle, terminated")
	}
	if to != StateIdle && stopReason != nil {
		return nil, nil, agentderr.InvalidInput(
			"stop_reason only applies when transitioning to idle",
			"omit stop_reason, or transition to idle with one of: requires_action, end_turn, retries_exhausted")
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, nil, agentderr.Internal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var from SessionState
	err = tx.QueryRow(ctx, `SELECT state FROM sessions WHERE id = $1 FOR UPDATE`, id).Scan(&from)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil, sessionNotFound(id)
	}
	if err != nil {
		return nil, nil, agentderr.Internal(err)
	}

	legal := false
	for _, t := range LegalTargets(from) {
		if t == to {
			legal = true
			break
		}
	}
	if !legal {
		return nil, nil, agentderr.InvalidTransition(
			fmt.Sprintf("cannot transition session from %q to %q", from, to),
			fmt.Sprintf("legal transitions from %q: %s", from, joinStates(LegalTargets(from))))
	}

	var sess Session
	err = tx.QueryRow(ctx,
		`UPDATE sessions SET state = $2, stop_reason = $3, updated_at = now()
		 WHERE id = $1 RETURNING `+sessionCols,
		id, to, stopReason,
	).Scan(&sess.ID, &sess.AgentID, &sess.AgentVersion, &sess.Harness, &sess.State,
		&sess.StopReason, &sess.CreatedAt, &sess.UpdatedAt)
	if err != nil {
		return nil, nil, agentderr.Internal(err)
	}

	payload, _ := json.Marshal(map[string]any{"from": from, "to": to, "stop_reason": stopReason})
	ev, err := insertEvent(ctx, tx, id, EventSessionStateChanged, ActorSystem, payload)
	if err != nil {
		return nil, nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, nil, agentderr.Internal(err)
	}
	return &sess, ev, nil
}

func joinStates(states []SessionState) string {
	if len(states) == 0 {
		return "(none — terminal state)"
	}
	ss := make([]string, len(states))
	for i, s := range states {
		ss[i] = string(s)
	}
	return strings.Join(ss, ", ")
}

func sessionNotFound(id uuid.UUID) *agentderr.Error {
	return agentderr.NotFound(
		fmt.Sprintf("session %s not found", id),
		"list sessions at GET /v1/sessions; create one via POST /v1/sessions")
}

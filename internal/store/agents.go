package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/warren-wupeng/agentd/internal/agentderr"
)

const agentCols = "id, name, description, created_at, updated_at"
const versionCols = "id, agent_id, version, config, created_at"

// CreateAgent inserts the agent and its version 1 in one transaction.
func (s *Store) CreateAgent(ctx context.Context, name, description string, config json.RawMessage) (*Agent, *AgentVersion, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, nil, agentderr.Internal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var a Agent
	err = tx.QueryRow(ctx,
		`INSERT INTO agents (name, description) VALUES ($1, $2) RETURNING `+agentCols,
		name, description,
	).Scan(&a.ID, &a.Name, &a.Description, &a.CreatedAt, &a.UpdatedAt)
	if isUniqueViolation(err) {
		return nil, nil, agentderr.Conflict(
			fmt.Sprintf("agent name %q already exists", name),
			"choose a different name, or create a new version of the existing agent via PUT /v1/agents/{id}")
	}
	if err != nil {
		return nil, nil, agentderr.Internal(err)
	}

	v, err := insertVersion(ctx, tx, a.ID, 1, config)
	if err != nil {
		return nil, nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, nil, agentderr.Internal(err)
	}
	return &a, v, nil
}

// CreateAgentVersion appends the next immutable version. The parent row is
// locked so concurrent creates cannot collide on (agent_id, version).
func (s *Store) CreateAgentVersion(ctx context.Context, agentID uuid.UUID, config json.RawMessage) (*AgentVersion, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, agentderr.Internal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var next int
	err = tx.QueryRow(ctx,
		`SELECT COALESCE((SELECT MAX(version) FROM agent_versions WHERE agent_id = $1), 0) + 1
		 FROM agents WHERE id = $1 FOR UPDATE`, agentID).Scan(&next)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, agentNotFound(agentID)
	}
	if err != nil {
		return nil, agentderr.Internal(err)
	}

	v, err := insertVersion(ctx, tx, agentID, next, config)
	if err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, `UPDATE agents SET updated_at = now() WHERE id = $1`, agentID); err != nil {
		return nil, agentderr.Internal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, agentderr.Internal(err)
	}
	return v, nil
}

func insertVersion(ctx context.Context, tx pgx.Tx, agentID uuid.UUID, version int, config json.RawMessage) (*AgentVersion, error) {
	var v AgentVersion
	err := tx.QueryRow(ctx,
		`INSERT INTO agent_versions (agent_id, version, config) VALUES ($1, $2, $3) RETURNING `+versionCols,
		agentID, version, config,
	).Scan(&v.ID, &v.AgentID, &v.Version, &v.Config, &v.CreatedAt)
	if err != nil {
		return nil, agentderr.Internal(err)
	}
	return &v, nil
}

// GetAgent returns one agent by id.
func (s *Store) GetAgent(ctx context.Context, id uuid.UUID) (*Agent, error) {
	var a Agent
	err := s.pool.QueryRow(ctx, `SELECT `+agentCols+` FROM agents WHERE id = $1`, id).
		Scan(&a.ID, &a.Name, &a.Description, &a.CreatedAt, &a.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, agentNotFound(id)
	}
	if err != nil {
		return nil, agentderr.Internal(err)
	}
	return &a, nil
}

// AgentSummary pairs an agent with its latest version number (0 = none,
// which cannot happen through the API but keeps the scan total).
type AgentSummary struct {
	Agent
	LatestVersion int `json:"latest_version"`
}

// ListAgents returns agents newest-first, capped at limit.
func (s *Store) ListAgents(ctx context.Context, limit int) ([]AgentSummary, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT a.id, a.name, a.description, a.created_at, a.updated_at,
		        COALESCE((SELECT MAX(version) FROM agent_versions av WHERE av.agent_id = a.id), 0)
		 FROM agents a ORDER BY a.created_at DESC LIMIT $1`, limit)
	if err != nil {
		return nil, agentderr.Internal(err)
	}
	defer rows.Close()

	out := []AgentSummary{}
	for rows.Next() {
		var as AgentSummary
		if err := rows.Scan(&as.ID, &as.Name, &as.Description, &as.CreatedAt, &as.UpdatedAt, &as.LatestVersion); err != nil {
			return nil, agentderr.Internal(err)
		}
		out = append(out, as)
	}
	if err := rows.Err(); err != nil {
		return nil, agentderr.Internal(err)
	}
	return out, nil
}

// GetAgentVersion returns one immutable snapshot, with remediation that
// names the latest version when the requested one does not exist.
func (s *Store) GetAgentVersion(ctx context.Context, agentID uuid.UUID, version int) (*AgentVersion, error) {
	var v AgentVersion
	err := s.pool.QueryRow(ctx,
		`SELECT `+versionCols+` FROM agent_versions WHERE agent_id = $1 AND version = $2`,
		agentID, version,
	).Scan(&v.ID, &v.AgentID, &v.Version, &v.Config, &v.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		if _, gerr := s.GetAgent(ctx, agentID); gerr != nil {
			return nil, gerr
		}
		latest, lerr := s.latestVersion(ctx, agentID)
		if lerr != nil {
			return nil, lerr
		}
		return nil, agentderr.NotFound(
			fmt.Sprintf("agent %s has no version %d (latest is %d)", agentID, version, latest),
			fmt.Sprintf("list versions at GET /v1/agents/%s/versions, or pin version %d", agentID, latest))
	}
	if err != nil {
		return nil, agentderr.Internal(err)
	}
	return &v, nil
}

// ListAgentVersions returns every version of an agent, newest-first.
func (s *Store) ListAgentVersions(ctx context.Context, agentID uuid.UUID) ([]AgentVersion, error) {
	if _, err := s.GetAgent(ctx, agentID); err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx,
		`SELECT `+versionCols+` FROM agent_versions WHERE agent_id = $1 ORDER BY version DESC`, agentID)
	if err != nil {
		return nil, agentderr.Internal(err)
	}
	defer rows.Close()

	out := []AgentVersion{}
	for rows.Next() {
		var v AgentVersion
		if err := rows.Scan(&v.ID, &v.AgentID, &v.Version, &v.Config, &v.CreatedAt); err != nil {
			return nil, agentderr.Internal(err)
		}
		out = append(out, v)
	}
	if err := rows.Err(); err != nil {
		return nil, agentderr.Internal(err)
	}
	return out, nil
}

// DeleteAgent refuses while live sessions pin the agent (409 — deletion is
// a governance act, not an accident). Without sessions it cascades versions.
func (s *Store) DeleteAgent(ctx context.Context, id uuid.UUID) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return agentderr.Internal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var n int
	if err := tx.QueryRow(ctx, `SELECT COUNT(*) FROM sessions WHERE agent_id = $1`, id).Scan(&n); err != nil {
		return agentderr.Internal(err)
	}
	if n > 0 {
		return agentderr.Conflict(
			fmt.Sprintf("agent %s has %d session(s) pinned to its versions", id, n),
			"terminate the sessions first; session history is preserved even for idle sessions")
	}

	tag, err := tx.Exec(ctx, `DELETE FROM agents WHERE id = $1`, id)
	if err != nil {
		return agentderr.Internal(err)
	}
	if tag.RowsAffected() == 0 {
		return agentNotFound(id)
	}
	if err := tx.Commit(ctx); err != nil {
		return agentderr.Internal(err)
	}
	return nil
}

func (s *Store) latestVersion(ctx context.Context, agentID uuid.UUID) (int, error) {
	var v int
	err := s.pool.QueryRow(ctx,
		`SELECT COALESCE(MAX(version), 0) FROM agent_versions WHERE agent_id = $1`, agentID).Scan(&v)
	if err != nil {
		return 0, agentderr.Internal(err)
	}
	return v, nil
}

// LatestAgentVersion resolves "no pin given" to the newest snapshot.
func (s *Store) LatestAgentVersion(ctx context.Context, agentID uuid.UUID) (*AgentVersion, error) {
	latest, err := s.latestVersion(ctx, agentID)
	if err != nil {
		return nil, err
	}
	if latest == 0 {
		return nil, agentNotFound(agentID)
	}
	return s.GetAgentVersion(ctx, agentID, latest)
}

func agentNotFound(id uuid.UUID) *agentderr.Error {
	return agentderr.NotFound(
		fmt.Sprintf("agent %s not found", id),
		"list agents at GET /v1/agents; create one via POST /v1/agents")
}

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

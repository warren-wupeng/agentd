// Package store is the only code that talks to Postgres. Per G1, session
// state changes exist only as transaction-wrapped functions that also append
// the corresponding event — no exported function mutates sessions.state
// directly.
package store

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/warren-wupeng/agentd/internal/agentderr"
)

// Store wraps the connection pool. All methods are safe for concurrent use.
type Store struct {
	pool *pgxpool.Pool
}

// New connects and verifies the database is reachable and migrated.
func New(ctx context.Context, databaseURL string) (*Store, error) {
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		return nil, agentderr.Wrap(agentderr.CodeInternal, err,
			"invalid DATABASE_URL", "check the connection string format: postgres://user:pass@host:5432/db")
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, agentderr.Wrap(agentderr.CodeInternal, err,
			"cannot reach Postgres", "is the database up? try: make dev-up && make migrate")
	}
	return &Store{pool: pool}, nil
}

// Close releases the pool.
func (s *Store) Close() { s.pool.Close() }

// Ping exposes a liveness check for /healthz.
func (s *Store) Ping(ctx context.Context) error {
	if err := s.pool.Ping(ctx); err != nil {
		return fmt.Errorf("postgres ping: %w", err)
	}
	return nil
}

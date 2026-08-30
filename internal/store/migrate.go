package store

import (
	"context"

	migrate "github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/postgres"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	"github.com/warren-wupeng/agentd/internal/agentderr"
	"github.com/warren-wupeng/agentd/migrations"
)

// Migrate applies ("up") or rolls back ("down") all migrations. The binary
// carries its own schema — no separate CLI tool required.
func Migrate(databaseURL, direction string) error {
	return MigrateContext(context.Background(), databaseURL, direction)
}

// MigrateContext is Migrate with a caller's context.
func MigrateContext(_ context.Context, databaseURL, direction string) error {
	src, err := iofs.New(migrations.FS, ".")
	if err != nil {
		return agentderr.Internal(err)
	}
	m, err := migrate.NewWithSourceInstance("iofs", src, databaseURL)
	if err != nil {
		return agentderr.Wrap(agentderr.CodeInternal, err,
			"cannot open database for migration",
			"check DATABASE_URL (postgres://user:pass@host:5432/db)")
	}
	defer func() { _, _ = m.Close() }()

	switch direction {
	case "up":
		err = m.Up()
	case "down":
		err = m.Down()
	default:
		return agentderr.InvalidInput("unknown migration direction "+direction, "use: up, down")
	}
	if err != nil && err != migrate.ErrNoChange {
		return agentderr.Wrap(agentderr.CodeInternal, err,
			"migration "+direction+" failed",
			"inspect the database state; fix and re-run `agentd-server migrate`")
	}
	return nil
}

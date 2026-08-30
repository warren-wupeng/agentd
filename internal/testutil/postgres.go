// Package testutil spins up a real, throwaway Postgres for tests —
// no Docker, no root, no shared state between test runs.
//
// Every test binary gets its OWN Postgres on a lock-claimed port (go test
// runs package binaries in parallel; a shared or raced port turns into
// cross-binary table drops). Lifecycle is TestMain-scoped so the database
// actually stops when the binary exits.
package testutil

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"

	embeddedpostgres "github.com/fergusstrange/embedded-postgres"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/warren-wupeng/agentd/internal/store"
)

var dbURL string

// Main is the TestMain entry point for packages that need Postgres:
//
//	func TestMain(m *testing.M) { testutil.Main(m) }
func Main(m *testing.M) {
	os.Exit(run(m))
}

func run(m *testing.M) int {
	port, release, err := claimPort()
	if err != nil {
		fmt.Fprintln(os.Stderr, "testutil:", err)
		return 1
	}
	defer release()

	// embedded-postgres defaults to ONE shared runtime dir and wipes it at
	// start — fine for a single binary, fatal for parallel test packages.
	// Everything this binary touches lives under a per-port runtime dir
	// (binaries copy, data dir, pid file); only the unix socket sits outside.
	rtDir := filepath.Join(os.TempDir(), fmt.Sprintf("agentd-test-pg-rt-%d", port))
	sockDir := filepath.Join(os.TempDir(), fmt.Sprintf("agentd-test-pg-sock-%d", port))
	if err := os.MkdirAll(sockDir, 0o700); err != nil {
		fmt.Fprintln(os.Stderr, "testutil:", err)
		return 1
	}
	ep := embeddedpostgres.NewDatabase(embeddedpostgres.DefaultConfig().
		Version(embeddedpostgres.V16).
		Username("agentd").
		Password("agentd").
		Database("agentd").
		Port(uint32(port)).
		RuntimePath(rtDir).
		StartParameters(map[string]string{
			// Per-binary socket dir too, or one postmaster's health check can
			// answer for another through /tmp/.s.PGSQL.<port>.
			"unix_socket_directories": sockDir,
		}))
	if err := ep.Start(); err != nil {
		fmt.Fprintln(os.Stderr, "testutil: start embedded postgres:", err)
		return 1
	}
	dbURL = fmt.Sprintf("postgres://agentd:agentd@127.0.0.1:%d/agentd?sslmode=disable", port)

	code := m.Run()
	if err := ep.Stop(); err != nil {
		fmt.Fprintln(os.Stderr, "testutil: stop postgres:", err)
	}
	_ = os.RemoveAll(rtDir)
	_ = os.RemoveAll(sockDir)
	return code
}

// claimPort takes a port out of a reserved range using an atomic lockfile,
// so two test binaries can never pick the same one. The lock records the
// owner's PID: a lock whose owner is still alive is never stolen — a fresh
// claimant's postgres simply may not be listening yet, which is
// indistinguishable from a crashed run by port probing alone. Stale locks
// (dead owner + verifiably free port) are reclaimed; the O_EXCL recreate
// serializes concurrent reclaimers.
func claimPort() (int, func(), error) {
	for p := 25432; p < 25532; p++ {
		lock := filepath.Join(os.TempDir(), fmt.Sprintf("agentd-test-pg-%d.lock", p))
		f, err := os.OpenFile(lock, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			if errors.Is(err, os.ErrExist) && lockOwnerDead(lock) && portFree(p) {
				_ = os.Remove(lock) // stale lock; retry this port
				p--
			}
			continue
		}
		_, _ = fmt.Fprint(f, os.Getpid())
		_ = f.Close()
		if !portFree(p) {
			_ = os.Remove(lock)
			continue
		}
		return p, func() { _ = os.Remove(lock) }, nil
	}
	return 0, nil, errors.New("no free test postgres port in range 25432-25531")
}

// lockOwnerDead reports whether the process that wrote the lock no longer
// exists. Unreadable or unparseable locks count as dead.
func lockOwnerDead(lock string) bool {
	data, err := os.ReadFile(lock)
	if err != nil {
		return true
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid <= 0 {
		return true
	}
	if err := syscall.Kill(pid, 0); err == nil || errors.Is(err, syscall.EPERM) {
		return false
	}
	return true
}

func portFree(p int) bool {
	l, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", p))
	if err != nil {
		return false
	}
	_ = l.Close()
	return true
}

// DatabaseURL exposes this binary's test database URL.
func DatabaseURL(t *testing.T) string {
	t.Helper()
	if dbURL == "" {
		t.Fatal("testutil: no database — add `func TestMain(m *testing.M) { testutil.Main(m) }` to the package")
	}
	return dbURL
}

// NewStore returns a store on a freshly migrated, empty database.
// Tables are truncated, so tests in the same package share one Postgres
// but never see each other's rows. Tests using NewStore must not run in
// parallel with each other.
//
// Truncation uses its own connection on purpose: store exposes no raw-SQL
// escape hatch, so G1 (state changes only via event-emitting functions)
// holds even in test code.
func NewStore(t *testing.T) *store.Store {
	t.Helper()
	url := DatabaseURL(t)
	if err := store.Migrate(url, "up"); err != nil {
		t.Fatalf("migrate up: %v", err)
	}
	st, err := store.New(context.Background(), url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(st.Close)
	truncate(t, url)
	return st
}

func truncate(t *testing.T, url string) {
	t.Helper()
	pool, err := pgxpool.New(context.Background(), url)
	if err != nil {
		t.Fatalf("connect for truncate: %v", err)
	}
	defer pool.Close()
	if _, err := pool.Exec(context.Background(),
		`TRUNCATE events, sessions, agent_versions, agents RESTART IDENTITY CASCADE`); err != nil {
		t.Fatalf("truncate: %v", err)
	}
}

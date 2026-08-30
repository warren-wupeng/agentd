// Package config loads process configuration from the environment.
// Missing or invalid values fail fast with a remediation (G5), never a
// silent default for anything that matters.
package config

import (
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"strconv"

	"github.com/warren-wupeng/agentd/internal/agentderr"
)

// Config is the process configuration.
type Config struct {
	DatabaseURL string
	HTTPAddr    string
	LogLevel    slog.Level

	// Native loop (M2). ModelBaseURL empty = CRUD-only process; sessions
	// that try to run park with retries_exhausted and a remediation.
	ModelBaseURL string
	ModelAPIKey  string
	SandboxProv  string // "exec" | "docker"
	SandboxBase  string
	LoopMaxSteps int
	LoopRetries  int
}

// Load reads and validates the environment.
func Load() (*Config, error) {
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		return nil, agentderr.InvalidInput(
			"DATABASE_URL is not set",
			"export DATABASE_URL=postgres://agentd:agentd@localhost:5432/agentd?sslmode=disable (see compose.yml)")
	}
	u, err := url.Parse(dbURL)
	if err != nil || (u.Scheme != "postgres" && u.Scheme != "postgresql") {
		return nil, agentderr.InvalidInput(
			"DATABASE_URL must be a postgres:// URL",
			"example: postgres://agentd:agentd@localhost:5432/agentd?sslmode=disable")
	}

	addr := os.Getenv("HTTP_ADDR")
	if addr == "" {
		addr = ":8080"
	}

	level := slog.LevelInfo
	if l := os.Getenv("LOG_LEVEL"); l != "" {
		if err := level.UnmarshalText([]byte(l)); err != nil {
			return nil, agentderr.InvalidInput(
				fmt.Sprintf("LOG_LEVEL %q is invalid", l),
				"one of: debug, info, warn, error")
		}
	}

	sandboxProv := os.Getenv("SANDBOX_PROVIDER")
	if sandboxProv == "" {
		sandboxProv = "exec"
	}
	if sandboxProv != "exec" && sandboxProv != "docker" {
		return nil, agentderr.InvalidInput(
			"SANDBOX_PROVIDER must be \"exec\" or \"docker\", got "+sandboxProv,
			"exec = dev fallback with zero isolation; docker = ADR-001 dev isolation")
	}
	sandboxBase := os.Getenv("SANDBOX_BASE")
	if sandboxBase == "" {
		sandboxBase = filepath.Join(os.TempDir(), "agentd-sandboxes")
	}

	maxSteps := 40
	if raw := os.Getenv("LOOP_MAX_STEPS"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > 1000 {
			return nil, agentderr.InvalidInput(
				fmt.Sprintf("LOOP_MAX_STEPS %q must be an integer 1..1000", raw),
				"it is the per-turn assistant-message cap before retries_exhausted")
		}
		maxSteps = n
	}
	retries := 3
	if raw := os.Getenv("LOOP_MODEL_RETRIES"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > 10 {
			return nil, agentderr.InvalidInput(
				fmt.Sprintf("LOOP_MODEL_RETRIES %q must be an integer 1..10", raw),
				"model-call attempts before the turn parks with retries_exhausted")
		}
		retries = n
	}

	return &Config{
		DatabaseURL:  dbURL,
		HTTPAddr:     addr,
		LogLevel:     level,
		ModelBaseURL: os.Getenv("MODEL_BASE_URL"),
		ModelAPIKey:  os.Getenv("MODEL_API_KEY"),
		SandboxProv:  sandboxProv,
		SandboxBase:  sandboxBase,
		LoopMaxSteps: maxSteps,
		LoopRetries:  retries,
	}, nil
}

// Package config loads process configuration from the environment.
// Missing or invalid values fail fast with a remediation (G5), never a
// silent default for anything that matters.
package config

import (
	"fmt"
	"log/slog"
	"net/url"
	"os"

	"github.com/warren-wupeng/agentd/internal/agentderr"
)

// Config is the process configuration.
type Config struct {
	DatabaseURL string
	HTTPAddr    string
	LogLevel    slog.Level
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

	return &Config{DatabaseURL: dbURL, HTTPAddr: addr, LogLevel: level}, nil
}

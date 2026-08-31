// Package mcp is the credential-injection proxy: the control plane
// calls MCP servers WITH credentials from the vault so the sandbox
// never sees them (M6's whole point). v0 proxies plain HTTP
// request/response; the MCP streaming transports land behind the same
// registry when a real server needs them.
package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/warren-wupeng/agentd/internal/agentderr"
	"github.com/warren-wupeng/agentd/internal/vault"
)

// MaxResponseBytes caps an upstream MCP response — tool data, not a
// firehose into the event log.
const MaxResponseBytes = 256 << 10

// Server is one registered MCP upstream. SecretName is a vault
// REFERENCE — a value never lives here.
type Server struct {
	Name       string `json:"name"`
	BaseURL    string `json:"base_url"`
	SecretName string `json:"secret_name"`
	AuthHeader string `json:"auth_header"`
	AuthScheme string `json:"auth_scheme"`
}

type MCP struct {
	pool   *pgxpool.Pool
	vault  *vault.Vault
	client *http.Client
}

func New(ctx context.Context, databaseURL string, v *vault.Vault) (*MCP, error) {
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		return nil, agentderr.Internal(err)
	}
	return &MCP{
		pool:  pool,
		vault: v,
		client: &http.Client{
			Timeout: 30 * time.Second,
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				return http.ErrUseLastResponse // credentials must not leak via redirects
			},
		},
	}, nil
}

func (m *MCP) Close() error { m.pool.Close(); return nil }

// Register adds or replaces an MCP server definition.
func (m *MCP) Register(ctx context.Context, s Server) error {
	if s.Name == "" || s.BaseURL == "" || s.SecretName == "" {
		return agentderr.InvalidInput(
			"name, base_url, and secret_name are required",
			`example: {"name":"github","base_url":"https://mcp.example.com","secret_name":"github"}`)
	}
	u, err := url.Parse(s.BaseURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return agentderr.InvalidInput(
			"base_url must be an absolute http(s) URL",
			"example: https://mcp.example.com — no paths to other origins, no redirects followed")
	}
	if s.AuthHeader == "" {
		s.AuthHeader = "Authorization"
	}
	if s.AuthScheme == "" {
		s.AuthScheme = "Bearer"
	}
	_, err = m.pool.Exec(ctx, `
		INSERT INTO mcp_servers (name, base_url, secret_name, auth_header, auth_scheme)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (name) DO UPDATE SET
		  base_url = EXCLUDED.base_url, secret_name = EXCLUDED.secret_name,
		  auth_header = EXCLUDED.auth_header, auth_scheme = EXCLUDED.auth_scheme`,
		s.Name, s.BaseURL, s.SecretName, s.AuthHeader, s.AuthScheme)
	if err != nil {
		return agentderr.Internal(err)
	}
	return nil
}

// List returns the registry. Secret references only, never values.
func (m *MCP) List(ctx context.Context) ([]Server, error) {
	rows, err := m.pool.Query(ctx,
		`SELECT name, base_url, secret_name, auth_header, auth_scheme FROM mcp_servers ORDER BY name`)
	if err != nil {
		return nil, agentderr.Internal(err)
	}
	defer rows.Close()
	out := []Server{}
	for rows.Next() {
		var s Server
		if err := rows.Scan(&s.Name, &s.BaseURL, &s.SecretName, &s.AuthHeader, &s.AuthScheme); err != nil {
			return nil, agentderr.Internal(err)
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		return nil, agentderr.Internal(err)
	}
	return out, nil
}

func (m *MCP) Delete(ctx context.Context, name string) error {
	tag, err := m.pool.Exec(ctx, `DELETE FROM mcp_servers WHERE name = $1`, name)
	if err != nil {
		return agentderr.Internal(err)
	}
	if tag.RowsAffected() == 0 {
		return agentderr.NotFound("mcp server "+name+" not found", "list at GET /v1/mcp/servers")
	}
	return nil
}

// Call is the credential injection: look up the server, pull the secret
// from the vault, and make the request OUR side. The credential exists
// in memory for the duration of one outbound request and nowhere else.
func (m *MCP) Call(ctx context.Context, serverName, method, path string, body json.RawMessage) (int, string, error) {
	var s Server
	err := m.pool.QueryRow(ctx,
		`SELECT name, base_url, secret_name, auth_header, auth_scheme FROM mcp_servers WHERE name = $1`,
		serverName).Scan(&s.Name, &s.BaseURL, &s.SecretName, &s.AuthHeader, &s.AuthScheme)
	if err == pgx.ErrNoRows {
		return 0, "", agentderr.NotFound(
			"mcp server "+serverName+" not registered",
			"list registered servers at GET /v1/mcp/servers")
	}
	if err != nil {
		return 0, "", agentderr.Internal(err)
	}

	secret, err := m.vault.GetSecret(ctx, s.SecretName)
	if err != nil {
		return 0, "", err
	}

	fullURL := strings.TrimRight(s.BaseURL, "/") + "/" + strings.TrimLeft(path, "/")
	var rdr io.Reader
	if len(body) > 0 {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, fullURL, rdr)
	if err != nil {
		return 0, "", agentderr.Internal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(s.AuthHeader, s.AuthScheme+" "+secret)

	resp, err := m.client.Do(req)
	if err != nil {
		return 0, "", agentderr.Wrap(agentderr.CodeConflict, err,
			"mcp server "+serverName+" unreachable",
			"check the base_url and that the server is up; the call was not made")
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, MaxResponseBytes+1))
	if err != nil {
		return 0, "", agentderr.Internal(err)
	}
	truncated := ""
	if len(raw) > MaxResponseBytes {
		raw = raw[:MaxResponseBytes]
		truncated = "\n...[response truncated]"
	}
	return resp.StatusCode, string(raw) + truncated, nil
}

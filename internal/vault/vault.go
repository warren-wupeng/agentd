// Package vault is where secrets live (G2: secrets only in
// internal/vault). Values are encrypted with AES-256-GCM under a master
// key from the environment; the database holds ciphertext only. A
// decrypted value exists in exactly two places, transiently: inside
// this package, and inside an outbound request this process makes.
// Values are never echoed by the API, never written to the event log,
// never placed in a sandbox.
package vault

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"

	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/warren-wupeng/agentd/internal/agentderr"
)

// Vault stores encrypted secrets and mints derived session tokens.
// It owns its own connection pool — connection-level separation from
// the event store.
type Vault struct {
	pool *pgxpool.Pool
	aead cipher.AEAD
	key  []byte
}

// NewMasterKey decodes VAULT_MASTER_KEY (32 bytes, hex or base64).
func NewMasterKey(raw string) ([]byte, error) {
	if raw == "" {
		return nil, agentderr.InvalidInput(
			"VAULT_MASTER_KEY is not set",
			"generate one: openssl rand -hex 32")
	}
	if b, err := hex.DecodeString(raw); err == nil && len(b) == 32 {
		return b, nil
	}
	if b, err := base64.StdEncoding.DecodeString(raw); err == nil && len(b) == 32 {
		return b, nil
	}
	return nil, agentderr.InvalidInput(
		"VAULT_MASTER_KEY must be 32 bytes as hex or base64",
		"generate one: openssl rand -hex 32")
}

func New(ctx context.Context, databaseURL string, masterKey []byte) (*Vault, error) {
	if len(masterKey) != 32 {
		return nil, agentderr.InvalidInput(
			"vault master key must be 32 bytes",
			"generate one: openssl rand -hex 32")
	}
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		return nil, agentderr.Internal(err)
	}
	block, err := aes.NewCipher(masterKey)
	if err != nil {
		return nil, agentderr.Internal(err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, agentderr.Internal(err)
	}
	return &Vault{pool: pool, aead: aead, key: masterKey}, nil
}

func (v *Vault) Close() error { v.pool.Close(); return nil }

// PutSecret stores (or replaces) a secret. Values are write-only: there
// is deliberately no read-path API outside of Inject-caller use.
func (v *Vault) PutSecret(ctx context.Context, name, value string) error {
	if name == "" || len(value) == 0 {
		return agentderr.InvalidInput("secret name and value are required",
			`example: {"name": "github", "value": "ghp_..."}`)
	}
	nonce := make([]byte, v.aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return agentderr.Internal(err)
	}
	ct := v.aead.Seal(nil, nonce, []byte(value), []byte("agentd-vault:"+name))

	_, err := v.pool.Exec(ctx, `
		INSERT INTO vault_secrets (name, ciphertext, nonce)
		VALUES ($1, $2, $3)
		ON CONFLICT (name) DO UPDATE
		  SET ciphertext = EXCLUDED.ciphertext, nonce = EXCLUDED.nonce, updated_at = now()`,
		name, ct, nonce)
	if err != nil {
		return agentderr.Internal(err)
	}
	return nil
}

// GetSecret decrypts one value. Callable only from within the process
// (not exposed over the API) — used by the MCP proxy to inject
// credentials into outbound requests.
func (v *Vault) GetSecret(ctx context.Context, name string) (string, error) {
	var ct, nonce []byte
	err := v.pool.QueryRow(ctx,
		`SELECT ciphertext, nonce FROM vault_secrets WHERE name = $1`, name).
		Scan(&ct, &nonce)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", agentderr.NotFound(
			"secret "+name+" not found",
			"store it via PUT /v1/vault/secrets first")
	}
	if err != nil {
		return "", agentderr.Internal(err)
	}
	plain, err := v.aead.Open(nil, nonce, ct, []byte("agentd-vault:"+name))
	if err != nil {
		return "", agentderr.Internal(fmt.Errorf("decrypt secret %q (wrong master key?)", name))
	}
	return string(plain), nil
}

// DeleteSecret removes a secret.
func (v *Vault) DeleteSecret(ctx context.Context, name string) error {
	tag, err := v.pool.Exec(ctx, `DELETE FROM vault_secrets WHERE name = $1`, name)
	if err != nil {
		return agentderr.Internal(err)
	}
	if tag.RowsAffected() == 0 {
		return agentderr.NotFound("secret "+name+" not found", "list names at GET /v1/vault/secrets")
	}
	return nil
}

// SecretMeta is what the outside world may see: names and timestamps.
// Never values.
type SecretMeta struct {
	Name      string `json:"name"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}

func (v *Vault) ListSecrets(ctx context.Context) ([]SecretMeta, error) {
	rows, err := v.pool.Query(ctx, `SELECT name, created_at, updated_at FROM vault_secrets ORDER BY name`)
	if err != nil {
		return nil, agentderr.Internal(err)
	}
	defer rows.Close()
	out := []SecretMeta{}
	for rows.Next() {
		var m SecretMeta
		var created, updated time.Time
		if err := rows.Scan(&m.Name, &created, &updated); err != nil {
			return nil, agentderr.Internal(err)
		}
		m.CreatedAt = created.Format(time.RFC3339)
		m.UpdatedAt = updated.Format(time.RFC3339)
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, agentderr.Internal(err)
	}
	return out, nil
}

// SessionToken derives the proxy token for a session: HMAC(master,
// "agentd-session:"+sessionID). No storage — verification recomputes;
// rotation of the master key or terminating the session kills it.
func (v *Vault) SessionToken(sessionID string) string {
	mac := hmac.New(sha256.New, v.key)
	mac.Write([]byte("agentd-session:" + sessionID))
	return hex.EncodeToString(mac.Sum(nil))
}

// VerifySessionToken is constant-time.
func (v *Vault) VerifySessionToken(sessionID, token string) bool {
	want := v.SessionToken(sessionID)
	return subtle.ConstantTimeCompare([]byte(want), []byte(strings.TrimSpace(token))) == 1
}

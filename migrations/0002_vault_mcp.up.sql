-- M6: vault + MCP credential registry (ADR README moat: credential
-- injection). Secrets are AES-GCM ciphertext only — the master key
-- lives in the environment and never touches this database.
CREATE TABLE vault_secrets (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    name        text NOT NULL UNIQUE,
    ciphertext  bytea NOT NULL,
    nonce       bytea NOT NULL,
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now()
);

-- MCP server registry: which upstream, which vault secret to inject,
-- and how. auth_header default covers the overwhelmingly common case.
CREATE TABLE mcp_servers (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    name        text NOT NULL UNIQUE,
    base_url    text NOT NULL,
    secret_name text NOT NULL,              -- vault reference, NOT a value
    auth_header text NOT NULL DEFAULT 'Authorization',
    auth_scheme text NOT NULL DEFAULT 'Bearer', -- value = scheme + ' ' + secret
    created_at  timestamptz NOT NULL DEFAULT now()
);

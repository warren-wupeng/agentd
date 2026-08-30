-- 0001_init: agents, agent_versions, sessions, events (ADR-003, ADR-004)

CREATE TABLE agents (
  id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  name        text NOT NULL UNIQUE,
  description text NOT NULL DEFAULT '',
  created_at  timestamptz NOT NULL DEFAULT now(),
  updated_at  timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE agent_versions (
  id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  agent_id    uuid NOT NULL REFERENCES agents(id) ON DELETE CASCADE,
  version     integer NOT NULL,
  config      jsonb NOT NULL,
  created_at  timestamptz NOT NULL DEFAULT now(),
  UNIQUE (agent_id, version),
  CHECK (version > 0)
);

-- Versions are immutable snapshots: updates create a new row, never mutate
-- an old one. Enforced at the database, not by convention (G1 family).
CREATE OR REPLACE FUNCTION reject_agent_version_mutation() RETURNS trigger AS $$
BEGIN
  RAISE EXCEPTION 'agent_versions are immutable; create a new version instead';
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER agent_versions_immutable
  BEFORE UPDATE ON agent_versions
  FOR EACH ROW EXECUTE FUNCTION reject_agent_version_mutation();

CREATE TABLE sessions (
  id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  agent_id      uuid NOT NULL REFERENCES agents(id),
  agent_version integer NOT NULL,
  harness       text NOT NULL DEFAULT 'native',
  state         text NOT NULL DEFAULT 'rescheduling'
                CHECK (state IN ('rescheduling', 'running', 'idle', 'terminated')),
  stop_reason   text CHECK (stop_reason IN ('requires_action', 'end_turn', 'retries_exhausted')),
  created_at    timestamptz NOT NULL DEFAULT now(),
  updated_at    timestamptz NOT NULL DEFAULT now(),
  FOREIGN KEY (agent_id, agent_version)
    REFERENCES agent_versions (agent_id, version)
);

CREATE TABLE events (
  id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  session_id   uuid NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
  seq          bigserial,
  type         text NOT NULL,
  actor        text NOT NULL CHECK (actor IN ('user', 'agent', 'system')),
  payload      jsonb NOT NULL,
  processed_at timestamptz,            -- null = queued, set = consumed (ADR-003)
  created_at   timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX events_session_seq_idx ON events (session_id, seq);
CREATE INDEX sessions_agent_idx ON sessions (agent_id);

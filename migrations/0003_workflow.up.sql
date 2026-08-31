-- M8: workflow run bookkeeping. The substance of every run lives in
-- its nodes' sessions (events, artifacts); this table is the index.
CREATE TABLE workflow_runs (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    name        text NOT NULL,
    status      text NOT NULL DEFAULT 'running'
                CHECK (status IN ('running', 'completed', 'failed')),
    definition  jsonb NOT NULL,
    node_states jsonb NOT NULL DEFAULT '[]',
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now()
);

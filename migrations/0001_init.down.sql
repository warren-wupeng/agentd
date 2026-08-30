DROP TABLE IF EXISTS events;
DROP TABLE IF EXISTS sessions;
DROP TRIGGER IF EXISTS agent_versions_immutable ON agent_versions;
DROP TABLE IF EXISTS agent_versions;
DROP FUNCTION IF EXISTS reject_agent_version_mutation;
DROP TABLE IF EXISTS agents;

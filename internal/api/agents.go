package api

import (
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/warren-wupeng/agentd/internal/agentderr"
)

// agentConfig is the versioned config snapshot. M1 validates only what the
// control plane itself depends on; the schema is owned by agent authors and
// will grow with M2 (tools) and M5 (mcp_servers).
type agentConfig struct {
	Model        string   `json:"model"`
	SystemPrompt string   `json:"system_prompt,omitempty"`
	Tools        []string `json:"tools,omitempty"`
	MCPServers   []string `json:"mcp_servers,omitempty"`
	Skills       []string `json:"skills,omitempty"`
}

type createAgentReq struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Config      json.RawMessage `json:"config"`
}

func (h *handler) createAgent(w http.ResponseWriter, r *http.Request) {
	var req createAgentReq
	if err := decode(r, &req); err != nil {
		writeErr(w, err)
		return
	}
	if err := validateAgentBasics(req.Name, req.Config); err != nil {
		writeErr(w, err)
		return
	}
	a, v, err := h.st.CreateAgent(r.Context(), req.Name, req.Description, req.Config)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"agent": a, "version": v})
}

func (h *handler) listAgents(w http.ResponseWriter, r *http.Request) {
	limit := clampLimit(r.URL.Query().Get("limit"))
	agents, err := h.st.ListAgents(r.Context(), limit)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"agents": agents})
}

func (h *handler) getAgent(w http.ResponseWriter, r *http.Request) {
	id, err := pathUUID(r, "id")
	if err != nil {
		writeErr(w, err)
		return
	}
	a, err := h.st.GetAgent(r.Context(), id)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"agent": a})
}

type updateAgentReq struct {
	Config json.RawMessage `json:"config"`
}

// updateAgent never mutates: it appends the next immutable version.
func (h *handler) updateAgent(w http.ResponseWriter, r *http.Request) {
	id, err := pathUUID(r, "id")
	if err != nil {
		writeErr(w, err)
		return
	}
	var req updateAgentReq
	if err := decode(r, &req); err != nil {
		writeErr(w, err)
		return
	}
	if err := validateConfig(req.Config); err != nil {
		writeErr(w, err)
		return
	}
	v, err := h.st.CreateAgentVersion(r.Context(), id, req.Config)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"version": v})
}

func (h *handler) deleteAgent(w http.ResponseWriter, r *http.Request) {
	id, err := pathUUID(r, "id")
	if err != nil {
		writeErr(w, err)
		return
	}
	if err := h.st.DeleteAgent(r.Context(), id); err != nil {
		writeErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *handler) listAgentVersions(w http.ResponseWriter, r *http.Request) {
	id, err := pathUUID(r, "id")
	if err != nil {
		writeErr(w, err)
		return
	}
	vs, err := h.st.ListAgentVersions(r.Context(), id)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"versions": vs})
}

func (h *handler) getAgentVersion(w http.ResponseWriter, r *http.Request) {
	id, err := pathUUID(r, "id")
	if err != nil {
		writeErr(w, err)
		return
	}
	v, err := strconv.Atoi(r.PathValue("v"))
	if err != nil || v < 1 {
		writeErr(w, agentderr.InvalidInput(
			"path parameter {v} must be a positive integer, got "+r.PathValue("v"),
			"use the version number from GET /v1/agents/{id}/versions"))
		return
	}
	ver, err := h.st.GetAgentVersion(r.Context(), id, v)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"version": ver})
}

func validateAgentBasics(name string, config json.RawMessage) error {
	if len(name) < 1 || len(name) > 64 {
		return agentderr.InvalidInput("name must be 1-64 characters",
			"use a short lowercase identifier, e.g. \"code-reviewer\"")
	}
	return validateConfig(config)
}

func validateConfig(raw json.RawMessage) error {
	if len(raw) == 0 {
		return agentderr.InvalidInput("config is required",
			`minimal config: {"model": "claude-sonnet-4-6"}`)
	}
	var cfg agentConfig
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return agentderr.Wrap(agentderr.CodeInvalidInput, err,
			"config must be a JSON object",
			`minimal config: {"model": "claude-sonnet-4-6"}`)
	}
	if cfg.Model == "" {
		return agentderr.InvalidInput("config.model is required",
			`set the model identifier, e.g. {"model": "claude-sonnet-4-6"}`)
	}
	return nil
}

func clampLimit(raw string) int {
	n, err := strconv.Atoi(raw)
	if err != nil || n < 1 {
		return 100
	}
	if n > 1000 {
		return 1000
	}
	return n
}

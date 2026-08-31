package api

import (
	"encoding/json"
	"io"
	"net/http"

	"github.com/warren-wupeng/agentd/internal/agentderr"
	"github.com/warren-wupeng/agentd/internal/mcp"
	"github.com/warren-wupeng/agentd/internal/store"
)

// --- vault: values are write-only. Nothing here ever echoes one. ---

type putSecretReq struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

func (h *handler) putSecret(w http.ResponseWriter, r *http.Request) {
	var req putSecretReq
	if err := decode(r, &req); err != nil {
		writeErr(w, err)
		return
	}
	if err := h.vault.PutSecret(r.Context(), req.Name, req.Value); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"name": req.Name,
		"note": "stored; values are write-only and never echoed",
	})
}

func (h *handler) listSecrets(w http.ResponseWriter, r *http.Request) {
	metas, err := h.vault.ListSecrets(r.Context())
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"secrets": metas})
}

func (h *handler) deleteSecret(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if name == "" {
		writeErr(w, agentderr.InvalidInput("secret name is required", "path: /v1/vault/secrets/{name}"))
		return
	}
	if err := h.vault.DeleteSecret(r.Context(), name); err != nil {
		writeErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// --- MCP server registry ---

func (h *handler) registerMCPServer(w http.ResponseWriter, r *http.Request) {
	var s mcp.Server
	if err := decode(r, &s); err != nil {
		writeErr(w, err)
		return
	}
	if err := h.mcp.Register(r.Context(), s); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"server": s})
}

func (h *handler) listMCPServers(w http.ResponseWriter, r *http.Request) {
	servers, err := h.mcp.List(r.Context())
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"servers": servers})
}

// --- the session proxy: external harness workers call MCP through
// agentd with a derived session token; credentials stay control-plane. ---

type mcpProxyReq struct {
	Method string          `json:"method"`
	Path   string          `json:"path"`
	Body   json.RawMessage `json:"body"`
}

func (h *handler) mcpProxy(w http.ResponseWriter, r *http.Request) {
	sessionID, err := pathUUID(r, "id")
	if err != nil {
		writeErr(w, err)
		return
	}
	server := r.PathValue("server")

	token := r.Header.Get("X-Session-Token")
	if token == "" || !h.vault.VerifySessionToken(sessionID.String(), token) {
		writeErr(w, agentderr.New(agentderr.CodeConflict,
			"missing or invalid X-Session-Token",
			"the token is derived per session and available to the session's harness; a terminated or wrong-session token is rejected"))
		return
	}
	sess, err := h.st.GetSession(r.Context(), sessionID)
	if err != nil {
		writeErr(w, err)
		return
	}
	if sess.State == store.StateTerminated {
		writeErr(w, agentderr.Conflict(
			"session is terminated; its proxy token is dead",
			"create a new session via POST /v1/sessions"))
		return
	}

	var req mcpProxyReq
	if err := decode(r, &req); err != nil {
		writeErr(w, err)
		return
	}
	if req.Method == "" {
		req.Method = http.MethodPost
	}

	status, resp, err := h.mcp.Call(r.Context(), server, req.Method, req.Path, req.Body)
	if err != nil {
		writeErr(w, err)
		return
	}
	// Stream the upstream body through unmodified (already capped).
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, resp)
}

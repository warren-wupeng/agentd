// Package api is the HTTP transport layer. It owns routing, JSON codecs,
// and the agentderr → HTTP status mapping. All state lives in store.
package api

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/warren-wupeng/agentd/internal/agentderr"
	"github.com/warren-wupeng/agentd/internal/store"
)

// NewHandler builds the full route table (Go 1.22 method+pattern mux).
func NewHandler(st *store.Store) http.Handler {
	h := &handler{st: st}
	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", h.healthz)

	mux.HandleFunc("POST /v1/agents", h.createAgent)
	mux.HandleFunc("GET /v1/agents", h.listAgents)
	mux.HandleFunc("GET /v1/agents/{id}", h.getAgent)
	mux.HandleFunc("PUT /v1/agents/{id}", h.updateAgent)
	mux.HandleFunc("DELETE /v1/agents/{id}", h.deleteAgent)
	mux.HandleFunc("GET /v1/agents/{id}/versions", h.listAgentVersions)
	mux.HandleFunc("GET /v1/agents/{id}/versions/{v}", h.getAgentVersion)

	mux.HandleFunc("POST /v1/sessions", h.createSession)
	mux.HandleFunc("GET /v1/sessions", h.listSessions)
	mux.HandleFunc("GET /v1/sessions/{id}", h.getSession)
	mux.HandleFunc("POST /v1/sessions/{id}/transitions", h.transitionSession)
	mux.HandleFunc("POST /v1/sessions/{id}/events", h.appendEvent)
	mux.HandleFunc("GET /v1/sessions/{id}/events", h.listEvents)
	mux.HandleFunc("POST /v1/sessions/{id}/events/{eventId}/claim", h.claimEvent)

	return requestLogger(mux)
}

type handler struct {
	st *store.Store
}

func (h *handler) healthz(w http.ResponseWriter, r *http.Request) {
	if err := h.st.Ping(r.Context()); err != nil {
		writeErr(w, agentderr.Wrap(agentderr.CodeInternal, err,
			"database unreachable", "check Postgres; try: make dev-up && make migrate"))
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// --- codec helpers ---

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, err error) {
	var ae *agentderr.Error
	if !errors.As(err, &ae) {
		slog.Error("untyped error", "err", err)
		ae = agentderr.Internal(err)
	}
	if ae.Code == agentderr.CodeInternal {
		slog.Error("internal error", "err", err)
	}
	writeJSON(w, agentderr.HTTPStatus(ae.Code), map[string]any{"error": ae})
}

// decode strict-parses a JSON body: unknown fields and trailing data are
// errors, with remediation that names the offending field (G5).
func decode(r *http.Request, dst any) error {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return agentderr.Wrap(agentderr.CodeInvalidInput, err,
			"request body is not valid for this endpoint",
			"fix the JSON payload; unknown fields are rejected — check the API shape")
	}
	if dec.More() {
		return agentderr.InvalidInput("request body must contain exactly one JSON value",
			"send a single JSON object")
	}
	return nil
}

func pathUUID(r *http.Request, name string) (uuid.UUID, error) {
	raw := r.PathValue(name)
	id, err := uuid.Parse(raw)
	if err != nil {
		return uuid.Nil, agentderr.InvalidInput(
			"path parameter {"+name+"} must be a UUID, got "+raw,
			"copy the id from the resource's JSON representation")
	}
	return id, nil
}

// --- logging middleware ---

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

func requestLogger(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		slog.Info("request",
			"method", r.Method, "path", r.URL.Path,
			"status", rec.status, "dur_ms", time.Since(start).Milliseconds())
	})
}

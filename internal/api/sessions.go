package api

import (
	"net/http"

	"github.com/google/uuid"
	"github.com/warren-wupeng/agentd/internal/agentderr"
	"github.com/warren-wupeng/agentd/internal/store"
)

type createSessionReq struct {
	AgentID      uuid.UUID `json:"agent_id"`
	AgentVersion int       `json:"agent_version"` // 0 = latest
	Harness      string    `json:"harness"`
}

func (h *handler) createSession(w http.ResponseWriter, r *http.Request) {
	var req createSessionReq
	if err := decode(r, &req); err != nil {
		writeErr(w, err)
		return
	}
	if req.AgentID == uuid.Nil {
		writeErr(w, agentderr.InvalidInput("agent_id is required",
			"list agents at GET /v1/agents and copy the id"))
		return
	}
	if req.AgentVersion < 0 {
		writeErr(w, agentderr.InvalidInput("agent_version must be >= 0",
			"use 0 to pin the latest version"))
		return
	}
	harness := req.Harness
	if harness == "" {
		harness = "native"
	}

	sess, _, err := h.st.CreateSession(r.Context(), req.AgentID, req.AgentVersion, harness)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"session": sess})
}

func (h *handler) listSessions(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit := clampLimit(q.Get("limit"))

	var state store.SessionState
	if raw := q.Get("state"); raw != "" {
		state = store.SessionState(raw)
		switch state {
		case store.StateRescheduling, store.StateRunning, store.StateIdle, store.StateTerminated:
		default:
			writeErr(w, agentderr.InvalidInput("unknown state filter "+raw,
				"one of: rescheduling, running, idle, terminated"))
			return
		}
	}

	var agentID *uuid.UUID
	if raw := q.Get("agent_id"); raw != "" {
		id, err := uuid.Parse(raw)
		if err != nil {
			writeErr(w, agentderr.InvalidInput("agent_id filter must be a UUID",
				"copy the id from GET /v1/agents"))
			return
		}
		agentID = &id
	}

	sessions, err := h.st.ListSessions(r.Context(), state, agentID, limit)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"sessions": sessions})
}

func (h *handler) getSession(w http.ResponseWriter, r *http.Request) {
	id, err := pathUUID(r, "id")
	if err != nil {
		writeErr(w, err)
		return
	}
	sess, err := h.st.GetSession(r.Context(), id)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"session": sess})
}

type transitionReq struct {
	To         string  `json:"to"`
	StopReason *string `json:"stop_reason"`
}

func (h *handler) transitionSession(w http.ResponseWriter, r *http.Request) {
	id, err := pathUUID(r, "id")
	if err != nil {
		writeErr(w, err)
		return
	}
	var req transitionReq
	if err := decode(r, &req); err != nil {
		writeErr(w, err)
		return
	}
	var sr *store.StopReason
	if req.StopReason != nil {
		v := store.StopReason(*req.StopReason)
		switch v {
		case store.StopRequiresAction, store.StopEndTurn, store.StopRetriesExhausted:
			sr = &v
		default:
			writeErr(w, agentderr.InvalidInput("unknown stop_reason "+*req.StopReason,
				"one of: requires_action, end_turn, retries_exhausted"))
			return
		}
	}

	sess, ev, err := h.st.TransitionSession(r.Context(), id, store.SessionState(req.To), sr)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"session": sess, "event": ev})
}

package api

import (
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/warren-wupeng/agentd/internal/agentderr"
	"github.com/warren-wupeng/agentd/internal/store"
)

type appendEventReq struct {
	Type    string          `json:"type"` // optional; only user-postable types accepted
	Payload json.RawMessage `json:"payload"`
}

// userPostableTypes is what external callers may append. Everything else is
// written by the system (store-internal paths like transitions).
var userPostableTypes = map[string]bool{store.EventMessageUser: true}

func (h *handler) appendEvent(w http.ResponseWriter, r *http.Request) {
	id, err := pathUUID(r, "id")
	if err != nil {
		writeErr(w, err)
		return
	}
	var req appendEventReq
	if err := decode(r, &req); err != nil {
		writeErr(w, err)
		return
	}
	eventType := req.Type
	if eventType == "" {
		eventType = store.EventMessageUser
	}
	if !userPostableTypes[eventType] {
		writeErr(w, agentderr.InvalidInput(
			"event type "+eventType+" is not user-postable",
			"post message.user; system events (session.state_changed, ...) are written via their own endpoints"))
		return
	}
	if len(req.Payload) == 0 || !json.Valid(req.Payload) {
		writeErr(w, agentderr.InvalidInput("payload must be a valid JSON value",
			`example: {"payload": {"content": [{"type": "text", "text": "hello"}]}}`))
		return
	}

	ev, err := h.st.AppendEvent(r.Context(), id, eventType, store.ActorUser, req.Payload)
	if err != nil {
		writeErr(w, err)
		return
	}

	// A user message on a native session wakes the actor (if the loop is
	// wired and the session can still run). Failures here never fail the
	// append — the event is durable; the actor can always be re-kicked
	// via POST /v1/sessions/{id}/run.
	if h.runner != nil && eventType == store.EventMessageUser {
		if sess, err := h.st.GetSession(r.Context(), id); err == nil &&
			sess.State != store.StateTerminated && sess.Harness == "native" {
			h.runner.Kick(id)
		}
	}

	writeJSON(w, http.StatusCreated, map[string]any{"event": ev})
}

// listEvents is the replay endpoint (ADR-003): after_seq cursor, ordered,
// idempotent. next_after_seq is null when the page is the tail.
func (h *handler) listEvents(w http.ResponseWriter, r *http.Request) {
	id, err := pathUUID(r, "id")
	if err != nil {
		writeErr(w, err)
		return
	}
	q := r.URL.Query()
	var afterSeq int64
	if raw := q.Get("after_seq"); raw != "" {
		n, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || n < 0 {
			writeErr(w, agentderr.InvalidInput("after_seq must be a non-negative integer",
				"use the next_after_seq cursor from the previous page"))
			return
		}
		afterSeq = n
	}
	limit := clampLimit(q.Get("limit"))

	events, err := h.st.ListEvents(r.Context(), id, afterSeq, limit)
	if err != nil {
		writeErr(w, err)
		return
	}
	var next *int64
	if len(events) == limit && len(events) > 0 {
		last := events[len(events)-1].Seq
		next = &last
	}
	writeJSON(w, http.StatusOK, map[string]any{"events": events, "next_after_seq": next})
}

func (h *handler) claimEvent(w http.ResponseWriter, r *http.Request) {
	id, err := pathUUID(r, "id")
	if err != nil {
		writeErr(w, err)
		return
	}
	eventID, err := pathUUID(r, "eventId")
	if err != nil {
		writeErr(w, err)
		return
	}
	ev, err := h.st.ClaimEvent(r.Context(), id, eventID)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"event": ev})
}

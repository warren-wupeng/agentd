package api

import (
	"net/http"

	"github.com/warren-wupeng/agentd/internal/agentderr"
)

// runSession resumes a session's actor: recovery after a crash (the turn
// is unfinished in the log), or a manual push for a parked session.
// Auto-kick already covers the common path (posting a message).
func (h *handler) runSession(w http.ResponseWriter, r *http.Request) {
	id, err := pathUUID(r, "id")
	if err != nil {
		writeErr(w, err)
		return
	}
	if h.runner == nil {
		writeErr(w, agentderr.New(agentderr.CodeConflict,
			"the agent loop is not configured in this process",
			"start the server with MODEL_BASE_URL and MODEL_API_KEY set, then retry"))
		return
	}
	sess, err := h.st.GetSession(r.Context(), id)
	if err != nil {
		writeErr(w, err)
		return
	}
	if sess.State == "terminated" {
		writeErr(w, agentderr.Conflict(
			"session is terminated; its event log is immutable",
			"create a new session via POST /v1/sessions"))
		return
	}
	if sess.Harness != "native" {
		writeErr(w, agentderr.Conflict(
			"session harness "+sess.Harness+" has no native actor",
			"external harness adapters land at M4 (see ADR-004)"))
		return
	}

	h.runner.Kick(id)
	// Report the pre-kick state; the actor moves it to running
	// asynchronously. Poll GET /v1/sessions/{id} and its events.
	writeJSON(w, http.StatusAccepted, map[string]any{
		"accepted": true,
		"session":  sess,
		"note":     "actor scheduled; poll GET /v1/sessions/{id} for state and GET /v1/sessions/{id}/events for the log",
	})
}

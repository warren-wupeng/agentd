package api

import (
	"io"
	"net/http"

	"github.com/warren-wupeng/agentd/internal/agentderr"
)

// startWorkflow accepts a definition, starts the run, and returns 202.
func (h *handler) startWorkflow(w http.ResponseWriter, r *http.Request) {
	if h.workflow == nil {
		writeErr(w, agentderr.New(agentderr.CodeConflict,
			"workflows are not configured in this process",
			"start the server with MODEL_BASE_URL and MODEL_API_KEY set, then retry"))
		return
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeErr(w, agentderr.Internal(err))
		return
	}
	run, err := h.workflow.Start(r.Context(), raw)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{
		"run":  run,
		"note": "poll GET /v1/workflows/{id} for node states",
	})
}

func (h *handler) getWorkflow(w http.ResponseWriter, r *http.Request) {
	if h.workflow == nil {
		writeErr(w, agenterr503())
		return
	}
	id, err := pathUUID(r, "id")
	if err != nil {
		writeErr(w, err)
		return
	}
	run, err := h.workflow.Get(r.Context(), id)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"run": run})
}

func agenterr503() error {
	return agentderr.New(agentderr.CodeConflict,
		"workflows are not configured in this process",
		"start the server with MODEL_BASE_URL and MODEL_API_KEY set, then retry")
}

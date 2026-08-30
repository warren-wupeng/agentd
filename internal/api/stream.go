package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/warren-wupeng/agentd/internal/agentderr"
)

// streamSession is the SSE live tail of a session (ADR-003): replay the
// durable log after the cursor, then stream live — new log events (with
// id: <seq>, so SSE's built-in reconnect carries the cursor via
// Last-Event-ID) and ephemeral model deltas (no id, never re-delivered).
func (h *handler) streamSession(w http.ResponseWriter, r *http.Request) {
	id, err := pathUUID(r, "id")
	if err != nil {
		writeErr(w, err)
		return
	}
	if _, err := h.st.GetSession(r.Context(), id); err != nil {
		writeErr(w, err)
		return
	}

	// Cursor: Last-Event-ID wins (native SSE reconnect), else after_seq.
	// Validated before the wiring check — a bad request shape is the
	// client's bug regardless of what this process can do.
	afterSeq := int64(0)
	if raw := r.Header.Get("Last-Event-ID"); raw != "" {
		n, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || n < 0 {
			writeErr(w, agentderr.InvalidInput(
				"Last-Event-ID must be a non-negative event seq, got "+raw,
				"send the last received event id, or omit the header and use ?after_seq"))
			return
		}
		afterSeq = n
	} else if raw := r.URL.Query().Get("after_seq"); raw != "" {
		n, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || n < 0 {
			writeErr(w, agentderr.InvalidInput(
				"after_seq must be a non-negative integer",
				"use the seq of the last event you have, or 0 for the full log"))
			return
		}
		afterSeq = n
	}

	if h.hub == nil || h.listener == nil {
		writeErr(w, agentderr.New(agentderr.CodeConflict,
			"streaming is not configured in this process",
			"start the server with MODEL_BASE_URL and MODEL_API_KEY set, then retry"))
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, agentderr.New(agentderr.CodeInternal,
			"streaming requires a flushing response writer",
			"this is a server wiring bug; report it"))
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	// Subscribe BEFORE replaying so nothing appended in between is missed;
	// duplicates are impossible because the pump always queries strictly
	// after the last seq this connection has written.
	deltas, cancelDeltas := h.hub.Subscribe(id)
	defer cancelDeltas()
	wake, cancelWake := h.listener.Subscribe(id.String())
	defer cancelWake()

	lastSeq, ok := h.replay(r.Context(), w, flusher, id, afterSeq)
	if !ok {
		return
	}

	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-wake:
			lastSeq, ok = h.replay(r.Context(), w, flusher, id, lastSeq)
			if !ok {
				return
			}
		case d := <-deltas:
			if !writeFrame(w, flusher, "delta", 0, d) {
				return
			}
		case <-heartbeat.C:
			// A comment frame keeps proxies from idling the connection out.
			if _, err := fmt.Fprint(w, ": keep-alive\n\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

// replay writes all events after afterSeq as log frames and returns the
// last seq written (and whether the stream is still healthy). Reads are
// paged so a long backlog cannot blow the frame loop.
func (h *handler) replay(ctx context.Context, w http.ResponseWriter, flusher http.Flusher, id uuid.UUID, afterSeq int64) (int64, bool) {
	for {
		events, err := h.st.ListEvents(ctx, id, afterSeq, 500)
		if err != nil {
			// The log is the source of truth but this connection is now
			// blind; drop it and let the client reconnect (replay contract).
			return afterSeq, false
		}
		for _, ev := range events {
			if !writeFrame(w, flusher, "log", ev.Seq, ev) {
				return afterSeq, false
			}
			afterSeq = ev.Seq
		}
		if len(events) < 500 {
			return afterSeq, true
		}
	}
}

// writeFrame emits one SSE frame; id > 0 attaches it (SSE reconnect cursor).
func writeFrame(w http.ResponseWriter, flusher http.Flusher, event string, id int64, data any) bool {
	payload, err := json.Marshal(data)
	if err != nil {
		return false
	}
	var frame string
	if id > 0 {
		frame = fmt.Sprintf("event: %s\nid: %d\ndata: %s\n\n", event, id, payload)
	} else {
		frame = fmt.Sprintf("event: %s\ndata: %s\n\n", event, payload)
	}
	if _, err := fmt.Fprint(w, frame); err != nil {
		return false
	}
	flusher.Flush()
	return true
}

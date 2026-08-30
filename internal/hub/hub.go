// Package hub fans ephemeral model deltas out to connected SSE clients.
// Deltas are never stored, never re-delivered, and may be DROPPED for a
// slow subscriber — the durable message.assistant event that follows is
// the truth (docs/design/agent-loop.md, Streaming; ADR-003).
package hub

import (
	"sync"

	"github.com/google/uuid"
	"github.com/warren-wupeng/agentd/internal/model"
)

// subscriberBuffer bounds how far a slow client may fall behind before
// deltas start dropping. 256 fragments ≈ a paragraph of tokens.
const subscriberBuffer = 256

// Hub is a process-local fan-out. One per server; the loop publishes,
// SSE handlers subscribe.
type Hub struct {
	mu   sync.Mutex
	subs map[uuid.UUID]map[chan model.Delta]struct{}
}

func New() *Hub {
	return &Hub{subs: map[uuid.UUID]map[chan model.Delta]struct{}{}}
}

// Subscribe returns the session's delta channel and a cancel func.
func (h *Hub) Subscribe(sessionID uuid.UUID) (<-chan model.Delta, func()) {
	ch := make(chan model.Delta, subscriberBuffer)
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.subs[sessionID] == nil {
		h.subs[sessionID] = map[chan model.Delta]struct{}{}
	}
	h.subs[sessionID][ch] = struct{}{}
	return ch, func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		if set := h.subs[sessionID]; set != nil {
			delete(set, ch)
			if len(set) == 0 {
				delete(h.subs, sessionID)
			}
		}
	}
}

// Publish is called by the loop with the streaming goroutine — it never
// blocks: a subscriber whose buffer is full misses fragments by design.
func (h *Hub) Publish(sessionID uuid.UUID, d model.Delta) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.subs[sessionID] {
		select {
		case ch <- d:
		default: // full: drop; the durable event is coming
		}
	}
}

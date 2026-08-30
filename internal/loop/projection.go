// Package loop is the native agent loop: a reentrant state machine over
// the session's event log (docs/design/agent-loop.md). Step advances any
// session by one unit; Runner is the goroutine-per-active-session actor
// that keeps calling Step until the turn parks.
package loop

import (
	"encoding/json"

	"github.com/warren-wupeng/agentd/internal/model"
	"github.com/warren-wupeng/agentd/internal/store"
)

// pendingTool is a tool_use block with no result event yet.
type pendingTool struct {
	MessageSeq int64 // seq of the assistant message carrying it
	Block      model.Block
}

// projection is the session state rebuilt from the log — G3: nothing
// enters a model request that the log cannot reconstruct.
type projection struct {
	messages []model.Message

	// pending tools, in log order (oldest first)
	pending []pendingTool

	// unprocessed user messages, in log order
	unprocessedUser []store.Event

	// assistantCount counts assistant messages since the last
	// turn.completed — the step-cap budget.
	assistantCount int

	// turnOpen is true while a turn is in flight: an assistant message
	// exists after the last turn.completed whose tools have results but
	// no turn.completed has landed yet.
	turnOpen bool
}

type messagePayload struct {
	Content []model.Block `json:"content"`
}

func project(events []store.Event) *projection {
	p := &projection{}

	for _, ev := range events {
		switch ev.Type {
		case store.EventMessageUser:
			var pl messagePayload
			_ = json.Unmarshal(ev.Payload, &pl)
			if len(pl.Content) == 0 {
				pl.Content = []model.Block{model.TextBlock(string(ev.Payload))}
			}
			p.messages = append(p.messages, model.Message{Role: "user", Blocks: pl.Content})
			if ev.ProcessedAt == nil {
				p.unprocessedUser = append(p.unprocessedUser, ev)
			}

		case store.EventMessageAssistant:
			var pl messagePayload
			_ = json.Unmarshal(ev.Payload, &pl)
			p.messages = append(p.messages, model.Message{Role: "assistant", Blocks: pl.Content})
			p.assistantCount++
			p.turnOpen = true
			for _, b := range pl.Content {
				if b.Type == model.BlockToolUse {
					p.pending = append(p.pending, pendingTool{MessageSeq: ev.Seq, Block: b})
				}
			}

		case store.EventToolCompleted, store.EventToolFailed:
			var pl struct {
				ToolUseID string `json:"tool_use_id"`
				Output    string `json:"output"`
				Error     string `json:"error"`
				IsError   bool   `json:"is_error"`
			}
			_ = json.Unmarshal(ev.Payload, &pl)
			content, isErr := pl.Output, pl.IsError
			if ev.Type == store.EventToolFailed {
				content, isErr = pl.Error, true
			}
			p.messages = append(p.messages, model.Message{Role: "user", Blocks: []model.Block{{
				Type:      model.BlockToolResult,
				ToolUseID: pl.ToolUseID,
				Content:   content,
				IsError:   isErr,
			}}})
			// result landed: drop the matching pending entry
			for i, pt := range p.pending {
				if pt.Block.ID == pl.ToolUseID {
					p.pending = append(p.pending[:i], p.pending[i+1:]...)
					break
				}
			}

		case store.EventTurnCompleted:
			p.turnOpen = false
			p.assistantCount = 0

		case store.EventSessionCreated, store.EventSessionStateChanged,
			store.EventToolRequested:
			// no projection effect: audit trail only (tool.requested marks
			// the dispatch attempt; the result events are the dedupe keys)
		}
	}
	return p
}

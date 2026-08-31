package eval

import (
	"context"
	"encoding/json"

	"github.com/google/uuid"
	"github.com/warren-wupeng/agentd/internal/model"
	"github.com/warren-wupeng/agentd/internal/store"
)

// ExportCase mines a case stub from a session's trace (the
// trace → dataset leg, v0): the first user message becomes the input;
// the rubric is left for a human to author. Deeper distillation waits
// for real users' traces.
func ExportCase(ctx context.Context, st *store.Store, sessionID uuid.UUID, caseID string) (*Case, error) {
	events, err := st.ListEvents(ctx, sessionID, 0, 1_000_000)
	if err != nil {
		return nil, err
	}
	sess, err := st.GetSession(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	c := &Case{ID: caseID, Harness: sess.Harness, Rubric: []Criterion{}}
	if c.ID == "" {
		c.ID = "case-" + sessionID.String()[:8]
	}
	for _, ev := range events {
		if ev.Type != store.EventMessageUser {
			continue
		}
		var pl struct {
			Content []model.Block `json:"content"`
		}
		_ = json.Unmarshal(ev.Payload, &pl)
		for _, b := range pl.Content {
			if b.Type == model.BlockText && b.Text != "" {
				c.Input = b.Text
				return c, nil // first user message is the case input
			}
		}
	}
	if c.Input == "" {
		return nil, &noInputError{sessionID.String()}
	}
	return c, nil
}

type noInputError struct{ sessionID string }

func (e *noInputError) Error() string {
	return "session " + e.sessionID + " has no user message to mine"
}

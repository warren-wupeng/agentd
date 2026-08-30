package loop

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/warren-wupeng/agentd/internal/model"
	"github.com/warren-wupeng/agentd/internal/policy"
	"github.com/warren-wupeng/agentd/internal/sandbox"
	"github.com/warren-wupeng/agentd/internal/store"
	"github.com/warren-wupeng/agentd/internal/tools"
)

// Deps is everything the loop needs. Constructed once per process.
type Deps struct {
	Store        *store.Store
	Model        model.Provider
	Sandbox      sandbox.Provider
	Policy       policy.Engine
	Registry     *tools.Registry
	MaxSteps     int           // assistant messages per turn before the cap trips
	ModelRetries int           // model-call attempts before retries_exhausted
	RetryBackoff time.Duration // base backoff between model attempts
	Log          *slog.Logger
}

func (d *Deps) maxSteps() int {
	if d.MaxSteps <= 0 {
		return 40
	}
	return d.MaxSteps
}

// Outcome says what the runner should do after one Step.
type Outcome string

const (
	// OutcomeContinue: the turn advanced but is not done — keep stepping.
	OutcomeContinue Outcome = "continue"
	// OutcomeParked: the turn ended (stop_reason recorded) or errored out.
	OutcomeParked Outcome = "parked"
	// OutcomeNoop: nothing to do — no pending tools, no unprocessed input.
	OutcomeNoop Outcome = "noop"
)

// agentConfig is the pinned agent version config the loop reads.
type agentConfig struct {
	Model        string   `json:"model"`
	SystemPrompt string   `json:"system_prompt"`
	Tools        []string `json:"tools"`
}

// Step advances a session by one unit: dispatch pending tools, or run one
// model call, or park the turn. Safe to run from any process at any time;
// correctness comes from the log, not from Step-local state.
func Step(ctx context.Context, d *Deps, sessionID uuid.UUID) (Outcome, error) {
	sess, err := d.Store.GetSession(ctx, sessionID)
	if err != nil {
		return OutcomeNoop, err
	}
	if sess.State == store.StateTerminated {
		return OutcomeNoop, nil
	}

	events, err := d.Store.ListEvents(ctx, sessionID, 0, 1_000_000)
	if err != nil {
		return OutcomeNoop, err
	}
	p := project(events)

	// Rule 6 first: tool_use blocks without results are dispatched before
	// anything else — the model is waiting on its own tools.
	if len(p.pending) > 0 {
		if err := dispatchTools(ctx, d, sess, p); err != nil {
			return OutcomeParked, err
		}
		return OutcomeContinue, nil
	}

	// Mid-turn user input queues (design rule 2): consumed only at a turn
	// boundary. No open turn + no unprocessed input = nothing to do.
	if p.turnOpen {
		// continue the open turn with a model call
	} else if len(p.unprocessedUser) == 0 {
		return OutcomeNoop, nil
	} else {
		for _, ev := range p.unprocessedUser {
			if _, err := d.Store.ClaimEvent(ctx, sessionID, ev.ID); err != nil {
				return OutcomeParked, err
			}
		}
	}

	// Budget: the step cap is the M2 stand-in for the full budget check
	// (design step 3). No silent truncation — the stop_reason says it.
	if p.assistantCount >= d.maxSteps() {
		if err := finishTurn(ctx, d, sess, store.StopRetriesExhausted,
			fmt.Sprintf("step cap of %d assistant messages reached for this turn", d.maxSteps())); err != nil {
			return OutcomeParked, err
		}
		return OutcomeParked, nil
	}

	cfg, err := loadAgentConfig(ctx, d, sess)
	if err != nil {
		if aerr := finishTurn(ctx, d, sess, store.StopRetriesExhausted, err.Error()); aerr != nil {
			return OutcomeParked, aerr
		}
		return OutcomeParked, nil
	}

	req := &model.CompletionRequest{
		Model:     cfg.Model,
		System:    cfg.SystemPrompt,
		Messages:  p.messages,
		MaxTokens: 4096,
	}
	enabled, err := d.Registry.Enable(cfg.Tools)
	if err != nil {
		if aerr := finishTurn(ctx, d, sess, store.StopRetriesExhausted, err.Error()); aerr != nil {
			return OutcomeParked, aerr
		}
		return OutcomeParked, nil
	}
	for _, t := range enabled {
		req.Tools = append(req.Tools, model.ToolDef{
			Name:        t.Name(),
			Description: t.Description(),
			Parameters:  t.Schema(),
		})
	}

	resp, err := completeWithRetries(ctx, d, req)
	if err != nil {
		if aerr := finishTurn(ctx, d, sess, store.StopRetriesExhausted, err.Error()); aerr != nil {
			return OutcomeParked, aerr
		}
		return OutcomeParked, nil
	}

	// Idempotency rule: the assistant message (with the provider's stable
	// tool_use ids) is persisted BEFORE any of its tools execute.
	_, err = d.Store.AppendEvent(ctx, sessionID, store.EventMessageAssistant, store.ActorAgent,
		mustJSON(map[string]any{
			"content": resp.Blocks,
			"model":   cfg.Model,
			"usage":   resp.Usage,
		}))
	if err != nil {
		return OutcomeParked, err
	}

	hasToolUse := false
	for _, b := range resp.Blocks {
		if b.Type == model.BlockToolUse {
			hasToolUse = true
			break
		}
	}
	if hasToolUse {
		return OutcomeContinue, nil // next Step dispatches the tools
	}
	if err := finishTurn(ctx, d, sess, store.StopEndTurn, ""); err != nil {
		return OutcomeParked, err
	}
	return OutcomeParked, nil
}

// dispatchTools executes pending tool_use blocks in log order. Results
// already in the log never reach here (projection filters them), which is
// the exactly-once-tools rule; a crash between execute and append can
// still re-run a tool on the next Step — the documented at-least-once
// boundary.
func dispatchTools(ctx context.Context, d *Deps, sess *store.Session, p *projection) error {
	handle, hErr := d.Sandbox.Handle(sess.ID)

	for _, pt := range p.pending {
		verdict := d.Policy.Check(pt.Block.Name, pt.Block.Input)
		if _, err := d.Store.AppendEvent(ctx, sess.ID, store.EventToolRequested, store.ActorSystem,
			mustJSON(map[string]any{
				"tool_use_id": pt.Block.ID,
				"name":        pt.Block.Name,
				"input":       pt.Block.Input,
				"verdict":     verdict,
			})); err != nil {
			return err
		}

		if verdict.Decision == policy.Deny {
			// Denials are data (G5): the model reads the reason and adapts.
			if _, err := d.Store.AppendEvent(ctx, sess.ID, store.EventToolCompleted, store.ActorSystem,
				mustJSON(map[string]any{
					"tool_use_id": pt.Block.ID,
					"output":      "denied: " + verdict.Reason,
					"is_error":    true,
				})); err != nil {
				return err
			}
			continue
		}

		if hErr != nil {
			if _, err := d.Store.AppendEvent(ctx, sess.ID, store.EventToolFailed, store.ActorSystem,
				mustJSON(map[string]any{
					"tool_use_id": pt.Block.ID,
					"error":       "sandbox unavailable: " + hErr.Error(),
				})); err != nil {
				return err
			}
			continue
		}

		tool, ok := d.Registry.Get(pt.Block.Name)
		if !ok {
			if _, err := d.Store.AppendEvent(ctx, sess.ID, store.EventToolCompleted, store.ActorSystem,
				mustJSON(map[string]any{
					"tool_use_id": pt.Block.ID,
					"output": fmt.Sprintf("error: unknown tool %q; available tools are in your tool list",
						pt.Block.Name),
					"is_error": true,
				})); err != nil {
				return err
			}
			continue
		}

		out, err := tool.Execute(ctx, handle, pt.Block.Input)
		if err != nil {
			if _, aerr := d.Store.AppendEvent(ctx, sess.ID, store.EventToolFailed, store.ActorSystem,
				mustJSON(map[string]any{
					"tool_use_id": pt.Block.ID,
					"error":       err.Error(),
				})); aerr != nil {
				return aerr
			}
			continue
		}
		if _, err := d.Store.AppendEvent(ctx, sess.ID, store.EventToolCompleted, store.ActorSystem,
			mustJSON(map[string]any{
				"tool_use_id": pt.Block.ID,
				"output":      out,
			})); err != nil {
			return err
		}
	}
	return nil
}

// finishTurn records turn.completed and parks the session at idle with
// the stop_reason — the only place Step ends a turn (G1: the transition
// rides its event).
func finishTurn(ctx context.Context, d *Deps, sess *store.Session, reason store.StopReason, detail string) error {
	payload := map[string]any{"stop_reason": reason}
	if detail != "" {
		payload["detail"] = detail
	}
	if _, err := d.Store.AppendEvent(ctx, sess.ID, store.EventTurnCompleted, store.ActorSystem,
		mustJSON(payload)); err != nil {
		return err
	}
	// Re-read: the state may have moved since Step loaded it (runner
	// transitioned to running, a parallel finishTurn parked it).
	cur, err := d.Store.GetSession(ctx, sess.ID)
	if err != nil {
		return err
	}
	if cur.State == store.StateRunning {
		if _, _, err := d.Store.TransitionSession(ctx, sess.ID, store.StateIdle, &reason); err != nil {
			return err
		}
	}
	return nil
}

func loadAgentConfig(ctx context.Context, d *Deps, sess *store.Session) (*agentConfig, error) {
	v, err := d.Store.GetAgentVersion(ctx, sess.AgentID, sess.AgentVersion)
	if err != nil {
		return nil, err
	}
	var cfg agentConfig
	if err := json.Unmarshal(v.Config, &cfg); err != nil {
		return nil, fmt.Errorf("agent %s version %d config is not valid JSON: %w", sess.AgentID, sess.AgentVersion, err)
	}
	if cfg.Model == "" {
		return nil, fmt.Errorf("agent %s version %d config has no model; set \"model\" in the agent config", sess.AgentID, sess.AgentVersion)
	}
	return &cfg, nil
}

func completeWithRetries(ctx context.Context, d *Deps, req *model.CompletionRequest) (*model.CompletionResponse, error) {
	retries := d.ModelRetries
	if retries <= 0 {
		retries = 3
	}
	backoff := d.RetryBackoff
	if backoff <= 0 {
		backoff = 2 * time.Second
	}
	var lastErr error
	for attempt := 0; attempt < retries; attempt++ {
		resp, err := d.Model.Complete(ctx, req)
		if err == nil {
			return resp, nil
		}
		lastErr = err
		if ctx.Err() != nil {
			break
		}
		wait := time.Duration(1<<uint(attempt)) * backoff // 1x, 2x, 4x...
		if wait > 30*time.Second {
			wait = 30 * time.Second
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(wait):
		}
	}
	return nil, fmt.Errorf("model call failed after %d attempt(s): %w", retries, lastErr)
}

func mustJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		// map[string]any of marshalable leaves; unreachable in practice
		return json.RawMessage(`{}`)
	}
	return b
}

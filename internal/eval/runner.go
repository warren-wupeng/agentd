package eval

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"
	"github.com/warren-wupeng/agentd/internal/harness"
	"github.com/warren-wupeng/agentd/internal/model"
	"github.com/warren-wupeng/agentd/internal/store"
)

// Runner drives a dataset through a harness — the same seam the product
// runs on. Each case is a fresh session pinned to the agent version;
// the trace is projected from the real event log after the turn parks.
type Runner struct {
	Store   *store.Store
	Harness harness.Harness
	Scorer  *Scorer
}

// RunDataset scores every case against one agent version. Errors on
// individual cases are recorded as failed results (with the reason),
// not aborts — a half-run report is more useful than no report.
func (r *Runner) RunDataset(ctx context.Context, d Dataset, agentID uuid.UUID, version int) (*VersionReport, error) {
	report := &VersionReport{AgentID: agentID.String(), Version: version, Dataset: d.Name}
	for _, c := range d.Cases {
		res := r.runCase(ctx, c, agentID, version)
		report.Results = append(report.Results, res)
	}
	// aggregates
	if len(report.Results) > 0 {
		sum, passes := 0.0, 0.0
		for _, res := range report.Results {
			sum += res.Score
			if res.Pass {
				passes++
			}
		}
		report.Score = sum / float64(len(report.Results))
		report.PassRate = passes / float64(len(report.Results))
	}
	return report, nil
}

func (r *Runner) runCase(ctx context.Context, c Case, agentID uuid.UUID, version int) CaseResult {
	fail := func(reason string) CaseResult {
		return CaseResult{CaseID: c.ID, Pass: false, Score: 0, Results: []CriterionResult{
			{Criterion: Criterion{Kind: "runner"}, Reason: "FAIL: " + reason},
		}}
	}

	// a pinned fresh session per case
	cfg, err := r.pinnedConfig(ctx, agentID, version)
	if err != nil {
		return fail(err.Error())
	}
	sess, _, err := r.Store.CreateSession(ctx, agentID, version, c.Harness)
	if err != nil {
		return fail("create session: " + err.Error())
	}

	input, _ := json.Marshal(map[string]any{
		"content": []model.Block{model.TextBlock(c.Input)},
	})
	if _, err := r.Store.AppendEvent(ctx, sess.ID, store.EventMessageUser, store.ActorUser, input); err != nil {
		return fail("post input: " + err.Error())
	}

	spec := harness.WorkerSpec{
		SessionID: sess.ID, AgentID: agentID, AgentVersion: version, Config: cfg,
	}
	handle, err := r.Harness.Launch(ctx, spec)
	if err != nil {
		return fail("launch: " + err.Error())
	}
	if err := r.Harness.Run(ctx, handle); err != nil {
		return fail("run: " + err.Error())
	}

	tr, err := r.traceOf(ctx, sess.ID)
	if err != nil {
		return fail("trace: " + err.Error())
	}
	return r.Scorer.Score(ctx, c, tr)
}

func (r *Runner) pinnedConfig(ctx context.Context, agentID uuid.UUID, version int) (json.RawMessage, error) {
	v, err := r.Store.GetAgentVersion(ctx, agentID, version)
	if err != nil {
		return nil, fmt.Errorf("agent version: %w", err)
	}
	return v.Config, nil
}

// traceOf projects the session's event log into a RunTrace.
func (r *Runner) traceOf(ctx context.Context, sessionID uuid.UUID) (RunTrace, error) {
	events, err := r.Store.ListEvents(ctx, sessionID, 0, 1_000_000)
	if err != nil {
		return RunTrace{}, err
	}
	tr := RunTrace{SessionID: sessionID}
	toolSet := map[string]bool{}
	for _, ev := range events {
		switch ev.Type {
		case store.EventMessageAssistant:
			tr.AssistantMsgs++
			var pl struct {
				Content []model.Block `json:"content"`
			}
			_ = json.Unmarshal(ev.Payload, &pl)
			for _, b := range pl.Content {
				if b.Type == model.BlockText && b.Text != "" {
					tr.FinalText = b.Text // last text block wins
				}
			}
		case store.EventToolRequested:
			var pl struct {
				Name string `json:"name"`
			}
			_ = json.Unmarshal(ev.Payload, &pl)
			if pl.Name != "" {
				toolSet[pl.Name] = true
			}
		case store.EventTurnCompleted:
			var pl struct {
				StopReason string `json:"stop_reason"`
			}
			_ = json.Unmarshal(ev.Payload, &pl)
			tr.StopReason = pl.StopReason
		}
	}
	for name := range toolSet {
		tr.ToolsUsed = append(tr.ToolsUsed, name)
	}
	return tr, nil
}

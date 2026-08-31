package eval

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/warren-wupeng/agentd/internal/sandbox"
)

// Scorer evaluates a trace against a rubric. Deterministic only —
// reproducibility in CI is the scorer's whole value; LLM judges are a
// post-M7 extension behind the same Criterion type.
type Scorer struct {
	// Sandbox reads artifacts through the session's handle (M5's file
	// API — works for every provider tier).
	sandbox sandbox.Provider
}

func NewScorer(sb sandbox.Provider) *Scorer { return &Scorer{sandbox: sb} }

// Score checks every criterion of the case against the trace.
func (s *Scorer) Score(ctx context.Context, c Case, tr RunTrace) CaseResult {
	res := CaseResult{CaseID: c.ID, SessionID: tr.SessionID.String()}
	total, passed := 0.0, 0.0
	for _, crit := range c.Rubric {
		w := crit.Weight
		if w <= 0 {
			w = 1 // same normalization ParseDataset applies; direct constructions too
		}
		cr := s.check(ctx, crit, tr)
		res.Results = append(res.Results, cr)
		total += w
		if cr.Pass {
			passed += w
		}
	}
	if total > 0 {
		res.Score = passed / total
	}
	res.Pass = len(res.Results) > 0 && passed == total
	return res
}

func (s *Scorer) check(ctx context.Context, crit Criterion, tr RunTrace) CriterionResult {
	cr := CriterionResult{Criterion: crit}
	switch crit.Kind {
	case KindContains:
		cr.Pass = strings.Contains(tr.FinalText, crit.Arg)
		cr.Reason = reasonFor(cr.Pass, "final text contains %q", crit.Arg)
	case KindNotContains:
		cr.Pass = !strings.Contains(tr.FinalText, crit.Arg)
		cr.Reason = reasonFor(cr.Pass, "final text does not contain %q", crit.Arg)
	case KindToolUsed:
		cr.Pass = containsStr(tr.ToolsUsed, crit.Arg)
		cr.Reason = reasonFor(cr.Pass, "tool %q was used", crit.Arg)
	case KindToolNotUsed:
		cr.Pass = !containsStr(tr.ToolsUsed, crit.Arg)
		cr.Reason = reasonFor(cr.Pass, "tool %q was not used", crit.Arg)
	case KindStopReason:
		cr.Pass = tr.StopReason == crit.Arg
		cr.Reason = reasonFor(cr.Pass, "stop_reason == %q", crit.Arg)
	case KindMaxTurns:
		max, err := strconv.Atoi(crit.Arg)
		if err != nil || max < 0 {
			cr.Reason = "max_turns arg must be a non-negative integer"
			break
		}
		cr.Pass = tr.AssistantMsgs <= max
		cr.Reason = reasonFor(cr.Pass, "assistant messages (%d) <= %d", tr.AssistantMsgs, max)
	case KindArtifactContains:
		content, err := s.readArtifact(ctx, tr, crit.Path)
		if err != nil {
			cr.Reason = "artifact read failed: " + err.Error()
			break
		}
		cr.Pass = strings.Contains(string(content), crit.Arg)
		cr.Reason = reasonFor(cr.Pass, "artifact %q contains %q", crit.Path, crit.Arg)
	default:
		cr.Reason = "unknown criterion kind " + string(crit.Kind)
	}
	return cr
}

func (s *Scorer) readArtifact(ctx context.Context, tr RunTrace, path string) ([]byte, error) {
	if s.sandbox == nil {
		return nil, fmt.Errorf("no sandbox provider wired for artifact checks")
	}
	h, err := s.sandbox.Handle(tr.SessionID)
	if err != nil {
		return nil, err
	}
	return h.ReadFile(ctx, path)
}

func reasonFor(pass bool, format string, args ...any) string {
	if pass {
		return "PASS: " + fmt.Sprintf(format, args...)
	}
	return "FAIL: " + fmt.Sprintf(format, args...)
}

func containsStr(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

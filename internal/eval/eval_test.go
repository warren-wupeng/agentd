package eval_test

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/warren-wupeng/agentd/internal/eval"
	"github.com/warren-wupeng/agentd/internal/harness"
	"github.com/warren-wupeng/agentd/internal/loop"
	"github.com/warren-wupeng/agentd/internal/model"
	"github.com/warren-wupeng/agentd/internal/policy"
	"github.com/warren-wupeng/agentd/internal/sandbox"
	"github.com/warren-wupeng/agentd/internal/store"
	"github.com/warren-wupeng/agentd/internal/testutil"
	"github.com/warren-wupeng/agentd/internal/tools"
)

var ctx = context.Background()

func TestMain(m *testing.M) { testutil.Main(m) }

// versionedModel dispatches on the system prompt: agents configured
// "be-good" produce a working write_file turn; "be-bad" agents answer
// without doing the work. That makes ONE dataset behave differently
// across agent versions — exactly what the compare report must show.
type versionedModel struct{}

func (versionedModel) Complete(_ context.Context, req *model.CompletionRequest) (*model.CompletionResponse, error) {
	if strings.Contains(req.System, "be-good") {
		return &model.CompletionResponse{
			Blocks: []model.Block{
				{Type: model.BlockToolUse, ID: "tu1", Name: "write_file",
					Input: json.RawMessage(`{"path":"note.txt","content":"the answer is 42"}`)},
			},
			FinishReason: model.FinishToolCalls,
		}, nil
	}
	return &model.CompletionResponse{
		Blocks:       []model.Block{model.TextBlock("no answer, ask someone else")},
		FinishReason: model.FinishStop,
	}, nil
}

// followUpModel: first call (be-good) returns the write_file tool_use;
// the call AFTER a tool result wraps up with text.
type followUpModel struct{}

func (followUpModel) Complete(_ context.Context, req *model.CompletionRequest) (*model.CompletionResponse, error) {
	lastIsToolResult := false
	if n := len(req.Messages); n > 0 {
		for _, b := range req.Messages[n-1].Blocks {
			if b.Type == model.BlockToolResult {
				lastIsToolResult = true
			}
		}
	}
	if lastIsToolResult {
		return &model.CompletionResponse{
			Blocks:       []model.Block{model.TextBlock("done: the answer is 42")},
			FinishReason: model.FinishStop,
		}, nil
	}
	return versionedModel{}.Complete(context.Background(), req)
}

const datasetJSON = `{
  "name": "answer-quality",
  "cases": [
    {"id": "writes-note", "input": "write the answer to note.txt",
     "rubric": [
       {"kind": "contains", "arg": "42"},
       {"kind": "tool_used", "arg": "write_file"},
       {"kind": "artifact_contains", "path": "note.txt", "arg": "42"},
       {"kind": "stop_reason", "arg": "end_turn"}
     ]},
    {"id": "no-errors", "input": "answer cleanly",
     "rubric": [
       {"kind": "not_contains", "arg": "error"},
       {"kind": "max_turns", "arg": "3"}
     ]},
    {"id": "no-shell", "input": "answer without a shell",
     "rubric": [{"kind": "tool_not_used", "arg": "bash"}]}
  ]
}`

// THE M7 done-when: two agent versions scored on the same dataset,
// diff printed — asserted, not eyeballed.
func TestDoneWhen_TwoVersionsSameDatasetDiffPrinted(t *testing.T) {
	st := testutil.NewStore(t)

	// v1: the lazy agent; v2: the fixed agent (same agent family)
	a, _, err := st.CreateAgent(ctx, "answerer", "", json.RawMessage(`{"model":"fake","system_prompt":"be-bad","tools":["bash","write_file"]}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateAgentVersion(ctx, a.ID, json.RawMessage(`{"model":"fake","system_prompt":"be-good","tools":["write_file"]}`)); err != nil {
		t.Fatal(err)
	}

	sb, err := sandbox.NewExec(filepath.Join(t.TempDir(), "sb"))
	if err != nil {
		t.Fatal(err)
	}
	ndeps := &loop.Deps{
		Store: st, Model: followUpModel{}, Sandbox: sb,
		Policy:       policy.NewStatic(),
		Registry:     tools.NewRegistry(),
		ModelRetries: 2, RetryBackoff: time.Millisecond,
	}
	runner := &eval.Runner{
		Store:   st,
		Harness: harness.NewNative(ndeps),
		Scorer:  eval.NewScorer(sb),
	}

	d, err := eval.ParseDataset([]byte(datasetJSON))
	if err != nil {
		t.Fatal(err)
	}

	v1, err := runner.RunDataset(ctx, *d, a.ID, 1)
	if err != nil {
		t.Fatal(err)
	}
	v2, err := runner.RunDataset(ctx, *d, a.ID, 2)
	if err != nil {
		t.Fatal(err)
	}

	// sanity: v1 fails the writes-note case, v2 passes everything
	byID := func(r *eval.VersionReport, id string) eval.CaseResult {
		for _, res := range r.Results {
			if res.CaseID == id {
				return res
			}
		}
		t.Fatalf("case %s missing from report", id)
		return eval.CaseResult{}
	}
	if byID(v1, "writes-note").Pass {
		t.Fatal("v1 (be-bad) unexpectedly passed writes-note")
	}
	if !byID(v2, "writes-note").Pass {
		t.Fatalf("v2 (be-good) failed writes-note: %+v", byID(v2, "writes-note"))
	}
	if v2.Score <= v1.Score {
		t.Fatalf("scores: v2 (%.2f) must beat v1 (%.2f)", v2.Score, v1.Score)
	}

	// the printed diff
	out := eval.Compare(v1, v2)
	for _, want := range []string{
		"dataset: answer-quality",
		"writes-note", "FAIL", "PASS",
		"IMPROVEMENT",
		"now passing",
		"aggregate",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("diff output missing %q:\n%s", want, out)
		}
	}
	// the flip line names the first criterion that changed
	if !strings.Contains(out, `contains`+`"42"`) && !strings.Contains(out, `contains "42"`) {
		t.Fatalf("flip line should name the flipped criterion:\n%s", out)
	}
	t.Logf("compare report:\n%s", out)
}

// trace → dataset: a session's first user message becomes a case stub.
func TestExportCaseMinesTrace(t *testing.T) {
	st := testutil.NewStore(t)
	a, _, err := st.CreateAgent(ctx, "mined", "", json.RawMessage(`{"model":"fake"}`))
	if err != nil {
		t.Fatal(err)
	}
	sess, _, err := st.CreateSession(ctx, a.ID, 0, "native")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.AppendEvent(ctx, sess.ID, store.EventMessageUser, store.ActorUser,
		json.RawMessage(`{"content":[{"type":"text","text":"deploy the service"}]}`)); err != nil {
		t.Fatal(err)
	}

	c, err := eval.ExportCase(ctx, st, sess.ID, "deploy-case")
	if err != nil {
		t.Fatal(err)
	}
	if c.Input != "deploy the service" || c.ID != "deploy-case" || c.Harness != "native" || len(c.Rubric) != 0 {
		t.Fatalf("mined case: %+v", c)
	}

	// no user message → explicit error
	sess2, _, _ := st.CreateSession(ctx, a.ID, 0, "native")
	if _, err := eval.ExportCase(ctx, st, sess2.ID, ""); err == nil {
		t.Fatal("empty session must not produce a case")
	}
}

func TestParseDatasetValidation(t *testing.T) {
	if _, err := eval.ParseDataset([]byte(`{"cases":[{"id":"x"},{"id":"x"}]}`)); err == nil {
		t.Fatal("duplicate case ids must be rejected")
	}
	d, err := eval.ParseDataset([]byte(`{"name":"d","cases":[{"id":"a"},{"id":"b","harness":"opencode"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if d.Cases[0].Harness != "native" || d.Cases[1].Harness != "opencode" {
		t.Fatalf("harness defaults: %+v", d.Cases)
	}
}

func TestScorerCriterionKinds(t *testing.T) {
	s := eval.NewScorer(nil)
	tr := eval.RunTrace{
		FinalText:     "the answer is 42",
		ToolsUsed:     []string{"write_file", "bash"},
		StopReason:    "end_turn",
		AssistantMsgs: 2,
	}
	c := eval.Case{ID: "x", Input: "i", Rubric: []eval.Criterion{
		{Kind: eval.KindContains, Arg: "42"},
		{Kind: eval.KindNotContains, Arg: "boom"},
		{Kind: eval.KindToolUsed, Arg: "bash"},
		{Kind: eval.KindToolNotUsed, Arg: "mcp"},
		{Kind: eval.KindStopReason, Arg: "end_turn"},
		{Kind: eval.KindMaxTurns, Arg: "2"},
		{Kind: eval.KindContains, Arg: "missing", Weight: 0}, // weight normalizes to 1
	}}
	res := s.Score(ctx, c, tr)
	if res.Pass {
		t.Fatalf("one criterion fails by design: %+v", res.Results)
	}
	if res.Score < 0.85 || res.Score > 0.86 {
		t.Fatalf("score = %.4f, want 6/7 ≈ 0.857", res.Score)
	}
	fails := 0
	for _, r := range res.Results {
		if !r.Pass {
			fails++
			if !strings.HasPrefix(r.Reason, "FAIL: ") {
				t.Fatalf("failing reason not prefixed: %q", r.Reason)
			}
		}
	}
	if fails != 1 {
		t.Fatalf("fails = %d, want 1", fails)
	}

	// unknown kind → failing result with a reason, never a panic
	res = s.Score(ctx, eval.Case{ID: "y", Rubric: []eval.Criterion{{Kind: "nonsense"}}}, tr)
	if res.Results[0].Pass || res.Results[0].Reason == "" {
		t.Fatalf("unknown kind: %+v", res.Results[0])
	}

	// artifact check without a sandbox → clear failure, not a crash
	res = s.Score(ctx, eval.Case{ID: "z", Rubric: []eval.Criterion{{Kind: eval.KindArtifactContains, Path: "a", Arg: "b"}}}, tr)
	if res.Results[0].Pass || !strings.Contains(res.Results[0].Reason, "no sandbox provider") {
		t.Fatalf("artifact without provider: %+v", res.Results[0])
	}
}

var _ = fmt.Sprint

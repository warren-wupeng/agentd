package workflow_test

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/warren-wupeng/agentd/internal/harness"
	"github.com/warren-wupeng/agentd/internal/loop"
	"github.com/warren-wupeng/agentd/internal/model"
	"github.com/warren-wupeng/agentd/internal/policy"
	"github.com/warren-wupeng/agentd/internal/sandbox"
	"github.com/warren-wupeng/agentd/internal/store"
	"github.com/warren-wupeng/agentd/internal/testutil"
	"github.com/warren-wupeng/agentd/internal/tools"
	"github.com/warren-wupeng/agentd/internal/workflow"
)

var ctx = context.Background()

func TestMain(m *testing.M) { testutil.Main(m) }

// pipelineModel plays every role of the software-dev template,
// dispatching on the prompt — and SLEEPS in the review/test roles so
// the parallel-fan-out proof has a real overlap window.
type pipelineModel struct{}

func (pipelineModel) Complete(_ context.Context, req *model.CompletionRequest) (*model.CompletionResponse, error) {
	prompt := ""
	for _, m := range req.Messages {
		for _, b := range m.Blocks {
			if b.Type == model.BlockText {
				prompt += b.Text
			}
		}
	}
	lastIsToolResult := false
	if n := len(req.Messages); n > 0 {
		for _, b := range req.Messages[n-1].Blocks {
			if b.Type == model.BlockToolResult {
				lastIsToolResult = true
			}
		}
	}

	switch {
	case strings.Contains(prompt, "You are the coder") && !lastIsToolResult:
		return toolUse("tu-coder", "write_file",
			`{"path":"solution.py","content":"def fib(n):\n    a, b = 0, 1\n    for _ in range(n):\n        a, b = b, a + b\n    return a\n"}`), nil
	case strings.Contains(prompt, "You are the coder") && lastIsToolResult:
		return text("coder summary: iterative fibonacci, O(n) time O(1) space"), nil
	case strings.Contains(prompt, "You are the code reviewer"):
		time.Sleep(400 * time.Millisecond) // widen the overlap window
		return text("REVIEW: rename nothing; add a docstring. Verdict: good code."), nil
	case strings.Contains(prompt, "You are the test engineer"):
		time.Sleep(400 * time.Millisecond)
		return text("TEST VERDICT: PASS — fib(10)==55 covered by plan"), nil
	case strings.Contains(prompt, "You are the merger") && !lastIsToolResult:
		return toolUse("tu-merger", "write_file",
			`{"path":"MERGED.md","content":"merged artifact"}`), nil
	case strings.Contains(prompt, "You are the merger") && lastIsToolResult:
		return text("merged: review incorporated, tests green"), nil
	}
	return nil, fmt.Errorf("pipelineModel: unrecognized prompt %.80s", prompt)
}

func toolUse(id, name, input string) *model.CompletionResponse {
	return &model.CompletionResponse{
		Blocks:       []model.Block{{Type: model.BlockToolUse, ID: id, Name: name, Input: json.RawMessage(input)}},
		FinishReason: model.FinishToolCalls,
	}
}

func text(s string) *model.CompletionResponse {
	return &model.CompletionResponse{Blocks: []model.Block{model.TextBlock(s)}, FinishReason: model.FinishStop}
}

// THE M8 done-when, mechanically: spec in → parallel harness workers →
// merged artifact out, with the parallelism PROVEN from the logs.
func TestDoneWhen_SoftwareDevFlow(t *testing.T) {
	st := testutil.NewStore(t)
	a, _, err := st.CreateAgent(ctx, "pipeline", "",
		json.RawMessage(`{"model":"fake","system_prompt":"You are a pipeline role player."}`))
	if err != nil {
		t.Fatal(err)
	}

	sb, err := sandbox.NewExec(filepath.Join(t.TempDir(), "sb"))
	if err != nil {
		t.Fatal(err)
	}
	deps := &loop.Deps{
		Store: st, Model: pipelineModel{}, Sandbox: sb,
		Policy: policy.NewStatic(), Registry: tools.NewRegistry(),
		ModelRetries: 2, RetryBackoff: time.Millisecond,
	}
	ex, err := workflow.NewExecutor(ctx, testutil.DatabaseURL(t), st, sb, slog.Default(), harness.NewNative(deps))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ex.Close() }()

	def := fmt.Sprintf(`{
	  "name": "software-dev-test",
	  "nodes": [
	    {"id": "coder", "agent": %q, "prompt": "You are the coder. SPEC: build fib.", "output_files": ["solution.py"]},
	    {"id": "reviewer", "agent": %q, "depends_on": ["coder"], "prompt": "You are the code reviewer. CODE: {{files.coder.solution.py}}"},
	    {"id": "tester", "agent": %q, "depends_on": ["coder"], "prompt": "You are the test engineer. CODE: {{files.coder.solution.py}}"},
	    {"id": "merger", "agent": %q, "depends_on": ["reviewer", "tester"],
	     "prompt": "You are the merger. CODE: {{files.coder.solution.py}} REVIEW: {{outputs.reviewer}} TEST: {{outputs.tester}}"}
	  ]
	}`, a.ID, a.ID, a.ID, a.ID)

	run, err := ex.Start(ctx, []byte(def))
	if err != nil {
		t.Fatal(err)
	}

	// wait for completion
	deadline := time.Now().Add(30 * time.Second)
	for {
		cur, err := ex.Get(ctx, uuid.MustParse(run.ID))
		if err != nil {
			t.Fatal(err)
		}
		if cur.Status != "running" {
			run = cur
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("workflow never finished; states: %+v", cur.NodeStates)
		}
		time.Sleep(100 * time.Millisecond)
	}

	if run.Status != "completed" {
		t.Fatalf("workflow status = %s; states: %+v", run.Status, run.NodeStates)
	}
	states := map[string]workflow.NodeState{}
	for _, s := range run.NodeStates {
		states[s.ID] = s
		if s.Status != "completed" {
			t.Fatalf("node %s: %s (%s)", s.ID, s.Status, s.Error)
		}
	}

	// 1. DEPENDENCY ORDER: coder finished before reviewer/tester started
	//    (their sessions were created after coder's turn completed).
	sessEvents := func(sessionID string) []store.Event {
		t.Helper()
		evs, err := st.ListEvents(ctx, uuid.MustParse(sessionID), 0, 1000)
		if err != nil {
			t.Fatal(err)
		}
		return evs
	}
	endOf := func(evs []store.Event, typ string) time.Time {
		for _, ev := range evs {
			if ev.Type == typ {
				return ev.CreatedAt
			}
		}
		return time.Time{}
	}
	coderDone := endOf(sessEvents(states["coder"].SessionID), store.EventTurnCompleted)
	reviewerStart := endOf(sessEvents(states["reviewer"].SessionID), store.EventSessionCreated)
	testerStart := endOf(sessEvents(states["tester"].SessionID), store.EventSessionCreated)
	if coderDone.IsZero() || reviewerStart.IsZero() || testerStart.IsZero() {
		t.Fatal("missing marker events")
	}
	if !coderDone.Before(reviewerStart) || !coderDone.Before(testerStart) {
		t.Fatalf("dependency order violated: coder done %v, reviewer start %v, tester start %v",
			coderDone, reviewerStart, testerStart)
	}

	// 2. PARALLELISM: reviewer and tester overlapped — the later session's
	//    creation precedes the earlier one's completion. With the 400ms
	//    role sleeps, sequential execution would be impossible.
	reviewerDone := endOf(sessEvents(states["reviewer"].SessionID), store.EventTurnCompleted)
	testerDone := endOf(sessEvents(states["tester"].SessionID), store.EventTurnCompleted)
	laterStart := reviewerStart
	if testerStart.After(laterStart) {
		laterStart = testerStart
	}
	earlierDone := reviewerDone
	if testerDone.Before(earlierDone) {
		earlierDone = testerDone
	}
	if !laterStart.Before(earlierDone) {
		t.Fatalf("no execution overlap — fan-out was sequential: laterStart %v, earlierDone %v", laterStart, earlierDone)
	}

	// 3. OUTPUT PROPAGATION: the merger's prompt provably contains the
	//    coder's artifact, the review, and the test verdict.
	mergerEvents := sessEvents(states["merger"].SessionID)
	var mergerPrompt string
	for _, ev := range mergerEvents {
		if ev.Type != store.EventMessageUser {
			continue
		}
		var pl struct {
			Content []model.Block `json:"content"`
		}
		_ = json.Unmarshal(ev.Payload, &pl)
		for _, b := range pl.Content {
			mergerPrompt += b.Text
		}
	}
	for _, want := range []string{
		"def fib(n)",             // the coder's file (via files.coder.solution.py)
		"REVIEW: rename nothing", // reviewer output
		"TEST VERDICT: PASS",     // tester output
	} {
		if !strings.Contains(mergerPrompt, want) {
			t.Fatalf("merger prompt missing %q:\n%s", want, mergerPrompt)
		}
	}

	// 4. MERGED ARTIFACT OUT: in the merger session's sandbox.
	h, err := sb.Handle(uuid.MustParse(states["merger"].SessionID))
	if err != nil {
		t.Fatal(err)
	}
	merged, err := h.ReadFile(ctx, "MERGED.md")
	if err != nil || len(merged) == 0 {
		t.Fatalf("MERGED.md missing in merger sandbox: %v (%d bytes)", err, len(merged))
	}
	if !strings.Contains(states["merger"].Output, "merged") {
		t.Fatalf("merger node output: %q", states["merger"].Output)
	}
}

// --- validation ---

func TestDefinitionValidation(t *testing.T) {
	cycle := `{"name":"c","nodes":[
		{"id":"a","agent":"x","prompt":"p","depends_on":["b"]},
		{"id":"b","agent":"x","prompt":"p","depends_on":["a"]}]}`
	if _, err := workflow.ParseDefinition([]byte(cycle)); err == nil {
		t.Fatal("cycle accepted")
	}
	unknownDep := `{"name":"u","nodes":[{"id":"a","agent":"x","prompt":"p","depends_on":["ghost"]}]}`
	if _, err := workflow.ParseDefinition([]byte(unknownDep)); err == nil {
		t.Fatal("unknown dep accepted")
	}
	if _, err := workflow.ParseDefinition([]byte(`{"name":"d","nodes":[]}`)); err == nil {
		t.Fatal("empty graph accepted")
	}
	if _, err := workflow.ParseDefinition([]byte(`{"nodes":[{"id":"a","agent":"x","prompt":"p"}]}`)); err == nil {
		t.Fatal("missing name accepted")
	}
}

func TestRenderPrompt(t *testing.T) {
	out := workflow.RenderPrompt(
		"code: {{files.coder.solution.py}} note: {{outputs.coder}}",
		map[string]workflow.NodeOutput{
			"coder": {Text: "the summary", Files: map[string]string{"solution.py": "print(1)"}},
		})
	if out != "code: print(1) note: the summary" {
		t.Fatalf("rendered: %q", out)
	}
	// unknown variables stay visible
	if got := workflow.RenderPrompt("{{outputs.ghost}} and {{files.a.b}}", nil); got != "{{outputs.ghost}} and {{files.a.b}}" {
		t.Fatalf("unknown vars must stay intact: %q", got)
	}
}

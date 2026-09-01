package loop_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

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

// fakeModel plays a scripted sequence of completions. Each script entry
// may inspect the request it answers.
type fakeModel struct {
	mu     sync.Mutex
	script []func(req *model.CompletionRequest) (*model.CompletionResponse, error)
	calls  int
}

func (f *fakeModel) Complete(_ context.Context, req *model.CompletionRequest) (*model.CompletionResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	i := f.calls
	f.calls++
	if i >= len(f.script) {
		return nil, fmt.Errorf("fake model: script exhausted at call %d", i)
	}
	return f.script[i](req)
}

func (f *fakeModel) callsMade() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func toolUseResp(id, name string, input any) *model.CompletionResponse {
	b, _ := json.Marshal(input)
	return &model.CompletionResponse{
		Blocks:       []model.Block{{Type: model.BlockToolUse, ID: id, Name: name, Input: b}},
		FinishReason: model.FinishToolCalls,
	}
}

func textResp(s string) *model.CompletionResponse {
	return &model.CompletionResponse{
		Blocks:       []model.Block{model.TextBlock(s)},
		FinishReason: model.FinishStop,
	}
}

func errResp(err error) func(*model.CompletionRequest) (*model.CompletionResponse, error) {
	return func(*model.CompletionRequest) (*model.CompletionResponse, error) { return nil, err }
}

type env struct {
	deps *loop.Deps
	fm   *fakeModel
	st   *store.Store
	sess *store.Session
}

func setup(t *testing.T, agentConfig string, script []func(*model.CompletionRequest) (*model.CompletionResponse, error)) *env {
	t.Helper()
	st := testutil.NewStore(t)
	a, _, err := st.CreateAgent(ctx, "test-agent", "", json.RawMessage(agentConfig))
	if err != nil {
		t.Fatal(err)
	}
	sess, _, err := st.CreateSession(ctx, a.ID, 0, "native")
	if err != nil {
		t.Fatal(err)
	}
	fm := &fakeModel{script: script}
	sb, err := sandbox.NewExec(filepath.Join(t.TempDir(), "sb"))
	if err != nil {
		t.Fatal(err)
	}
	deps := &loop.Deps{
		Store:        st,
		Model:        fm,
		Sandbox:      sb,
		Policy:       policy.NewStatic(),
		Registry:     tools.NewRegistry(),
		MaxSteps:     40,
		ModelRetries: 2,
		RetryBackoff: time.Millisecond,
	}
	return &env{deps: deps, fm: fm, st: st, sess: sess}
}

// runAsRunning mirrors what the Runner does before stepping.
func (e *env) runAsRunning(t *testing.T) {
	t.Helper()
	if _, _, err := e.st.TransitionSession(ctx, e.sess.ID, store.StateRunning, nil); err != nil {
		t.Fatal(err)
	}
}

func (e *env) postUser(t *testing.T, text string) {
	t.Helper()
	_, err := e.st.AppendEvent(ctx, e.sess.ID, store.EventMessageUser, store.ActorUser,
		json.RawMessage(fmt.Sprintf(`{"content":[{"type":"text","text":%q}]}`, text)))
	if err != nil {
		t.Fatal(err)
	}
}

func (e *env) events(t *testing.T) []store.Event {
	t.Helper()
	evs, err := e.st.ListEvents(ctx, e.sess.ID, 0, 1000)
	if err != nil {
		t.Fatal(err)
	}
	return evs
}

// assertAssistantBeforeTools checks the idempotency rule structurally:
// every tool.requested for id X follows the assistant event that carries X.
func assertAssistantBeforeTools(t *testing.T, evs []store.Event) {
	t.Helper()
	assistantOf := map[string]int64{} // tool_use id -> assistant seq
	for _, ev := range evs {
		if ev.Type != store.EventMessageAssistant {
			continue
		}
		var pl struct {
			Content []model.Block `json:"content"`
		}
		_ = json.Unmarshal(ev.Payload, &pl)
		for _, b := range pl.Content {
			if b.Type == model.BlockToolUse {
				assistantOf[b.ID] = ev.Seq
			}
		}
	}
	for _, ev := range evs {
		if ev.Type != store.EventToolRequested && ev.Type != store.EventToolCompleted && ev.Type != store.EventToolFailed {
			continue
		}
		var pl struct {
			ToolUseID string `json:"tool_use_id"`
		}
		_ = json.Unmarshal(ev.Payload, &pl)
		aseq, ok := assistantOf[pl.ToolUseID]
		if !ok {
			t.Fatalf("tool event for %q has no assistant message carrying it", pl.ToolUseID)
		}
		if ev.Seq <= aseq {
			t.Fatalf("tool event for %q (seq %d) does not follow its assistant message (seq %d)",
				pl.ToolUseID, ev.Seq, aseq)
		}
	}
}

const testAgentConfig = `{"model":"fake-model","system_prompt":"You are a test agent."}`

// --- happy path: write_file → bash → end_turn ---

func TestReplay_WriteBashEndTurn(t *testing.T) {
	e := setup(t, testAgentConfig, []func(*model.CompletionRequest) (*model.CompletionResponse, error){
		func(req *model.CompletionRequest) (*model.CompletionResponse, error) {
			if len(req.Tools) != 4 {
				t.Errorf("model saw %d tools, want 4", len(req.Tools))
			}
			if req.System != "You are a test agent." {
				t.Errorf("system prompt not passed through: %q", req.System)
			}
			return toolUseResp("tu1", "write_file", map[string]string{
				"path": "hello.txt", "content": "hi from the loop",
			}), nil
		},
		func(req *model.CompletionRequest) (*model.CompletionResponse, error) {
			// second call must see the tool result as the last message
			last := req.Messages[len(req.Messages)-1]
			if len(last.Blocks) != 1 || last.Blocks[0].Type != model.BlockToolResult ||
				last.Blocks[0].ToolUseID != "tu1" {
				t.Fatalf("second model call last message is not the tu1 tool result: %+v", last)
			}
			return toolUseResp("tu2", "bash", map[string]string{"command": "cat hello.txt"}), nil
		},
		func(req *model.CompletionRequest) (*model.CompletionResponse, error) {
			last := req.Messages[len(req.Messages)-1]
			if last.Blocks[0].Content == "" || last.Blocks[0].ToolUseID != "tu2" {
				t.Fatalf("third model call last message is not the tu2 tool result: %+v", last)
			}
			if !strings.Contains(last.Blocks[0].Content, "hi from the loop") {
				t.Errorf("bash output missing from tool result: %q", last.Blocks[0].Content)
			}
			return textResp("done"), nil
		},
	})
	e.runAsRunning(t)
	e.postUser(t, "write hi to hello.txt and cat it")

	steps := []loop.Outcome{loop.OutcomeContinue, loop.OutcomeContinue, loop.OutcomeContinue,
		loop.OutcomeContinue, loop.OutcomeParked}
	for i, want := range steps {
		got, err := loop.Step(ctx, e.deps, e.sess.ID)
		if err != nil {
			t.Fatalf("step %d: %v", i+1, err)
		}
		if got != want {
			t.Fatalf("step %d outcome = %q, want %q", i+1, got, want)
		}
	}
	// A further Step is a no-op: turn done, nothing unprocessed.
	if got, _ := loop.Step(ctx, e.deps, e.sess.ID); got != loop.OutcomeNoop {
		t.Fatalf("post-turn step = %q, want noop", got)
	}

	evs := e.events(t)
	assertAssistantBeforeTools(t, evs)

	sess, err := e.st.GetSession(ctx, e.sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	if sess.State != store.StateIdle || sess.StopReason == nil || *sess.StopReason != store.StopEndTurn {
		t.Fatalf("session state = %s stop_reason = %v, want idle/end_turn", sess.State, sess.StopReason)
	}

	// the artifact is real
	handle, err := e.deps.Sandbox.Handle(e.sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(handle.Workdir(), "hello.txt"))
	if err != nil || string(data) != "hi from the loop" {
		t.Fatalf("hello.txt = %q err=%v, want the written content", data, err)
	}

	// user message was claimed
	for _, ev := range evs {
		if ev.Type == store.EventMessageUser && ev.ProcessedAt == nil {
			t.Error("user message never claimed (processed_at nil)")
		}
	}
}

// --- crash recovery: assistant persisted, tools never ran ---

func TestCrashRecovery_PendingToolsExecuteExactlyOnce(t *testing.T) {
	e := setup(t, testAgentConfig, []func(*model.CompletionRequest) (*model.CompletionResponse, error){
		func(*model.CompletionRequest) (*model.CompletionResponse, error) {
			return textResp("recovered"), nil
		},
	})
	e.runAsRunning(t)
	e.postUser(t, "make a marker")

	// Simulate the crash window: the assistant message (with tool_use) is
	// durable, but its tools never executed.
	_, err := e.st.AppendEvent(ctx, e.sess.ID, store.EventMessageAssistant, store.ActorAgent,
		json.RawMessage(`{"content":[{"type":"tool_use","id":"tu_x","name":"bash","input":{"command":"echo survived > crash.txt"}}],"model":"fake-model"}`))
	if err != nil {
		t.Fatal(err)
	}

	// First Step dispatches the stranded tools.
	if out, err := loop.Step(ctx, e.deps, e.sess.ID); err != nil || out != loop.OutcomeContinue {
		t.Fatalf("recovery step = %q err=%v, want continue", out, err)
	}
	handle, _ := e.deps.Sandbox.Handle(e.sess.ID)
	if _, err := os.Stat(filepath.Join(handle.Workdir(), "crash.txt")); err != nil {
		t.Fatalf("stranded tool never executed: %v", err)
	}

	// Second Step: model call (script[0]) → end_turn.
	if out, err := loop.Step(ctx, e.deps, e.sess.ID); err != nil || out != loop.OutcomeParked {
		t.Fatalf("post-recovery step = %q err=%v, want parked", out, err)
	}

	// Exactly one tool.completed for tu_x — re-stepping must not re-execute.
	completed := 0
	for _, ev := range e.events(t) {
		if ev.Type == store.EventToolCompleted {
			var pl struct {
				ToolUseID string `json:"tool_use_id"`
			}
			_ = json.Unmarshal(ev.Payload, &pl)
			if pl.ToolUseID == "tu_x" {
				completed++
			}
		}
	}
	if completed != 1 {
		t.Fatalf("tool.completed for tu_x = %d, want exactly 1", completed)
	}
	if e.fm.callsMade() != 1 {
		t.Fatalf("model calls = %d, want 1 (tools were already pending; no model call before dispatch)", e.fm.callsMade())
	}
}

// --- policy deny is data, not a crash ---

func TestPolicyDeny_ReturnsRemediationAsResult(t *testing.T) {
	e := setup(t, testAgentConfig, []func(*model.CompletionRequest) (*model.CompletionResponse, error){
		func(*model.CompletionRequest) (*model.CompletionResponse, error) {
			return toolUseResp("tu1", "bash", map[string]string{"command": "sudo touch pwned.txt"}), nil
		},
		func(req *model.CompletionRequest) (*model.CompletionResponse, error) {
			last := req.Messages[len(req.Messages)-1]
			if !last.Blocks[0].IsError || !strings.Contains(last.Blocks[0].Content, "denied") {
				t.Errorf("model did not see the denial as an error result: %+v", last.Blocks[0])
			}
			return textResp("understood"), nil
		},
	})
	e.runAsRunning(t)
	e.postUser(t, "try sudo")

	for i := 0; i < 4; i++ {
		if _, err := loop.Step(ctx, e.deps, e.sess.ID); err != nil {
			t.Fatalf("step %d: %v", i+1, err)
		}
	}

	handle, _ := e.deps.Sandbox.Handle(e.sess.ID)
	if _, err := os.Stat(filepath.Join(handle.Workdir(), "pwned.txt")); err == nil {
		t.Fatal("denied command executed — policy failed")
	}
	// verdict recorded on tool.requested (unlogged decision = never happened)
	found := false
	for _, ev := range e.events(t) {
		if ev.Type != store.EventToolRequested {
			continue
		}
		var pl struct {
			Verdict policy.Verdict `json:"verdict"`
		}
		_ = json.Unmarshal(ev.Payload, &pl)
		if pl.Verdict.Decision == policy.Deny {
			found = true
		}
	}
	if !found {
		t.Fatal("deny verdict not recorded on tool.requested")
	}
}

// --- model failures park the turn honestly ---

func TestModelRetries_ParksWithRetriesExhausted(t *testing.T) {
	e := setup(t, testAgentConfig, []func(*model.CompletionRequest) (*model.CompletionResponse, error){
		errResp(fmt.Errorf("connection reset")),
		errResp(fmt.Errorf("connection reset")),
	})
	e.runAsRunning(t)
	e.postUser(t, "hello")

	out, err := loop.Step(ctx, e.deps, e.sess.ID)
	if err != nil {
		t.Fatalf("step returned error: %v", err)
	}
	if out != loop.OutcomeParked {
		t.Fatalf("outcome = %q, want parked", out)
	}

	var detail string
	sawTurn := false
	for _, ev := range e.events(t) {
		if ev.Type != store.EventTurnCompleted {
			continue
		}
		sawTurn = true
		var pl struct {
			StopReason string `json:"stop_reason"`
			Detail     string `json:"detail"`
		}
		_ = json.Unmarshal(ev.Payload, &pl)
		if pl.StopReason != string(store.StopRetriesExhausted) {
			t.Errorf("stop_reason = %q, want retries_exhausted", pl.StopReason)
		}
		detail = pl.Detail
	}
	if !sawTurn {
		t.Fatal("no turn.completed after model failures")
	}
	if !strings.Contains(detail, "2 attempt") {
		t.Errorf("detail does not mention attempt count: %q", detail)
	}

	sess, _ := e.st.GetSession(ctx, e.sess.ID)
	if sess.State != store.StateIdle {
		t.Fatalf("state = %s, want idle", sess.State)
	}
}

// --- the step cap stops runaway loops ---

func TestStepCap_TripsRetriesExhausted(t *testing.T) {
	// a model that never stops asking for tools, each with a fresh id
	script := make([]func(*model.CompletionRequest) (*model.CompletionResponse, error), 10)
	for i := range script {
		id := fmt.Sprintf("tu-%d", i)
		script[i] = func(*model.CompletionRequest) (*model.CompletionResponse, error) {
			return toolUseResp(id, "bash", map[string]string{"command": "true"}), nil
		}
	}
	e := setup(t, testAgentConfig, script)
	e.deps.MaxSteps = 3
	e.runAsRunning(t)
	e.postUser(t, "loop forever")

	steps := 0
	for {
		out, err := loop.Step(ctx, e.deps, e.sess.ID)
		if err != nil {
			t.Fatalf("step %d: %v", steps+1, err)
		}
		steps++
		if out == loop.OutcomeParked {
			break
		}
		if steps > 20 {
			t.Fatal("step cap never tripped")
		}
	}

	assistant := 0
	stopReason := ""
	for _, ev := range e.events(t) {
		switch ev.Type {
		case store.EventMessageAssistant:
			assistant++
		case store.EventTurnCompleted:
			var pl struct {
				StopReason string `json:"stop_reason"`
			}
			_ = json.Unmarshal(ev.Payload, &pl)
			stopReason = pl.StopReason
		}
	}
	if assistant != 3 {
		t.Fatalf("assistant messages = %d, want 3 (the cap)", assistant)
	}
	if stopReason != string(store.StopRetriesExhausted) {
		t.Fatalf("stop_reason = %q, want retries_exhausted", stopReason)
	}
}

// --- the runner drives a session end to end ---

func TestRunner_DrivesSessionToEndTurn(t *testing.T) {
	e := setup(t, testAgentConfig, []func(*model.CompletionRequest) (*model.CompletionResponse, error){
		func(*model.CompletionRequest) (*model.CompletionResponse, error) {
			return toolUseResp("tu1", "write_file", map[string]string{
				"path": "note.md", "content": "# kept\n",
			}), nil
		},
		func(*model.CompletionRequest) (*model.CompletionResponse, error) {
			return textResp("written"), nil
		},
	})
	runner := loop.NewRunner(ctx, e.deps)
	e.postUser(t, "write a note")
	runner.Kick(e.sess.ID)

	deadline := time.Now().Add(10 * time.Second)
	seenRunning := false
	for {
		sess, err := e.st.GetSession(ctx, e.sess.ID)
		if err != nil {
			t.Fatal(err)
		}
		if sess.State == store.StateRunning {
			seenRunning = true
		}
		// idle only counts as parked once the turn has actually started:
		// the session is BORN idle, before the runner's kick lands.
		if seenRunning && sess.State == store.StateIdle {
			if sess.StopReason != nil && *sess.StopReason == store.StopEndTurn {
				break
			}
			t.Fatalf("idle without end_turn: stop_reason=%v", sess.StopReason)
		}
		if time.Now().After(deadline) {
			t.Fatalf("session never parked; state=%s", sess.State)
		}
		time.Sleep(20 * time.Millisecond)
	}

	// state machine trail: idle → running → idle
	states := []string{}
	for _, ev := range e.events(t) {
		if ev.Type != store.EventSessionStateChanged {
			continue
		}
		var pl struct {
			From string `json:"from"`
			To   string `json:"to"`
		}
		_ = json.Unmarshal(ev.Payload, &pl)
		states = append(states, pl.From+"→"+pl.To)
	}
	if len(states) < 2 || states[0] != "idle→running" {
		t.Fatalf("state trail = %v, want idle→running then →idle", states)
	}
	if states[len(states)-1] != "running→idle" {
		t.Fatalf("last transition = %s, want running→idle", states[len(states)-1])
	}
}

// --- escalation: ask parks at requires_action; the answer resumes ---

func TestEscalation_AskParksAndAnswerResumes(t *testing.T) {
	e := setup(t, testAgentConfig, []func(*model.CompletionRequest) (*model.CompletionResponse, error){
		// turn 1: model wants to push
		func(*model.CompletionRequest) (*model.CompletionResponse, error) {
			return toolUseResp("tu1", "bash", map[string]string{"command": "git push origin main"}), nil
		},
		// turn 2 (after the human answers): model wraps up
		func(req *model.CompletionRequest) (*model.CompletionResponse, error) {
			// the new turn must see BOTH the escalation result and the answer
			var sawEscalation, sawAnswer bool
			for _, m := range req.Messages {
				for _, b := range m.Blocks {
					if b.Type == model.BlockToolResult && strings.Contains(b.Content, "escalated to a human") {
						sawEscalation = true
					}
					if b.Type == model.BlockText && strings.Contains(b.Text, "approved") {
						sawAnswer = true
					}
				}
			}
			if !sawEscalation || !sawAnswer {
				t.Fatalf("turn 2 history missing escalation(%v)/answer(%v)", sawEscalation, sawAnswer)
			}
			return textResp("pushed"), nil
		},
	})
	e.runAsRunning(t)
	e.postUser(t, "ship it")

	// Step 1: model call → assistant with the git push tool_use
	if out, err := loop.Step(ctx, e.deps, e.sess.ID); err != nil || out != loop.OutcomeContinue {
		t.Fatalf("step 1 = %q err=%v", out, err)
	}
	// Step 2: dispatch → ask verdict → parked at requires_action
	if out, err := loop.Step(ctx, e.deps, e.sess.ID); err != nil || out != loop.OutcomeParked {
		t.Fatalf("step 2 = %q err=%v, want parked", out, err)
	}

	sess, _ := e.st.GetSession(ctx, e.sess.ID)
	if sess.State != store.StateIdle || sess.StopReason == nil || *sess.StopReason != store.StopRequiresAction {
		t.Fatalf("state = %s reason = %v, want idle/requires_action", sess.State, sess.StopReason)
	}

	kinds := map[string]int{}
	for _, ev := range e.events(t) {
		kinds[ev.Type]++
	}
	if kinds[store.EventEscalationRequested] != 1 {
		t.Fatalf("escalation.requested count = %d, want 1", kinds[store.EventEscalationRequested])
	}
	if kinds[store.EventToolCompleted] != 1 {
		t.Fatalf("the asked tool must have a result (protocol balance), got %d", kinds[store.EventToolCompleted])
	}

	// The human answers; the next turn runs to end_turn with the full
	// picture. Re-promote to running as the runner would.
	e.runAsRunning(t)
	e.postUser(t, "approved, push it now")
	for {
		out, err := loop.Step(ctx, e.deps, e.sess.ID)
		if err != nil {
			t.Fatal(err)
		}
		if out == loop.OutcomeNoop {
			break
		}
	}
	sess, _ = e.st.GetSession(ctx, e.sess.ID)
	if sess.StopReason == nil || *sess.StopReason != store.StopEndTurn {
		t.Fatalf("after answer: state=%s reason=%v, want end_turn", sess.State, sess.StopReason)
	}
	if e.fm.callsMade() != 2 {
		t.Fatalf("model calls = %d, want 2 (one per turn)", e.fm.callsMade())
	}
}

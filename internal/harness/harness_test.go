package harness_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

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

// --- scripted native model (same idea as loop_test's fakeModel) ---

type scriptedModel struct {
	mu     sync.Mutex
	script []*model.CompletionResponse
	calls  int
}

func (f *scriptedModel) Complete(_ context.Context, _ *model.CompletionRequest) (*model.CompletionResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	i := f.calls
	f.calls++
	if i >= len(f.script) {
		return nil, fmt.Errorf("script exhausted")
	}
	return f.script[i], nil
}

func nativeDeps(t *testing.T, st *store.Store, script []*model.CompletionResponse) *loop.Deps {
	t.Helper()
	sb, err := sandbox.NewExec(filepath.Join(t.TempDir(), "sb"))
	if err != nil {
		t.Fatal(err)
	}
	return &loop.Deps{
		Store: st, Model: &scriptedModel{script: script}, Sandbox: sb,
		Policy: policy.NewStatic(), Registry: tools.NewRegistry(),
		ModelRetries: 2, RetryBackoff: time.Millisecond,
	}
}

// --- fake OpenCode server: THE WIRE CONTRACT ---

type fakeOpenCode struct {
	mu           sync.Mutex
	createdCalls int
	prompts      []string
	permissions  map[string]string // permission id → answered status
	script       []string          // SSE data lines replayed on /event
	sessionID    string
	baseURL      string // set by newFakeOpenCode
}

func newFakeOpenCode(t *testing.T, script []string) *fakeOpenCode {
	f := &fakeOpenCode{permissions: map[string]string{}, script: script, sessionID: "ses_test123"}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/session":
			f.mu.Lock()
			f.createdCalls++
			f.mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]string{"id": f.sessionID})
		case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/session/") && strings.HasSuffix(r.URL.Path, "/message"):
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			f.mu.Lock()
			f.prompts = append(f.prompts, promptText(body))
			f.mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]string{"messageID": "msg_1"})
		case r.Method == http.MethodGet && r.URL.Path == "/event":
			w.Header().Set("Content-Type", "text/event-stream")
			fl := w.(http.Flusher)
			f.mu.Lock()
			script := append([]string(nil), f.script...)
			f.mu.Unlock()
			for _, line := range script {
				fmt.Fprintf(w, "data: %s\n\n", line)
				fl.Flush()
			}
		case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/permission/"):
			id := strings.TrimPrefix(r.URL.Path, "/permission/")
			var body map[string]string
			_ = json.NewDecoder(r.Body).Decode(&body)
			f.mu.Lock()
			f.permissions[id] = body["status"]
			f.mu.Unlock()
			w.WriteHeader(http.StatusOK)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	f.baseURL = srv.URL
	return f
}

func promptText(body map[string]any) string {
	msg, _ := body["message"].(map[string]any)
	parts, _ := msg["parts"].([]any)
	var texts []string
	for _, p := range parts {
		pm, _ := p.(map[string]any)
		if pm["type"] == "text" {
			texts = append(texts, fmt.Sprint(pm["text"]))
		}
	}
	return strings.Join(texts, "\n")
}

// semantic extracts the conformance-comparable shape of an event stream:
// volatile fields (ids, seqs, timestamps, free text, usage) collapse to
// what must match across harnesses.
type semanticEvent struct {
	Type     string `json:"type"`
	Tool     string `json:"tool,omitempty"`
	Stop     string `json:"stop,omitempty"`
	Verdict  string `json:"verdict,omitempty"`
	Actor    string `json:"actor,omitempty"`
	HasTools bool   `json:"has_tools,omitempty"` // message.assistant carried tool_use
}

func semanticStream(t *testing.T, events []store.Event) []semanticEvent {
	t.Helper()
	var out []semanticEvent
	for _, ev := range events {
		switch ev.Type {
		case store.EventMessageUser, store.EventSessionCreated,
			store.EventSessionStateChanged, store.EventHarnessLaunched:
			continue // bookkeeping + input, not turn semantics
		}
		se := semanticEvent{Type: ev.Type, Actor: string(ev.Actor)}
		var pl struct {
			Name       string `json:"name"`
			StopReason string `json:"stop_reason"`
			Verdict    policy.Verdict
			Content    []model.Block `json:"content"`
		}
		_ = json.Unmarshal(ev.Payload, &pl)
		switch ev.Type {
		case store.EventToolRequested:
			se.Tool = pl.Name
			se.Verdict = string(pl.Verdict.Decision)
		case store.EventMessageAssistant:
			for _, b := range pl.Content {
				if b.Type == model.BlockToolUse {
					se.HasTools = true
					if se.Tool == "" {
						se.Tool = b.Name
					}
				}
			}
		case store.EventTurnCompleted:
			se.Stop = pl.StopReason
		}
		out = append(out, se)
	}
	return out
}

// THE M4 done-when (ADR-004): the same scripted task through native and
// OpenCode produces the same normalized event stream.
func TestConformance_NativeVsOpenCodeGoldenTranscript(t *testing.T) {
	task := "write a note file"

	// One store for BOTH harnesses — testutil.NewStore truncates, so a
	// second one inside the same test would wipe the native session.
	st := testutil.NewStore(t)
	nst := st
	na, _, err := nst.CreateAgent(ctx, "conf-native", "", json.RawMessage(`{"model":"fake"}`))
	if err != nil {
		t.Fatal(err)
	}
	nsess, _, err := nst.CreateSession(ctx, na.ID, 0, "native")
	if err != nil {
		t.Fatal(err)
	}
	ndeps := nativeDeps(t, nst, []*model.CompletionResponse{
		{Blocks: []model.Block{{
			Type: model.BlockToolUse, ID: "tu1", Name: "write_file",
			Input: json.RawMessage(`{"path":"note.txt","content":"hi"}`),
		}}, FinishReason: model.FinishToolCalls},
		{Blocks: []model.Block{model.TextBlock("done")}, FinishReason: model.FinishStop},
	})
	native := harness.NewNative(ndeps)
	if _, err := nst.AppendEvent(ctx, nsess.ID, store.EventMessageUser, store.ActorUser,
		json.RawMessage(`{"content":[{"type":"text","text":"`+task+`"}]}`)); err != nil {
		t.Fatal(err)
	}
	spec := harness.WorkerSpec{SessionID: nsess.ID, AgentID: na.ID, AgentVersion: 1, Config: json.RawMessage(`{"model":"fake"}`)}
	handle, err := native.Launch(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	if err := native.Run(ctx, handle); err != nil {
		t.Fatal(err)
	}
	nevents, err := nst.ListEvents(ctx, nsess.ID, 0, 1000)
	if err != nil {
		t.Fatal(err)
	}

	// --- OpenCode side: the fake replays the equivalent protocol ---
	fake := newFakeOpenCode(t, []string{
		`{"type":"message.part.updated","data":{"sessionID":"ses_test123","messageID":"msg_2","role":"assistant","part":{"type":"tool","tool":"write_file","state":"running","input":{"path":"note.txt","content":"hi"}}}}`,
		`{"type":"permission.ask","data":{"id":"per_1","sessionID":"ses_test123","providerID":"write_file","input":{"path":"note.txt","content":"hi"}}}`,
		`{"type":"message.part.updated","data":{"sessionID":"ses_test123","messageID":"msg_2","role":"assistant","part":{"type":"tool","tool":"write_file","state":"completed","output":"wrote 2 bytes"}}}`,
		`{"type":"message.part.updated","data":{"sessionID":"ses_test123","messageID":"msg_3","role":"assistant","part":{"type":"text","text":"done","state":"completed"}}}`,
		`{"type":"session.idle","data":{"sessionID":"ses_test123"}}`,
	})
	ost := st
	oa, _, err := ost.CreateAgent(ctx, "conf-oc", "", json.RawMessage(`{"model":"fake"}`))
	if err != nil {
		t.Fatal(err)
	}
	osess, _, err := ost.CreateSession(ctx, oa.ID, 0, "opencode")
	if err != nil {
		t.Fatal(err)
	}
	oc := harness.NewOpenCode(fake.baseURL, ost, policy.NewStatic())
	if _, err := ost.AppendEvent(ctx, osess.ID, store.EventMessageUser, store.ActorUser,
		json.RawMessage(`{"content":[{"type":"text","text":"`+task+`"}]}`)); err != nil {
		t.Fatal(err)
	}
	ospec := harness.WorkerSpec{SessionID: osess.ID, AgentID: oa.ID, AgentVersion: 1, Config: json.RawMessage(`{"model":"fake"}`)}
	ohandle, err := oc.Launch(ctx, ospec)
	if err != nil {
		t.Fatal(err)
	}
	if err := oc.Run(ctx, ohandle); err != nil {
		t.Fatal(err)
	}
	oevents, err := ost.ListEvents(ctx, osess.ID, 0, 1000)
	if err != nil {
		t.Fatal(err)
	}

	// --- the criterion ---
	nstream := semanticStream(t, nevents)
	ostream := semanticStream(t, oevents)
	if len(nstream) == 0 || len(ostream) == 0 {
		t.Fatalf("empty streams: native=%d opencode=%d", len(nstream), len(ostream))
	}
	if fmt.Sprint(nstream) != fmt.Sprint(ostream) {
		t.Fatalf("normalized streams diverge:\n  native:   %s\n  opencode: %s", fmt.Sprint(nstream), fmt.Sprint(ostream))
	}
	// and the shape is the one the roadmap demanded
	want := []semanticEvent{
		{Type: store.EventMessageAssistant, Actor: "agent", Tool: "write_file", HasTools: true},
		{Type: store.EventToolRequested, Actor: "system", Tool: "write_file", Verdict: "allow"},
		{Type: store.EventToolCompleted, Actor: "system"},
		{Type: store.EventMessageAssistant, Actor: "agent"},
		{Type: store.EventTurnCompleted, Actor: "system", Stop: "end_turn"},
	}
	if fmt.Sprint(nstream) != fmt.Sprint(want) {
		t.Fatalf("native stream shape drift:\n  got:  %s\n  want: %s", fmt.Sprint(nstream), fmt.Sprint(want))
	}

	// the prompt reached the harness, the permission was delegated and
	// answered with our engine's verdict
	if got := fake.prompts; len(got) != 1 || got[0] != task {
		t.Fatalf("prompts = %v, want [%q]", got, task)
	}
	if fake.permissions["per_1"] != "once" {
		t.Fatalf("permission answer = %q, want once (allow)", fake.permissions["per_1"])
	}

	// both sessions parked idle/end_turn
	ns, _ := nst.GetSession(ctx, nsess.ID)
	os2, _ := ost.GetSession(ctx, osess.ID)
	if ns.State != store.StateIdle || os2.State != store.StateIdle ||
		ns.StopReason == nil || *ns.StopReason != store.StopEndTurn ||
		os2.StopReason == nil || *os2.StopReason != store.StopEndTurn {
		t.Fatalf("parking mismatch: native=%s/%v opencode=%s/%v",
			ns.State, ns.StopReason, os2.State, os2.StopReason)
	}
}

// The governance showcase: OpenCode asks, OUR engine says no, the answer
// goes back as reject and the turn parks at requires_action.
func TestOpenCodePermissionDelegationDeny(t *testing.T) {
	fake := newFakeOpenCode(t, []string{
		`{"type":"permission.ask","data":{"id":"per_9","sessionID":"ses_test123","providerID":"bash","input":{"command":"sudo rm -rf x"}}}`,
		`{"type":"message.part.updated","data":{"sessionID":"ses_test123","messageID":"msg_2","role":"assistant","part":{"type":"text","text":"permission was denied","state":"completed"}}}`,
		`{"type":"session.idle","data":{"sessionID":"ses_test123"}}`,
	})
	st := testutil.NewStore(t)
	a, _, err := st.CreateAgent(ctx, "oc-deny", "", json.RawMessage(`{"model":"fake"}`))
	if err != nil {
		t.Fatal(err)
	}
	sess, _, err := st.CreateSession(ctx, a.ID, 0, "opencode")
	if err != nil {
		t.Fatal(err)
	}
	oc := harness.NewOpenCode(fake.baseURL, st, policy.NewStatic())
	if _, err := st.AppendEvent(ctx, sess.ID, store.EventMessageUser, store.ActorUser,
		json.RawMessage(`{"content":[{"type":"text","text":"try sudo"}]}`)); err != nil {
		t.Fatal(err)
	}
	handle, err := oc.Launch(ctx, harness.WorkerSpec{SessionID: sess.ID, AgentID: a.ID, AgentVersion: 1})
	if err != nil {
		t.Fatal(err)
	}
	if err := oc.Run(ctx, handle); err != nil {
		t.Fatal(err)
	}

	if fake.permissions["per_9"] != "reject" {
		t.Fatalf("deny verdict not forwarded: %v", fake.permissions)
	}
	s2, _ := st.GetSession(ctx, sess.ID)
	if s2.StopReason == nil || *s2.StopReason != store.StopRequiresAction {
		t.Fatalf("stop_reason = %v, want requires_action after a denied delegation", s2.StopReason)
	}
}

// Launch is idempotent and replay-backed: a second Launch must not create
// another opencode session (the harness.launched mapping is durable).
func TestOpenCodeLaunchIdempotentViaHarnessLaunchedEvent(t *testing.T) {
	fake := newFakeOpenCode(t, nil)
	st := testutil.NewStore(t)
	a, _, _ := st.CreateAgent(ctx, "oc-idem", "", json.RawMessage(`{"model":"fake"}`))
	sess, _, _ := st.CreateSession(ctx, a.ID, 0, "opencode")
	oc := harness.NewOpenCode(fake.baseURL, st, policy.NewStatic())
	spec := harness.WorkerSpec{SessionID: sess.ID, AgentID: a.ID, AgentVersion: 1}

	if _, err := oc.Launch(ctx, spec); err != nil {
		t.Fatal(err)
	}
	handle, err := oc.Launch(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	if fake.createdCalls != 1 {
		t.Fatalf("opencode sessions created = %d, want 1 (idempotent launch)", fake.createdCalls)
	}

	// checkpoint round-trips through Resume
	tok, err := oc.Checkpoint(ctx, handle)
	if err != nil {
		t.Fatal(err)
	}
	if tok.Harness != "opencode" {
		t.Fatalf("token minted by %q", tok.Harness)
	}
	if _, err := oc.Resume(ctx, spec, tok); err != nil {
		t.Fatal(err)
	}
	if _, err := oc.Resume(ctx, spec, harness.CheckpointToken{Harness: "native"}); err == nil {
		t.Fatal("resume with a foreign token must fail")
	}
}

package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/warren-wupeng/agentd/internal/api"
	"github.com/warren-wupeng/agentd/internal/harness"
	"github.com/warren-wupeng/agentd/internal/sandbox"
	"github.com/warren-wupeng/agentd/internal/store"
	"github.com/warren-wupeng/agentd/internal/testutil"
	"github.com/warren-wupeng/agentd/internal/workflow"
)

type server struct {
	t  *testing.T
	ts *httptest.Server
}

func TestMain(m *testing.M) { testutil.Main(m) }

func newServer(t *testing.T, opts ...api.Option) *server {
	t.Helper()
	_, srv := newServerWithStore(t, opts...)
	return srv
}

func newServerWithStore(t *testing.T, opts ...api.Option) (*storeFixture, *server) {
	t.Helper()
	st := testutil.NewStore(t)
	ts := httptest.NewServer(api.NewHandler(st, opts...))
	t.Cleanup(ts.Close)
	return &storeFixture{store: st, databaseURL: testutil.DatabaseURL(t)}, &server{t: t, ts: ts}
}

type storeFixture struct {
	store       *store.Store
	databaseURL string
}

// recordingRunner is an api.Runner that records kicks instead of running
// the loop — the API layer's contract is "the right session gets kicked",
// not what the loop then does (loop_test covers that).
type recordingRunner struct {
	mu    sync.Mutex
	kicks []string
}

func (r *recordingRunner) Kick(sessionID uuid.UUID) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.kicks = append(r.kicks, sessionID.String())
}

func (r *recordingRunner) kicked() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.kicks...)
}

// do performs a request and fails the test on transport errors. Body may be nil.
func (s *server) do(method, path string, body any) (int, map[string]any) {
	s.t.Helper()
	var rdr io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			s.t.Fatal(err)
		}
		rdr = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, s.ts.URL+path, rdr)
	if err != nil {
		s.t.Fatal(err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		s.t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var out map[string]any
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &out); err != nil {
			s.t.Fatalf("non-JSON response to %s %s: %s", method, path, raw)
		}
	}
	return resp.StatusCode, out
}

func (s *server) mustCreateAgent(name, model string) string {
	s.t.Helper()
	code, resp := s.do("POST", "/v1/agents", map[string]any{
		"name": name, "description": "test agent",
		"config": map[string]any{"model": model},
	})
	if code != http.StatusCreated {
		s.t.Fatalf("create agent: %d %v", code, resp)
	}
	return resp["agent"].(map[string]any)["id"].(string)
}

func TestHealthz(t *testing.T) {
	s := newServer(t)
	code, resp := s.do("GET", "/healthz", nil)
	if code != http.StatusOK || resp["status"] != "ok" {
		t.Fatalf("healthz: %d %v", code, resp)
	}
}

func TestAgentVersioningOverHTTP(t *testing.T) {
	s := newServer(t)
	id := s.mustCreateAgent("http-versioned", "m1")

	// Unknown fields are rejected with a remediation (G5).
	code, resp := s.do("POST", "/v1/agents", map[string]any{
		"name": "bad", "confg": map[string]any{"model": "m"},
	})
	if code != http.StatusBadRequest || resp["error"].(map[string]any)["remediation"] == "" {
		t.Fatalf("want 400 with remediation, got %d %v", code, resp)
	}

	// Missing config.model → 400.
	code, _ = s.do("POST", "/v1/agents", map[string]any{
		"name": "nomodel", "config": map[string]any{"system_prompt": "hi"},
	})
	if code != http.StatusBadRequest {
		t.Fatalf("want 400 for missing model, got %d", code)
	}

	// PUT creates v2; v1 remains retrievable and semantically intact.
	code, resp = s.do("PUT", "/v1/agents/"+id, map[string]any{
		"config": map[string]any{"model": "m2", "system_prompt": "new"},
	})
	if code != http.StatusCreated || resp["version"].(map[string]any)["version"].(float64) != 2 {
		t.Fatalf("create v2: %d %v", code, resp)
	}

	code, resp = s.do("GET", "/v1/agents/"+id+"/versions/1", nil)
	if code != http.StatusOK {
		t.Fatalf("get v1: %d %v", code, resp)
	}
	cfg := resp["version"].(map[string]any)["config"].(map[string]any)
	if cfg["model"] != "m1" {
		t.Fatalf("v1 config drifted: %v", cfg)
	}

	code, resp = s.do("GET", "/v1/agents/"+id+"/versions/99", nil)
	if code != http.StatusNotFound {
		t.Fatalf("want 404, got %d", code)
	}
	rem := resp["error"].(map[string]any)["remediation"].(string)
	if rem == "" {
		t.Fatal("404 must carry remediation")
	}
}

func newWorkflowOption(t *testing.T, st *store.Store, databaseURL string) api.Option {
	t.Helper()
	sb, err := sandbox.NewExec(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ex, err := workflow.NewExecutor(context.Background(), databaseURL, st, sb, slog.Default(), stubHarness{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ex.Close() })
	return api.WithWorkflow(ex)
}

// stubHarness satisfies the Harness interface for tests that only wire
// the executor (workflow list endpoint) and never launch a node. Real
// execution paths are covered by workflow_test via harness.NewNative.
type stubHarness struct{}

func (stubHarness) Name() string                        { return "native" }
func (stubHarness) Capabilities() harness.CapabilitySet { return harness.CapabilitySet{} }
func (stubHarness) Launch(_ context.Context, spec harness.WorkerSpec) (harness.Handle, error) {
	return harness.Handle{Spec: spec}, nil
}
func (stubHarness) Run(_ context.Context, _ harness.Handle) error { return nil }
func (stubHarness) Checkpoint(_ context.Context, _ harness.Handle) (harness.CheckpointToken, error) {
	return harness.CheckpointToken{Harness: "native"}, nil
}
func (stubHarness) Resume(_ context.Context, spec harness.WorkerSpec, _ harness.CheckpointToken) (harness.Handle, error) {
	return harness.Handle{Spec: spec}, nil
}
func (stubHarness) Interrupt(uuid.UUID) {}

func TestDeleteAgentOverHTTP(t *testing.T) {
	s := newServer(t)
	id := s.mustCreateAgent("deletable", "m1")

	// With a live session → 409.
	code, _ := s.do("POST", "/v1/sessions", map[string]any{"agent_id": id})
	if code != http.StatusCreated {
		t.Fatal("setup session failed")
	}
	code, resp := s.do("DELETE", "/v1/agents/"+id, nil)
	if code != http.StatusConflict || resp["error"].(map[string]any)["remediation"] == "" {
		t.Fatalf("want 409 with remediation, got %d %v", code, resp)
	}

	// Without sessions → 204.
	id2 := s.mustCreateAgent("deletable2", "m1")
	code, _ = s.do("DELETE", "/v1/agents/"+id2, nil)
	if code != http.StatusNoContent {
		t.Fatalf("want 204, got %d", code)
	}
}

// TestReplay_SessionEventLog is the G4 replay test for the events endpoint:
// kill the flow mid-run (here: replay from a mid-log cursor), reconnect,
// and assert the client sees an ordered, lossless, idempotent history.
func TestWorkflowListOverHTTP(t *testing.T) {
	fixture, s := newServerWithStore(t)
	s.ts.Config.Handler = api.NewHandler(fixture.store, newWorkflowOption(t, fixture.store, fixture.databaseURL))
	a1 := s.mustCreateAgent("wf-a", "m1")
	a2 := s.mustCreateAgent("wf-b", "m1")
	defs := []string{
		fmt.Sprintf(`{"name":"older","nodes":[{"id":"n1","agent":%q,"prompt":"do one"}]}`, a1),
		fmt.Sprintf(`{"name":"newer","nodes":[{"id":"n2","agent":%q,"prompt":"do two"}]}`, a2),
	}
	for _, def := range defs {
		code, resp := s.do("POST", "/v1/workflows", json.RawMessage(def))
		if code != http.StatusAccepted {
			t.Fatalf("start workflow: %d %v", code, resp)
		}
	}

	code, resp := s.do("GET", "/v1/workflows?limit=1", nil)
	if code != http.StatusOK {
		t.Fatalf("list workflows: %d %v", code, resp)
	}
	runs := resp["runs"].([]any)
	if len(runs) != 1 {
		t.Fatalf("want 1 run, got %d", len(runs))
	}
	latest := runs[0].(map[string]any)
	if latest["name"] != "newer" {
		t.Fatalf("want newest workflow first, got %v", latest)
	}
	if latest["created_at"] == nil || latest["created_at"] == "" {
		t.Fatalf("want created_at from server, got %v", latest)
	}

	code, resp = s.do("GET", "/v1/workflows?limit=0", nil)
	if code != http.StatusBadRequest || resp["error"].(map[string]any)["remediation"] == "" {
		t.Fatalf("want 400 with remediation for bad limit, got %d %v", code, resp)
	}
}

func TestReplay_SessionEventLog(t *testing.T) {
	s := newServer(t)
	agentID := s.mustCreateAgent("replay-api", "m1")

	code, resp := s.do("POST", "/v1/sessions", map[string]any{"agent_id": agentID})
	if code != http.StatusCreated {
		t.Fatalf("create session: %v", resp)
	}
	sessID := resp["session"].(map[string]any)["id"].(string)

	for i := 0; i < 50; i++ {
		code, resp := s.do("POST", "/v1/sessions/"+sessID+"/events",
			map[string]any{"payload": map[string]any{"i": i}})
		if code != http.StatusCreated {
			t.Fatalf("append %d: %d %v", i, code, resp)
		}
	}

	// Full page: 51 events (50 + session.created), cursor present at limit.
	code, resp = s.do("GET", "/v1/sessions/"+sessID, nil)
	if code != http.StatusOK || resp["session"].(map[string]any)["state"] != "rescheduling" {
		t.Fatalf("get session: %d %s", code, errStr(resp))
	}
	code, resp = s.do("GET", "/v1/sessions/"+sessID+"/events?limit=10", nil)
	if code != http.StatusOK {
		t.Fatalf("list events: %d", code)
	}
	events := resp["events"].([]any)
	if len(events) != 10 {
		t.Fatalf("want 10 events, got %d", len(events))
	}
	cursor := resp["next_after_seq"].(float64)

	// Replay from a mid-log cursor twice: identical, ordered ids.
	var first, second []any
	code, resp = s.do("GET", fmt.Sprintf("/v1/sessions/%s/events?after_seq=%d", sessID, int(cursor)), nil)
	if code != http.StatusOK {
		t.Fatal("replay failed")
	}
	first = resp["events"].([]any)
	code, resp = s.do("GET", fmt.Sprintf("/v1/sessions/%s/events?after_seq=%d", sessID, int(cursor)), nil)
	if code != http.StatusOK {
		t.Fatal("replay 2 failed")
	}
	second = resp["events"].([]any)

	if len(first) != len(second) || len(first) == 0 {
		t.Fatalf("replay mismatch: %d vs %d", len(first), len(second))
	}
	prevSeq := 0.0
	for i := range first {
		a, b := first[i].(map[string]any), second[i].(map[string]any)
		if a["id"] != b["id"] {
			t.Fatalf("replay not idempotent at %d", i)
		}
		seq := a["seq"].(float64)
		if seq <= prevSeq {
			t.Fatalf("events out of order: %v after %v", seq, prevSeq)
		}
		prevSeq = seq
	}

	// Claim an event; claiming twice must not move processed_at.
	evID := first[0].(map[string]any)["id"].(string)
	code, resp = s.do("POST", "/v1/sessions/"+sessID+"/events/"+evID+"/claim", nil)
	if code != http.StatusOK || resp["event"].(map[string]any)["processed_at"] == nil {
		t.Fatalf("claim: %d %v", code, resp)
	}
	pt1 := resp["event"].(map[string]any)["processed_at"].(string)
	code, resp = s.do("POST", "/v1/sessions/"+sessID+"/events/"+evID+"/claim", nil)
	if code != http.StatusOK || resp["event"].(map[string]any)["processed_at"].(string) != pt1 {
		t.Fatalf("second claim changed processed_at: %v", resp)
	}

	// User cannot post system event types.
	code, resp = s.do("POST", "/v1/sessions/"+sessID+"/events",
		map[string]any{"type": "session.state_changed", "payload": map[string]any{}})
	if code != http.StatusBadRequest {
		t.Fatalf("want 400 for system type, got %d %v", code, resp)
	}
}

func TestTransitionsOverHTTP(t *testing.T) {
	s := newServer(t)
	agentID := s.mustCreateAgent("fsm-api", "m1")
	code, resp := s.do("POST", "/v1/sessions", map[string]any{"agent_id": agentID})
	if code != http.StatusCreated {
		t.Fatal(resp)
	}
	sessID := resp["session"].(map[string]any)["id"].(string)

	// Illegal edge → 409 with remediation naming legal targets.
	code, resp = s.do("POST", "/v1/sessions/"+sessID+"/transitions", map[string]any{"to": "idle"})
	if code != http.StatusConflict {
		t.Fatalf("want 409, got %d %v", code, resp)
	}
	e := resp["error"].(map[string]any)
	if e["code"] != "INVALID_TRANSITION" || e["remediation"] == "" {
		t.Fatalf("bad error shape: %v", e)
	}

	code, resp = s.do("POST", "/v1/sessions/"+sessID+"/transitions", map[string]any{"to": "running"})
	if code != http.StatusOK || resp["session"].(map[string]any)["state"] != "running" {
		t.Fatalf("→running: %d %v", code, resp)
	}
	if resp["event"].(map[string]any)["type"] != "session.state_changed" {
		t.Fatalf("transition did not emit event: %v", resp)
	}

	code, resp = s.do("POST", "/v1/sessions/"+sessID+"/transitions",
		map[string]any{"to": "idle", "stop_reason": "requires_action"})
	if code != http.StatusOK {
		t.Fatalf("→idle: %d %v", code, resp)
	}
	if resp["session"].(map[string]any)["stop_reason"] != "requires_action" {
		t.Fatalf("stop_reason missing: %v", resp)
	}

	// idle → terminated is legal; terminated is final.
	code, _ = s.do("POST", "/v1/sessions/"+sessID+"/transitions", map[string]any{"to": "terminated"})
	if code != http.StatusOK {
		t.Fatalf("idle→terminated should be legal, got %d", code)
	}
	code, resp = s.do("POST", "/v1/sessions/"+sessID+"/transitions", map[string]any{"to": "running"})
	if code != http.StatusConflict {
		t.Fatalf("terminated must be final, got %d %v", code, resp)
	}
}

func errStr(resp map[string]any) string {
	raw, _ := json.Marshal(resp)
	return string(raw)
}

// G4: the run endpoint's replayable behavior is "kick the session actor,
// never mutate state inline" — the actor owns transitions.

func TestReplay_RunEndpointKicksActor(t *testing.T) {
	rr := &recordingRunner{}
	s := newServer(t, api.WithRunner(rr))
	agentID := s.mustCreateAgent("run-api", "m2")
	code, resp := s.do("POST", "/v1/sessions", map[string]any{"agent_id": agentID})
	if code != http.StatusCreated {
		t.Fatal(resp)
	}
	sessID := resp["session"].(map[string]any)["id"].(string)

	code, resp = s.do("POST", "/v1/sessions/"+sessID+"/run", nil)
	if code != http.StatusAccepted {
		t.Fatalf("run: %d %v", code, resp)
	}
	if resp["accepted"] != true {
		t.Fatalf("run response missing accepted: %v", resp)
	}
	// state untouched synchronously — the actor moves it asynchronously
	if resp["session"].(map[string]any)["state"] != "rescheduling" {
		t.Fatalf("run must not mutate state inline: %v", resp)
	}

	kicks := rr.kicked()
	if len(kicks) != 1 || kicks[0] != sessID {
		t.Fatalf("kicks = %v, want [%s]", kicks, sessID)
	}
}

func TestRunEndpointWithoutRunnerIs409(t *testing.T) {
	s := newServer(t) // no WithRunner
	agentID := s.mustCreateAgent("norun-api", "m2")
	code, resp := s.do("POST", "/v1/sessions", map[string]any{"agent_id": agentID})
	if code != http.StatusCreated {
		t.Fatal(resp)
	}
	sessID := resp["session"].(map[string]any)["id"].(string)

	code, resp = s.do("POST", "/v1/sessions/"+sessID+"/run", nil)
	if code != http.StatusConflict {
		t.Fatalf("run without loop: %d %v", code, resp)
	}
	if e, ok := resp["error"].(map[string]any); !ok || e["remediation"] == nil {
		t.Fatalf("error must carry a remediation: %v", resp)
	}
}

func TestMessageAppendAutoKicksNativeSession(t *testing.T) {
	rr := &recordingRunner{}
	s := newServer(t, api.WithRunner(rr))
	agentID := s.mustCreateAgent("auto-api", "m2")
	code, resp := s.do("POST", "/v1/sessions", map[string]any{"agent_id": agentID})
	if code != http.StatusCreated {
		t.Fatal(resp)
	}
	sessID := resp["session"].(map[string]any)["id"].(string)

	code, resp = s.do("POST", "/v1/sessions/"+sessID+"/events", map[string]any{
		"payload": map[string]any{"content": []map[string]any{
			{"type": "text", "text": "hello"},
		}},
	})
	if code != http.StatusCreated {
		t.Fatalf("append: %d %v", code, resp)
	}
	kicks := rr.kicked()
	if len(kicks) != 1 || kicks[0] != sessID {
		t.Fatalf("auto-kick = %v, want [%s]", kicks, sessID)
	}

	// terminated sessions never get kicked again
	code, _ = s.do("POST", "/v1/sessions/"+sessID+"/transitions", map[string]any{"to": "terminated"})
	if code != http.StatusOK {
		t.Fatal("terminate failed")
	}
	code, _ = s.do("POST", "/v1/sessions/"+sessID+"/events", map[string]any{
		"payload": map[string]any{"content": []map[string]any{
			{"type": "text", "text": "too late"},
		}},
	})
	if code != http.StatusConflict { // terminated log is immutable → error, no kick
		t.Fatalf("append to terminated: %d", code)
	}
	if n := len(rr.kicked()); n != 1 {
		t.Fatalf("kicks after termination = %d, want 1", n)
	}
}

// doWithHeader performs a JSON request with one extra header.
func (s *server) doWithHeader(method, path string, body any, header, value string) (int, map[string]any) {
	s.t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		s.t.Fatal(err)
	}
	req, err := http.NewRequest(method, s.ts.URL+path, bytes.NewReader(raw))
	if err != nil {
		s.t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(header, value)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		s.t.Fatal(err)
	}
	defer resp.Body.Close()
	rawResp, _ := io.ReadAll(resp.Body)
	var out map[string]any
	if len(rawResp) > 0 {
		if err := json.Unmarshal(rawResp, &out); err != nil {
			s.t.Fatalf("non-JSON response to %s %s: %s", method, path, rawResp)
		}
	}
	return resp.StatusCode, out
}

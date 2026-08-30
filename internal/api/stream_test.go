package api_test

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/warren-wupeng/agentd/internal/api"
	"github.com/warren-wupeng/agentd/internal/hub"
	"github.com/warren-wupeng/agentd/internal/loop"
	"github.com/warren-wupeng/agentd/internal/model"
	"github.com/warren-wupeng/agentd/internal/policy"
	"github.com/warren-wupeng/agentd/internal/sandbox"
	"github.com/warren-wupeng/agentd/internal/store"
	"github.com/warren-wupeng/agentd/internal/testutil"
	"github.com/warren-wupeng/agentd/internal/tools"
)

// newStreamServer wraps a fully wired handler (loop runner + hub +
// listener) in the test server helper.
func newStreamServer(t *testing.T, handler http.Handler) *server {
	t.Helper()
	ts := httptest.NewServer(handler)
	t.Cleanup(ts.Close)
	return &server{t: t, ts: ts}
}

// doReq issues a JSON request against the server and decodes the reply.
func doReq(t *testing.T, s *server, method, path string, body any) (int, map[string]any) {
	t.Helper()
	return s.do(method, path, body)
}

// streamingModel implements Provider + Streamer: scripted responses, with
// text blocks dripped as slow deltas so a client can be killed mid-stream.
type streamingModel struct {
	mu     sync.Mutex
	script []*model.CompletionResponse
	calls  int
}

func (f *streamingModel) Complete(_ context.Context, _ *model.CompletionRequest) (*model.CompletionResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	i := f.calls
	f.calls++
	if i >= len(f.script) {
		return nil, fmt.Errorf("script exhausted")
	}
	return f.script[i], nil
}

func (f *streamingModel) Stream(ctx context.Context, _ *model.CompletionRequest, onDelta func(model.Delta)) (*model.CompletionResponse, error) {
	resp, err := f.Complete(ctx, nil)
	if err != nil {
		return nil, err
	}
	for _, b := range resp.Blocks {
		if b.Type != model.BlockText {
			continue
		}
		// drip in two halves with a gap — enough window to kill the client
		for _, half := range []string{b.Text[:len(b.Text)/2], b.Text[len(b.Text)/2:]} {
			onDelta(model.Delta{Type: "text", Text: half})
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(150 * time.Millisecond):
			}
		}
	}
	return resp, nil
}

type sseFrame struct {
	event string
	id    int64
	data  string
}

// readFrame parses one SSE frame off the wire.
func readFrame(r *bufio.Reader) (sseFrame, error) {
	var f sseFrame
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return f, err
		}
		line = strings.TrimRight(line, "\n")
		if line == "" {
			if f.event == "" && f.id == 0 && f.data == "" {
				continue // stray blank line
			}
			return f, nil
		}
		switch {
		case strings.HasPrefix(line, "event: "):
			f.event = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "id: "):
			if n, err := strconv.ParseInt(strings.TrimPrefix(line, "id: "), 10, 64); err == nil {
				f.id = n
			}
		case strings.HasPrefix(line, "data: "):
			f.data = strings.TrimPrefix(line, "data: ")
		}
	}
}

// frameSource pumps frames off a live response body into a channel.
func frameSource(t *testing.T, resp *http.Response) <-chan sseFrame {
	t.Helper()
	out := make(chan sseFrame, 128)
	go func() {
		defer close(out)
		br := bufio.NewReader(resp.Body)
		for {
			f, err := readFrame(br)
			if err != nil {
				return
			}
			out <- f
		}
	}()
	return out
}

func recv(t *testing.T, frames <-chan sseFrame, what string) sseFrame {
	t.Helper()
	select {
	case f, ok := <-frames:
		if !ok {
			t.Fatalf("stream closed while waiting for %s", what)
		}
		return f
	case <-time.After(10 * time.Second):
		t.Fatalf("timeout waiting for %s", what)
	}
	return sseFrame{}
}

// G4 / roadmap done-when: kill the client mid-run, reconnect with
// Last-Event-ID, nothing lost. Deltas seen before the kill are ephemeral
// and never re-delivered; the durable log replays exactly, in order,
// without gaps.
func TestReplay_StreamSurvivesClientKill(t *testing.T) {
	st := testutil.NewStore(t)
	sb, err := sandbox.NewExec(t.TempDir() + "/sb")
	if err != nil {
		t.Fatal(err)
	}
	fm := &streamingModel{script: []*model.CompletionResponse{
		{Blocks: []model.Block{{
			Type: model.BlockToolUse, ID: "tu1", Name: "write_file",
			Input: json.RawMessage(`{"path":"note.txt","content":"survived"}`),
		}}, FinishReason: model.FinishToolCalls},
		{Blocks: []model.Block{model.TextBlock("all done, file written and verified")},
			FinishReason: model.FinishStop},
	}}
	deltas := hub.New()
	deps := &loop.Deps{
		Store: st, Model: fm, Sandbox: sb, Policy: policy.NewStatic(),
		Registry: tools.NewRegistry(), ModelRetries: 2, RetryBackoff: time.Millisecond,
		Deltas: deltas,
	}
	runner := loop.NewRunner(context.Background(), deps)
	listener, err := store.NewEventListener(context.Background(), testutil.DatabaseURL(t))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listener.Close() }()
	ts := newStreamServer(t, api.NewHandler(st,
		api.WithRunner(runner), api.WithStream(deltas, listener)))

	// agent + session
	code, resp := doReq(t, ts, "POST", "/v1/agents", map[string]any{
		"name": "stream-agent", "config": map[string]any{"model": "fake"},
	})
	if code != http.StatusCreated {
		t.Fatal(resp)
	}
	agentID := resp["agent"].(map[string]any)["id"].(string)
	code, resp = doReq(t, ts, "POST", "/v1/sessions", map[string]any{"agent_id": agentID})
	if code != http.StatusCreated {
		t.Fatal(resp)
	}
	sessID := resp["session"].(map[string]any)["id"].(string)

	// connect the first client
	ctx1, kill := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx1, "GET", ts.ts.URL+"/v1/sessions/"+sessID+"/stream", nil)
	client1, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	frames1 := frameSource(t, client1)

	// start the turn
	code, _ = doReq(t, ts, "POST", "/v1/sessions/"+sessID+"/events", map[string]any{
		"payload": map[string]any{"content": []map[string]any{{"type": "text", "text": "write a note"}}},
	})
	if code != http.StatusCreated {
		t.Fatalf("append: %d", code)
	}

	// wait until deltas are flowing, then kill the client mid-stream
	lastLogSeq := int64(0)
	deltasSeen := 0
	for deltasSeen < 2 {
		f := recv(t, frames1, "frame")
		switch f.event {
		case "log":
			var ev struct {
				Seq int64 `json:"seq"`
			}
			if err := json.Unmarshal([]byte(f.data), &ev); err != nil {
				t.Fatalf("log frame data not JSON: %s", f.data)
			}
			lastLogSeq = ev.Seq
		case "delta":
			deltasSeen++
		}
	}
	kill()
	_ = client1.Body.Close()

	// wait for the turn to finish without us
	deadline := time.Now().Add(15 * time.Second)
	for {
		code, resp = doReq(t, ts, "GET", "/v1/sessions/"+sessID, nil)
		if code != http.StatusOK {
			t.Fatalf("get session: %d", code)
		}
		s := resp["session"].(map[string]any)
		if s["state"] == "idle" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("session never parked after client kill")
		}
		time.Sleep(50 * time.Millisecond)
	}

	// reconnect with the native SSE cursor and drain to the end
	req2, _ := http.NewRequest("GET", ts.ts.URL+"/v1/sessions/"+sessID+"/stream", nil)
	req2.Header.Set("Last-Event-ID", fmt.Sprintf("%d", lastLogSeq))
	client2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatal(err)
	}
	defer client2.Body.Close()
	frames2 := frameSource(t, client2)

	var gotSeqs []int64
	sawTurnCompleted := false
	for {
		var f sseFrame
		if sawTurnCompleted {
			// turn.completed lands before the final running→idle
			// state_changed; drain briefly to catch the tail.
			select {
			case f = <-frames2:
			case <-time.After(300 * time.Millisecond):
			}
		} else {
			f = recv(t, frames2, "reconnect frame")
		}
		if f.event == "" && f.data == "" {
			break // drain window closed with nothing more
		}
		if f.event != "log" {
			continue
		}
		var ev struct {
			Seq  int64  `json:"seq"`
			Type string `json:"type"`
		}
		if err := json.Unmarshal([]byte(f.data), &ev); err != nil {
			t.Fatalf("bad log frame: %s", f.data)
		}
		gotSeqs = append(gotSeqs, ev.Seq)
		if ev.Type == "turn.completed" {
			sawTurnCompleted = true
		}
	}

	// nothing ≤ lastLogSeq is re-delivered, order is strict
	for _, seq := range gotSeqs {
		if seq <= lastLogSeq {
			t.Fatalf("re-delivered event %d (cursor was %d)", seq, lastLogSeq)
		}
	}
	for i := 1; i < len(gotSeqs); i++ {
		if gotSeqs[i] <= gotSeqs[i-1] {
			t.Fatalf("out of order: %v", gotSeqs)
		}
	}
	// no gaps: the reconnect stream must reach the final event contiguously
	sid, err := uuid.Parse(sessID)
	if err != nil {
		t.Fatal(err)
	}
	all, err := st.ListEvents(context.Background(), sid, 0, 1000)
	if err != nil {
		t.Fatal(err)
	}
	lastSeq := all[len(all)-1].Seq
	if len(gotSeqs) == 0 || gotSeqs[len(gotSeqs)-1] != lastSeq {
		t.Fatalf("reconnect ended at %v, want the log tail %d", gotSeqs, lastSeq)
	}
	want := lastSeq - lastLogSeq
	if int64(len(gotSeqs)) != want {
		t.Fatalf("got %d frames after cursor, want %d (gap or duplicate)", len(gotSeqs), want)
	}
	if !sawTurnCompleted {
		t.Fatal("reconnect never saw turn.completed")
	}
}

func TestStreamEndpointWithoutStreamWiringIs409(t *testing.T) {
	s := newServer(t)
	agentID := s.mustCreateAgent("nostream", "m")
	code, resp := s.do("POST", "/v1/sessions", map[string]any{"agent_id": agentID})
	if code != http.StatusCreated {
		t.Fatal(resp)
	}
	sessID := resp["session"].(map[string]any)["id"].(string)
	code, resp = s.do("GET", "/v1/sessions/"+sessID+"/stream", nil)
	if code != http.StatusConflict {
		t.Fatalf("stream without wiring: %d %v", code, resp)
	}
}

func TestStreamBadCursorIs400(t *testing.T) {
	s := newServer(t)
	agentID := s.mustCreateAgent("badcursor", "m")
	code, resp := s.do("POST", "/v1/sessions", map[string]any{"agent_id": agentID})
	if code != http.StatusCreated {
		t.Fatal(resp)
	}
	sessID := resp["session"].(map[string]any)["id"].(string)
	code, resp = s.do("GET", "/v1/sessions/"+sessID+"/stream?after_seq=abc", nil)
	if code != http.StatusBadRequest {
		t.Fatalf("bad cursor: %d %v", code, resp)
	}
}

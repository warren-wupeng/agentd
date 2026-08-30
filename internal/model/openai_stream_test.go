package model

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// cannedStream exercises the parser against a realistic chunk sequence:
// text deltas, a tool_call split across fragments (id/name first, then
// two argument fragments), finish_reason, usage chunk, [DONE].
const cannedStream = `data: {"choices":[{"index":0,"delta":{"role":"assistant","content":"Hel"},"finish_reason":null}]}

data: {"choices":[{"index":0,"delta":{"content":"lo "},"finish_reason":null}]}

data: {"choices":[{"index":0,"delta":{"content":"world"},"finish_reason":null}]}

data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"toolu_1","type":"function","function":{"name":"write_file","arguments":""}}]},"finish_reason":null}]}

data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"path\":\"a"}}]},"finish_reason":null}]}

data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":".txt\",\"content\":\"hi\"}"}}]},"finish_reason":null}]}

data: {"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}

data: {"choices":[{"index":0,"delta":{}}],"usage":{"prompt_tokens":10,"completion_tokens":7}}

data: [DONE]

`

func TestOpenAIStreamParsesTextDeltasAndToolFragments(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, cannedStream)
	}))
	defer srv.Close()
	o := NewOpenAI(srv.URL, "test-key")

	var got []Delta
	resp, err := o.Stream(context.Background(), &CompletionRequest{Model: "m"}, func(d Delta) {
		got = append(got, d)
	})
	if err != nil {
		t.Fatal(err)
	}

	// text deltas passed through in order
	if len(got) != 3 || got[0].Text != "Hel" || got[1].Text != "lo " || got[2].Text != "world" {
		t.Fatalf("text deltas = %+v", got)
	}

	// assembled response: text block + complete tool_use block
	if len(resp.Blocks) != 2 {
		t.Fatalf("blocks = %+v", resp.Blocks)
	}
	if resp.Blocks[0].Text != "Hello world" {
		t.Fatalf("text block = %q", resp.Blocks[0].Text)
	}
	tu := resp.Blocks[1]
	if tu.Type != BlockToolUse || tu.ID != "toolu_1" || tu.Name != "write_file" ||
		string(tu.Input) != `{"path":"a.txt","content":"hi"}` {
		t.Fatalf("tool_use block = %+v", tu)
	}
	if resp.FinishReason != FinishToolCalls {
		t.Fatalf("finish = %q", resp.FinishReason)
	}
	if resp.Usage.InputTokens != 10 || resp.Usage.OutputTokens != 7 {
		t.Fatalf("usage = %+v", resp.Usage)
	}
}

func TestOpenAIStreamNon200IsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":{"message":"boom"}}`, http.StatusBadGateway)
	}))
	defer srv.Close()
	o := NewOpenAI(srv.URL, "k")
	if _, err := o.Stream(context.Background(), &CompletionRequest{Model: "m"}, nil); err == nil ||
		!strings.Contains(err.Error(), "502") {
		t.Fatalf("want 502 error, got %v", err)
	}
}

// The streamed request must carry stream:true and the explicit-null
// content discipline (same regression class as the non-streaming path).
func TestOpenAIStreamRequestShape(t *testing.T) {
	var body string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		body = string(raw)
		_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()
	o := NewOpenAI(srv.URL, "k")
	_, _ = o.Stream(context.Background(), &CompletionRequest{
		Model:    "m",
		Messages: []Message{{Role: "user", Blocks: []Block{TextBlock("hi")}}},
	}, nil)
	if !strings.Contains(body, `"stream":true`) {
		t.Fatalf("stream flag missing: %s", body)
	}
	if !strings.Contains(body, `"stream_options":{"include_usage":true}`) {
		t.Fatalf("include_usage missing: %s", body)
	}
}

// Regression: the gateway's gemini route streams tool_call arguments in
// the cumulative-final dialect — the closing fragment (partial:false)
// REPEATS the complete arguments. Concatenating both copies produced
// invalid JSON that silently became an empty event payload (found in
// the M3 live demo). The parser must REPLACE on the final fragment.
func TestOpenAIStreamOpenRouterFinalFragmentRepeatsArguments(t *testing.T) {
	body := `data: {"choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"toolu_1","type":"function","function":{"name":"write_file","arguments":"","partial":true}}]}}]}

data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"type":"function","function":{"name":"","arguments":"{\"path\":\"a.txt\",\"content\":\"hi\"}","partial":true}}]}}]}

data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"type":"function","function":{"name":"","arguments":"{\"path\":\"a.txt\",\"content\":\"hi\"}","partial":false}}]},"finish_reason":"stop"}]}

data: [DONE]

`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, body)
	}))
	defer srv.Close()
	o := NewOpenAI(srv.URL, "k")

	resp, err := o.Stream(context.Background(), &CompletionRequest{Model: "m"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Blocks) != 1 {
		t.Fatalf("blocks = %+v", resp.Blocks)
	}
	tu := resp.Blocks[0]
	if tu.ID != "toolu_1" || tu.Name != "write_file" ||
		string(tu.Input) != `{"path":"a.txt","content":"hi"}` {
		t.Fatalf("assembled tool_use = %+v input=%s", tu, tu.Input)
	}
}

// Invalid assembled arguments must surface as an error (retryable),
// never as a garbage event payload downstream.
func TestOpenAIStreamInvalidArgumentsIsError(t *testing.T) {
	body := `data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"t1","type":"function","function":{"name":"bash","arguments":"{\"command\"","partial":true}}]}}]}

data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"type":"function","function":{"name":"","arguments":"\"ls\"}","partial":false}}]},"finish_reason":"tool_calls"}]}

data: [DONE]

`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, body)
	}))
	defer srv.Close()
	o := NewOpenAI(srv.URL, "k")

	resp, err := o.Stream(context.Background(), &CompletionRequest{Model: "m"}, nil)
	if err == nil {
		t.Fatalf("expected invalid-arguments error, got %+v", resp)
	}
	if !strings.Contains(err.Error(), "invalid JSON arguments") {
		t.Fatalf("error should name the problem: %v", err)
	}
}

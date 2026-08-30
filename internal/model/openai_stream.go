package model

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// Stream implements Streamer against any OpenAI-compatible endpoint.
// Text deltas flow to onDelta as they arrive; tool_call fragments are
// assembled by index (first fragment carries id/name, later fragments
// append to arguments) and returned in the assembled response — the log
// records facts, not fragments.
func (o *OpenAI) Stream(ctx context.Context, req *CompletionRequest, onDelta func(Delta)) (*CompletionResponse, error) {
	if onDelta == nil {
		onDelta = func(Delta) {}
	}
	wr := wireRequest{Model: req.Model, MaxTokens: req.MaxTokens, Stream: true,
		StreamOptions: map[string]bool{"include_usage": true}}
	if req.System != "" {
		s := req.System
		wr.Messages = append(wr.Messages, wireMessage{Role: "system", Content: &s})
	}
	for _, m := range req.Messages {
		wr.Messages = append(wr.Messages, toWire(m)...)
	}
	for _, t := range req.Tools {
		var d wireToolDef
		d.Type = "function"
		d.Function.Name = t.Name
		d.Function.Description = t.Description
		d.Function.Parameters = t.Parameters
		wr.Tools = append(wr.Tools, d)
	}

	body, err := json.Marshal(wr)
	if err != nil {
		return nil, fmt.Errorf("marshal stream request: %w", err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		o.baseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+o.apiKey)
	httpReq.Header.Set("Accept", "text/event-stream")

	resp, err := o.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("model stream request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		buf := new(bytes.Buffer)
		_, _ = buf.ReadFrom(resp.Body)
		return nil, fmt.Errorf("model endpoint returned %s: %.400s", resp.Status, buf.String())
	}

	var text strings.Builder
	var toolOrder []int
	toolCalls := map[int]*wireTool{}
	finish := ""
	out := &CompletionResponse{}

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data:") {
			continue // comments, blank lines, event: tags
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "" || data == "[DONE]" {
			continue
		}
		var chunk streamChunk
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			return nil, fmt.Errorf("decode stream chunk: %w", err)
		}
		for i := range chunk.Choices {
			c := &chunk.Choices[i]
			if c.Delta.Content != "" {
				onDelta(Delta{Type: "text", Text: c.Delta.Content})
				text.WriteString(c.Delta.Content)
			}
			for _, tc := range c.Delta.ToolCalls {
				mergeToolCallFragment(&toolOrder, toolCalls, tc)
			}
			if c.FinishReason != "" {
				finish = c.FinishReason
			}
		}
		if chunk.Usage != nil {
			out.Usage = Usage{InputTokens: chunk.Usage.InputTokens, OutputTokens: chunk.Usage.OutputTokens}
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read stream: %w", err)
	}

	if text.Len() > 0 {
		out.Blocks = append(out.Blocks, TextBlock(text.String()))
	}
	for _, idx := range toolOrder {
		tc := toolCalls[idx]
		if !json.Valid([]byte(tc.Function.Arguments)) {
			return nil, fmt.Errorf(
				"model streamed tool_call %q with invalid JSON arguments after assembly: %.120s",
				tc.Function.Name, tc.Function.Arguments)
		}
		out.Blocks = append(out.Blocks, Block{
			Type:  BlockToolUse,
			ID:    tc.ID,
			Name:  tc.Function.Name,
			Input: json.RawMessage(tc.Function.Arguments),
		})
	}
	switch finish {
	case "tool_calls", "function_call":
		out.FinishReason = FinishToolCalls
	default:
		out.FinishReason = FinishStop
	}
	return out, nil
}

// mergeToolCallFragment assembles one streamed tool_call. Two fragment
// dialects exist: incremental (OpenAI pass-through — no "partial" field,
// every fragment's arguments append) and cumulative-final (OpenRouter-
// style — "partial": false marks the fragment carrying the COMPLETE
// arguments, which must REPLACE the accumulation, not append to it:
// the gateway repeats the full value on the closing fragment).
func mergeToolCallFragment(order *[]int, calls map[int]*wireTool, frag wireToolDlt) {
	final := frag.Function.Partial != nil && !*frag.Function.Partial
	tc, ok := calls[frag.Index]
	if !ok {
		tc = &wireTool{ID: frag.ID, Type: "function"}
		tc.Function.Name = frag.Function.Name
		tc.Function.Arguments = frag.Function.Arguments
		calls[frag.Index] = tc
		*order = append(*order, frag.Index)
		return
	}
	if frag.ID != "" {
		tc.ID = frag.ID
	}
	if frag.Function.Name != "" {
		tc.Function.Name += frag.Function.Name
	}
	if final && frag.Function.Arguments != "" {
		tc.Function.Arguments = frag.Function.Arguments
	} else {
		tc.Function.Arguments += frag.Function.Arguments
	}
}

// streamChunk is one SSE `data:` payload from a chat-completions stream.
type streamChunk struct {
	Choices []struct {
		Index        int          `json:"index"`
		Delta        deltaPayload `json:"delta"`
		FinishReason string       `json:"finish_reason"`
	} `json:"choices"`
	Usage *struct {
		InputTokens  int `json:"prompt_tokens"`
		OutputTokens int `json:"completion_tokens"`
	} `json:"usage"`
}

type deltaPayload struct {
	Content   string        `json:"content"`
	ToolCalls []wireToolDlt `json:"tool_calls"`
}

// wireToolDlt is a streamed tool_call fragment. Partial is a *bool so
// "field absent" (OpenAI incremental dialect) is distinguishable from
// "present and false" (OpenRouter final fragment — see mergeToolCallFragment).
type wireToolDlt struct {
	Index    int    `json:"index"`
	ID       string `json:"id,omitempty"`
	Type     string `json:"type,omitempty"`
	Function struct {
		Name      string `json:"name,omitempty"`
		Arguments string `json:"arguments,omitempty"`
		Partial   *bool  `json:"partial,omitempty"`
	} `json:"function"`
}

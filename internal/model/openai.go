package model

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/warren-wupeng/agentd/internal/agentderr"
)

// OpenAI is a Provider against any OpenAI-compatible chat completions
// endpoint (OpenAI, OpenRouter, vLLM, gateways). BaseURL points at the
// API root, e.g. https://api.openai.com/v1 — Complete appends
// /chat/completions.
type OpenAI struct {
	baseURL string
	apiKey  string
	client  *http.Client
}

func NewOpenAI(baseURL, apiKey string) *OpenAI {
	return &OpenAI{
		baseURL: strings.TrimRight(baseURL, "/"),
		apiKey:  apiKey,
		client:  &http.Client{Timeout: 120 * time.Second},
	}
}

// wire types — kept local and boring; if this grows a streaming sibling
// (M3) they move to a wire.go in this package.

type wireMessage struct {
	Role string `json:"role"`
	// Content has NO omitempty: assistant messages that carry tool_calls
	// must serialize an explicit "content": null — several OpenAI-
	// compatible backends (observed: our gateway's google-vertex route)
	// reject the request outright when the field is absent.
	Content    *string    `json:"content"`
	ToolCalls  []wireTool `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"` // role "tool"
	Name       string     `json:"name,omitempty"`
}

type wireTool struct {
	ID       string `json:"id"`
	Type     string `json:"type"` // "function"
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"` // JSON-encoded object
	} `json:"function"`
}

type wireToolDef struct {
	Type     string `json:"type"` // "function"
	Function struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		Parameters  json.RawMessage `json:"parameters"`
	} `json:"function"`
}

type wireRequest struct {
	Model         string          `json:"model"`
	Messages      []wireMessage   `json:"messages"`
	Tools         []wireToolDef   `json:"tools,omitempty"`
	MaxTokens     int             `json:"max_tokens,omitempty"`
	Stream        bool            `json:"stream,omitempty"`
	StreamOptions map[string]bool `json:"stream_options,omitempty"`
}

type wireResponse struct {
	Choices []struct {
		FinishReason string `json:"finish_reason"`
		Message      struct {
			Role      string     `json:"role"`
			Content   string     `json:"content"`
			ToolCalls []wireTool `json:"tool_calls"`
		} `json:"message"`
	} `json:"choices"`
	Usage struct {
		InputTokens  int `json:"prompt_tokens"`
		OutputTokens int `json:"completion_tokens"`
	} `json:"usage"`
	Error *struct {
		Message string `json:"message"`
		Type    string `json:"type"`
	} `json:"error"`
}

func (o *OpenAI) Complete(ctx context.Context, req *CompletionRequest) (*CompletionResponse, error) {
	wr := wireRequest{Model: req.Model, MaxTokens: req.MaxTokens}
	if req.System != "" {
		wr.Messages = append(wr.Messages, wireMessage{Role: "system", Content: &req.System})
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
		return nil, agentderr.Internal(err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		o.baseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, agentderr.Internal(err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+o.apiKey)

	resp, err := o.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("model request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, fmt.Errorf("read model response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		slog.Debug("model request rejected",
			"status", resp.Status, "request", string(body), "response", string(raw))
		return nil, fmt.Errorf("model endpoint returned %s: %.400s", resp.Status, raw)
	}
	var wr2 wireResponse
	if err := json.Unmarshal(raw, &wr2); err != nil {
		return nil, fmt.Errorf("decode model response: %w", err)
	}
	if wr2.Error != nil {
		return nil, fmt.Errorf("model endpoint error (%s): %s", wr2.Error.Type, wr2.Error.Message)
	}
	if len(wr2.Choices) == 0 {
		return nil, fmt.Errorf("model endpoint returned no choices")
	}

	choice := wr2.Choices[0]
	out := &CompletionResponse{Usage: Usage{
		InputTokens:  wr2.Usage.InputTokens,
		OutputTokens: wr2.Usage.OutputTokens,
	}}
	if choice.Message.Content != "" {
		out.Blocks = append(out.Blocks, TextBlock(choice.Message.Content))
	}
	for _, tc := range choice.Message.ToolCalls {
		if !json.Valid([]byte(tc.Function.Arguments)) {
			return nil, fmt.Errorf(
				"model returned tool_call %q with invalid JSON arguments: %.120s",
				tc.Function.Name, tc.Function.Arguments)
		}
		out.Blocks = append(out.Blocks, Block{
			Type:  BlockToolUse,
			ID:    tc.ID,
			Name:  tc.Function.Name,
			Input: json.RawMessage(tc.Function.Arguments),
		})
	}
	switch choice.FinishReason {
	case "tool_calls", "function_call":
		out.FinishReason = FinishToolCalls
	default:
		out.FinishReason = FinishStop
	}
	return out, nil
}

// toWire maps one neutral message to wire messages: text first, then tool
// calls (assistant) or tool results (user). A user message made of
// tool_result blocks becomes one role:"tool" message per block — that's
// the OpenAI shape; other providers remap differently.
func toWire(m Message) []wireMessage {
	var out []wireMessage
	var text strings.Builder
	var toolUses []wireTool
	var toolResults []Block
	for _, b := range m.Blocks {
		switch b.Type {
		case BlockText:
			text.WriteString(b.Text)
		case BlockToolUse:
			wt := wireTool{ID: b.ID, Type: "function"}
			wt.Function.Name = b.Name
			wt.Function.Arguments = string(b.Input)
			toolUses = append(toolUses, wt)
		case BlockToolResult:
			toolResults = append(toolResults, b)
		}
	}
	// Text first, then tool calls/results — the OpenAI shape.
	if text.Len() > 0 {
		s := text.String()
		out = append(out, wireMessage{Role: m.Role, Content: &s})
	}
	for _, wt := range toolUses {
		wm := wireMessage{Role: "assistant"}
		wm.ToolCalls = []wireTool{wt}
		out = append(out, wm)
	}
	for _, tr := range toolResults {
		content := tr.Content
		wm := wireMessage{Role: "tool", ToolCallID: tr.ToolUseID, Content: &content}
		if tr.IsError {
			wm.Name = "error"
		}
		out = append(out, wm)
	}
	return out
}

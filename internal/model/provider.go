// Package model is the provider-neutral seam between the loop and whoever
// turns messages into completions. The loop never sees a wire format; a
// provider maps these types to its own (OpenAI-compatible today,
// Anthropic-native later — see docs/design/agent-loop.md).
package model

import (
	"context"
	"encoding/json"
)

// Block types. One flat discriminated struct on purpose: these land in
// jsonb event payloads verbatim, and a union of tiny structs buys nothing
// there.
const (
	BlockText       = "text"
	BlockToolUse    = "tool_use"
	BlockToolResult = "tool_result"
)

// Block is one content block. Only the fields for its Type are set.
type Block struct {
	Type string `json:"type"`

	// text
	Text string `json:"text,omitempty"`

	// tool_use
	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`

	// tool_result
	ToolUseID string `json:"tool_use_id,omitempty"`
	Content   string `json:"content,omitempty"`
	IsError   bool   `json:"is_error,omitempty"`
}

func TextBlock(text string) Block {
	return Block{Type: BlockText, Text: text}
}

// Message is one turn participant. Tool results ride in user-role messages
// as tool_result blocks — providers remap to their wire shape.
type Message struct {
	Role   string  `json:"role"` // user | assistant
	Blocks []Block `json:"blocks"`
}

// ToolDef is a tool as the model sees it.
type ToolDef struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"` // JSON Schema
}

// Usage is what billing and budgets need. Providers that do not report
// usage leave zeros — the loop records what it knows, never invents.
type Usage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

// CompletionRequest is everything a provider needs to run one call.
type CompletionRequest struct {
	Model     string    `json:"model"`
	System    string    `json:"system,omitempty"`
	MaxTokens int       `json:"max_tokens,omitempty"`
	Messages  []Message `json:"messages"`
	Tools     []ToolDef `json:"tools,omitempty"`
}

// FinishReason values a provider must normalize to.
const (
	FinishStop      = "stop"       // no tool calls — turn can end
	FinishToolCalls = "tool_calls" // assistant wants tools
)

// CompletionResponse is the neutral assistant turn.
type CompletionResponse struct {
	Blocks       []Block `json:"blocks"`
	FinishReason string  `json:"finish_reason"` // stop | tool_calls
	Usage        Usage   `json:"usage"`
}

// Provider runs one completion. Errors here are infrastructure (network,
// auth, rate limit) — the loop retries them; semantic content never
// arrives as an error.
type Provider interface {
	Complete(ctx context.Context, req *CompletionRequest) (*CompletionResponse, error)
}

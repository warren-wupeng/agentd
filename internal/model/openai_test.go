package model

import (
	"encoding/json"
	"strings"
	"testing"
)

// toWire is where provider-neutrality meets the OpenAI wire format; the
// mapping is load-bearing for tool loops, so it's pinned here.
func TestToWireOrdering(t *testing.T) {
	msg := Message{
		Role: "assistant",
		Blocks: []Block{
			TextBlock("thinking out loud"),
			{Type: BlockToolUse, ID: "tu1", Name: "bash", Input: json.RawMessage(`{"command":"ls"}`)},
		},
	}
	wire := toWire(msg)
	if len(wire) != 2 {
		t.Fatalf("wire messages = %d, want 2 (text then tool_call)", len(wire))
	}
	if wire[0].Role != "assistant" || wire[0].Content == nil || *wire[0].Content != "thinking out loud" {
		t.Fatalf("first wire message wrong: %+v", wire[0])
	}
	if len(wire[1].ToolCalls) != 1 || wire[1].ToolCalls[0].ID != "tu1" ||
		wire[1].ToolCalls[0].Function.Name != "bash" {
		t.Fatalf("tool_call wire message wrong: %+v", wire[1])
	}
}

func TestToWireToolResults(t *testing.T) {
	msg := Message{
		Role: "user",
		Blocks: []Block{
			{Type: BlockToolResult, ToolUseID: "tu1", Content: "ok"},
			{Type: BlockToolResult, ToolUseID: "tu2", Content: "boom", IsError: true},
		},
	}
	wire := toWire(msg)
	if len(wire) != 2 {
		t.Fatalf("wire messages = %d, want one per tool_result", len(wire))
	}
	if wire[0].Role != "tool" || wire[0].ToolCallID != "tu1" || *wire[0].Content != "ok" {
		t.Fatalf("first tool result wrong: %+v", wire[0])
	}
	if wire[1].ToolCallID != "tu2" || *wire[1].Content != "boom" || wire[1].Name != "error" {
		t.Fatalf("error tool result wrong: %+v", wire[1])
	}
}

// Regression: assistant messages with tool_calls must serialize an explicit
// "content": null — absent content 500s on some OpenAI-compatible backends
// (found against the internal gateway's google-vertex route during the M2
// end-to-end demo).
func TestWireAssistantToolCallsEmitExplicitNullContent(t *testing.T) {
	msg := Message{
		Role: "assistant",
		Blocks: []Block{
			{Type: BlockToolUse, ID: "tu1", Name: "bash", Input: json.RawMessage(`{}`)},
		},
	}
	wire := toWire(msg)
	raw, err := json.Marshal(wire)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"content":null`) {
		t.Fatalf("assistant tool_call message must carry explicit content:null, got: %s", raw)
	}
}

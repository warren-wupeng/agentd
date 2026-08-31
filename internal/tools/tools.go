// Package tools is the native harness toolbox: bash + the three file
// primitives the loop ships at M2 (docs/design/agent-loop.md). Tools are
// how the model touches the world; everything they do lands in the event
// log via the loop.
package tools

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/warren-wupeng/agentd/internal/policy"
	"github.com/warren-wupeng/agentd/internal/sandbox"
)

// Tool is one capability offered to the model.
type Tool interface {
	Name() string
	Description() string
	// Schema is the JSON Schema for the input object, as sent to the model.
	Schema() json.RawMessage
	// PolicyDefault is the tool's own verdict hint; the engine has final say.
	PolicyDefault() policy.Verdict
	// Execute runs the tool. A non-nil error marks tool.failed
	// (infrastructure); semantic failures come back as ordinary output
	// the model can read and react to.
	Execute(ctx context.Context, h sandbox.Handle, input json.RawMessage) (string, error)
}

// Registry is the set of tools a session may use.
type Registry struct {
	byName map[string]Tool
	order  []string
}

// RegistryOption extends a registry at construction.
type RegistryOption func(*Registry)

// WithMCP adds the control-plane MCP proxy tool (M6): model-visible,
// credential-free from the sandbox's perspective.
func WithMCP(c MCPCaller) RegistryOption {
	return func(r *Registry) {
		t := NewMCP(c)
		r.byName[t.Name()] = t
		r.order = append(r.order, t.Name())
	}
}

// NewRegistry returns the built-in tools plus any options.
func NewRegistry(opts ...RegistryOption) *Registry {
	r := &Registry{byName: map[string]Tool{}}
	for _, t := range []Tool{NewBash(), NewReadFile(), NewWriteFile(), NewEditFile()} {
		r.byName[t.Name()] = t
		r.order = append(r.order, t.Name())
	}
	for _, o := range opts {
		o(r)
	}
	return r
}

// Get returns one tool by name.
func (r *Registry) Get(name string) (Tool, bool) {
	t, ok := r.byName[name]
	return t, ok
}

// All returns every registered tool.
func (r *Registry) All() []Tool {
	out := make([]Tool, 0, len(r.order))
	for _, n := range r.order {
		out = append(out, r.byName[n])
	}
	return out
}

// Enable returns the tools named in names (nil/empty = all). Unknown
// names return an error naming the valid set — an agent config listing a
// typo'd tool must fail loudly, not silently run a reduced toolbox.
func (r *Registry) Enable(names []string) ([]Tool, error) {
	if len(names) == 0 {
		return r.All(), nil
	}
	var out []Tool
	for _, n := range names {
		t, ok := r.byName[n]
		if !ok {
			return nil, fmt.Errorf("unknown tool %q; known tools: %v", n, r.order)
		}
		out = append(out, t)
	}
	return out, nil
}

// objSchema builds a JSON Schema object with properties/required.
func objSchema(props map[string]any, required ...string) json.RawMessage {
	b, err := json.Marshal(map[string]any{
		"type":                 "object",
		"properties":           props,
		"required":             required,
		"additionalProperties": false,
	})
	if err != nil {
		panic(err) // static schemas; a bug here is a build-time bug
	}
	return b
}

func prop(desc, typ string) map[string]any {
	return map[string]any{"type": typ, "description": desc}
}

func propInt(desc string) map[string]any {
	return map[string]any{"type": "integer", "description": desc}
}

func propBool(desc string) map[string]any {
	return map[string]any{"type": "boolean", "description": desc}
}

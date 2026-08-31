package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/warren-wupeng/agentd/internal/agentderr"
	"github.com/warren-wupeng/agentd/internal/policy"
	"github.com/warren-wupeng/agentd/internal/sandbox"
)

// MCPCaller is the seam to the control-plane MCP proxy (internal/mcp).
// The tool lives in the sandbox's tool list but executes in the control
// plane — credentials are injected server-side and never cross into
// the sandbox (M6).
type MCPCaller interface {
	Call(ctx context.Context, server, method, path string, body json.RawMessage) (status int, response string, err error)
}

// MCP is the model-visible tool for calling registered MCP servers.
type MCP struct {
	caller MCPCaller
}

func NewMCP(c MCPCaller) *MCP { return &MCP{caller: c} }

func (t *MCP) Name() string { return "mcp" }
func (t *MCP) PolicyDefault() policy.Verdict {
	return policy.Verdict{Decision: policy.Allow}
}

func (t *MCP) Description() string {
	return "Call a registered MCP server through the agentd proxy. Credentials are injected by the control " +
		"plane — you never handle them. Use servers listed by your operator; method defaults to POST."
}

func (t *MCP) Schema() json.RawMessage {
	return objSchema(map[string]any{
		"server": prop("Registered MCP server name", "string"),
		"path":   prop("Path on the server, relative to its base URL", "string"),
		"method": prop("HTTP method (default POST)", "string"),
		"body": map[string]any{
			"description": "Optional JSON request body",
			"type":        "object",
		},
	}, "server", "path")
}

// Execute runs control-plane side. The handle param is unused on
// purpose — the sandbox is not part of this call path.
func (t *MCP) Execute(ctx context.Context, _ sandbox.Handle, input json.RawMessage) (string, error) {
	var in struct {
		Server string          `json:"server"`
		Path   string          `json:"path"`
		Method string          `json:"method"`
		Body   json.RawMessage `json:"body"`
	}
	if err := json.Unmarshal(input, &in); err != nil {
		return "", fmt.Errorf("parse input: %w", err)
	}
	if in.Server == "" || in.Path == "" {
		return "error: server and path are required", nil
	}
	method := in.Method
	if method == "" {
		method = "POST"
	}

	status, resp, err := t.caller.Call(ctx, in.Server, method, in.Path, in.Body)
	if err != nil {
		// Registry/vault/reachability problems are model-visible data
		// when they carry remediation; infrastructure otherwise.
		if msg := remediationOf(err); msg != "" {
			return "error: " + err.Error() + " — " + msg, nil
		}
		return "", err
	}
	return fmt.Sprintf("[status %d]\n%s", status, resp), nil
}

// remediationOf extracts the remediation from an agentderr error so
// the model can act on it; empty for untyped errors.
func remediationOf(err error) string {
	var ae *agentderr.Error
	if errors.As(err, &ae) {
		return ae.Remediation
	}
	return ""
}

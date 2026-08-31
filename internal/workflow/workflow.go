// Package workflow is the M8 DAG executor (ADR-004: minimal and
// embedded — agent semantics only, no Argo rebuild). Nodes are tasks
// bound to an agent + harness; each node runs as an ordinary session
// through the harness seam; outputs propagate downstream by prompt
// injection (text + output_files), keeping every node exactly as
// isolated as M5 proved sessions to be.
package workflow

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/warren-wupeng/agentd/internal/agentderr"
)

// NodeDef is one task in the DAG.
type NodeDef struct {
	ID          string   `json:"id"`
	Agent       string   `json:"agent"`             // agent id (UUID) — resolved at run time
	Harness     string   `json:"harness,omitempty"` // default native
	Prompt      string   `json:"prompt"`            // template; {{outputs.<node>}} / {{files.<node>.<path>}}
	DependsOn   []string `json:"depends_on,omitempty"`
	MaxRetries  int      `json:"max_retries,omitempty"`  // default 1 attempt
	OutputFiles []string `json:"output_files,omitempty"` // sandbox paths read after the node parks
}

// Definition is a workflow — a JSON file, versioned in git (review
// artifact, same discipline as eval datasets).
type Definition struct {
	Name  string    `json:"name"`
	Nodes []NodeDef `json:"nodes"`
}

// NodeState is the persisted state of one node in a run.
type NodeState struct {
	ID        string `json:"id"`
	Status    string `json:"status"` // pending | running | completed | failed
	SessionID string `json:"session_id,omitempty"`
	Output    string `json:"output,omitempty"` // final assistant text
	Error     string `json:"error,omitempty"`  // last failure reason
	Attempts  int    `json:"attempts,omitempty"`
	StartedAt string `json:"started_at,omitempty"`
	EndedAt   string `json:"ended_at,omitempty"`
}

// Run is the persisted workflow run.
type Run struct {
	ID         string      `json:"id"`
	Name       string      `json:"name"`
	Status     string      `json:"status"`
	CreatedAt  string      `json:"created_at,omitempty"`
	Definition *Definition `json:"definition"`
	NodeStates []NodeState `json:"node_states"`
}

// ParseDefinition decodes and validates a workflow definition.
func ParseDefinition(raw []byte) (*Definition, error) {
	var d Definition
	if err := json.Unmarshal(raw, &d); err != nil {
		return nil, agentderr.Wrap(agentderr.CodeInvalidInput, err,
			"workflow definition is not valid JSON",
			"see templates/software-dev.json for the shape")
	}
	if err := d.Validate(); err != nil {
		return nil, err
	}
	return &d, nil
}

// Validate enforces the DAG contract.
func (d *Definition) Validate() error {
	if d.Name == "" {
		return agentderr.InvalidInput("workflow name is required", `example: "software-dev"`)
	}
	if len(d.Nodes) == 0 {
		return agentderr.InvalidInput("workflow has no nodes", "a DAG needs at least one node")
	}
	ids := map[string]bool{}
	for i := range d.Nodes {
		n := &d.Nodes[i]
		if n.ID == "" {
			return agentderr.InvalidInput("every node needs an id", "the node's id is how depends_on and outputs reference it")
		}
		if ids[n.ID] {
			return agentderr.InvalidInput("duplicate node id "+n.ID, "node ids must be unique within a workflow")
		}
		ids[n.ID] = true
		if n.Agent == "" {
			return agentderr.InvalidInput("node "+n.ID+" has no agent", "bind each node to an agent id")
		}
		if n.Prompt == "" {
			return agentderr.InvalidInput("node "+n.ID+" has no prompt", "the prompt is the node's task")
		}
		if n.Harness == "" {
			n.Harness = "native"
		}
		if n.MaxRetries < 0 {
			return agentderr.InvalidInput("node "+n.ID+" max_retries must be >= 0", "0 = one attempt")
		}
	}
	// deps exist + no cycles (Kahn)
	indegrees := map[string]int{}
	for _, n := range d.Nodes {
		for _, dep := range n.DependsOn {
			if !ids[dep] {
				return agentderr.InvalidInput(
					"node "+n.ID+" depends on unknown node "+dep,
					"depends_on must reference node ids in this workflow")
			}
		}
	}
	for _, n := range d.Nodes {
		indegrees[n.ID] = len(d.DependsOnUnique(&n))
	}
	queue := []string{}
	for _, n := range d.Nodes {
		if indegrees[n.ID] == 0 {
			queue = append(queue, n.ID)
		}
	}
	seen := 0
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		seen++
		for _, n := range d.Nodes {
			for _, dep := range n.DependsOn {
				if dep == id {
					indegrees[n.ID]--
					if indegrees[n.ID] == 0 {
						queue = append(queue, n.ID)
					}
				}
			}
		}
	}
	if seen != len(d.Nodes) {
		return agentderr.InvalidInput(
			"workflow contains a dependency cycle",
			"topologically sort the nodes; a cycle can never run")
	}
	return nil
}

// DependsOnUnique returns the node's deps without duplicates.
func (d *Definition) DependsOnUnique(n *NodeDef) []string {
	seen := map[string]bool{}
	var out []string
	for _, dep := range n.DependsOn {
		if !seen[dep] {
			seen[dep] = true
			out = append(out, dep)
		}
	}
	return out
}

// RenderPrompt substitutes {{outputs.<node>}} and
// {{files.<node>.<path>}} with the upstream nodes' collected outputs.
// Unknown variables are left intact — visible in the trace, silent
// substitution would hide definition bugs.
func RenderPrompt(template string, outputs map[string]NodeOutput) string {
	out := template
	// deterministic substitution: longer keys first (files before outputs)
	var keys []string
	for node := range outputs {
		for path := range outputs[node].Files {
			keys = append(keys, fmt.Sprintf("{{files.%s.%s}}", node, path))
		}
		keys = append(keys, fmt.Sprintf("{{outputs.%s}}", node))
	}
	sort.Slice(keys, func(i, j int) bool { return len(keys[i]) > len(keys[j]) })
	for _, k := range keys {
		var val string
		if strings.HasPrefix(k, "{{files.") {
			rest := strings.TrimSuffix(strings.TrimPrefix(k, "{{files."), "}}")
			parts := strings.SplitN(rest, ".", 2)
			if len(parts) == 2 {
				val = outputs[parts[0]].Files[parts[1]]
			}
		} else {
			node := strings.TrimSuffix(strings.TrimPrefix(k, "{{outputs."), "}}")
			val = outputs[node].Text
		}
		out = strings.ReplaceAll(out, k, val)
	}
	return out
}

// NodeOutput is what one finished node contributes downstream.
type NodeOutput struct {
	Text  string            `json:"text"`
	Files map[string]string `json:"files,omitempty"` // path → content
}

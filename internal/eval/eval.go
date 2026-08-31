// Package eval is the v0 eval harness (M7): datasets of cases with
// deterministic rubrics, run through the SAME harness seam the product
// uses, scored reproducibly, and compared across agent versions.
// An eval run is a set of ordinary sessions — auditable traces, not a
// parallel simulator.
package eval

import (
	"encoding/json"

	"github.com/google/uuid"
)

// CriterionKind enumerates the deterministic rubric criteria.
type CriterionKind string

const (
	KindContains         CriterionKind = "contains"          // final text contains Arg
	KindNotContains      CriterionKind = "not_contains"      // final text must NOT contain Arg
	KindToolUsed         CriterionKind = "tool_used"         // tool named Arg was requested
	KindToolNotUsed      CriterionKind = "tool_not_used"     // tool named Arg never requested
	KindArtifactContains CriterionKind = "artifact_contains" // file Path contains Arg
	KindStopReason       CriterionKind = "stop_reason"       // turn ended with Arg
	KindMaxTurns         CriterionKind = "max_turns"         // assistant messages <= int(Arg)
)

// Criterion is one checkable expectation. Weight defaults to 1.
type Criterion struct {
	Kind   CriterionKind `json:"kind"`
	Arg    string        `json:"arg,omitempty"`
	Path   string        `json:"path,omitempty"`
	Weight float64       `json:"weight,omitempty"`
}

// Case is one eval input with its rubric.
type Case struct {
	ID      string      `json:"id"`
	Input   string      `json:"input"`
	Harness string      `json:"harness,omitempty"` // default native
	Rubric  []Criterion `json:"rubric"`
}

// Dataset is a set of cases — a JSON file, versioned in git.
type Dataset struct {
	Name  string `json:"name"`
	Cases []Case `json:"cases"`
}

// RunTrace is what a finished case leaves behind, projected from the
// event log plus the sandbox state.
type RunTrace struct {
	SessionID     uuid.UUID
	FinalText     string   // last assistant text block
	ToolsUsed     []string // names from tool.requested events
	StopReason    string
	AssistantMsgs int
}

// CriterionResult is one check's outcome with a human-readable reason.
type CriterionResult struct {
	Criterion Criterion `json:"criterion"`
	Pass      bool      `json:"pass"`
	Reason    string    `json:"reason"`
}

// CaseResult scores one case run.
type CaseResult struct {
	CaseID    string            `json:"case_id"`
	SessionID string            `json:"session_id"`
	Pass      bool              `json:"pass"`  // ALL criteria passed
	Score     float64           `json:"score"` // weighted fraction
	Results   []CriterionResult `json:"results"`
}

// VersionReport aggregates one agent version over the dataset.
type VersionReport struct {
	AgentID  string       `json:"agent_id"`
	Version  int          `json:"version"`
	Dataset  string       `json:"dataset"`
	Results  []CaseResult `json:"results"`
	Score    float64      `json:"score"` // mean case score
	PassRate float64      `json:"pass_rate"`
}

// ParseDataset decodes a dataset file body.
func ParseDataset(raw []byte) (*Dataset, error) {
	var d Dataset
	if err := json.Unmarshal(raw, &d); err != nil {
		return nil, err
	}
	if d.Name == "" {
		d.Name = "unnamed"
	}
	seen := map[string]bool{}
	for i := range d.Cases {
		c := &d.Cases[i]
		if c.ID == "" {
			c.ID = "case-" + string(rune('1'+i))
		}
		if seen[c.ID] {
			return nil, &duplicateCaseError{c.ID}
		}
		seen[c.ID] = true
		if c.Harness == "" {
			c.Harness = "native"
		}
		for j := range c.Rubric {
			if c.Rubric[j].Weight <= 0 {
				c.Rubric[j].Weight = 1
			}
		}
	}
	return &d, nil
}

type duplicateCaseError struct{ id string }

func (e *duplicateCaseError) Error() string { return "duplicate case id: " + e.id }

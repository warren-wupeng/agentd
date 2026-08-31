package eval

import (
	"fmt"
	"sort"
	"strings"
)

// Compare prints two version reports side by side with per-case
// criterion flips — the M7 done-when artifact: "two agent versions
// scored on the same dataset, diff printed".
func Compare(a, b *VersionReport) string {
	if a.AgentID != b.AgentID || a.Dataset != b.Dataset {
		return fmt.Sprintf("cannot compare: reports disagree on agent (%s vs %s) or dataset (%q vs %q)",
			a.AgentID, b.AgentID, a.Dataset, b.Dataset)
	}

	byID := map[string]CaseResult{}
	for _, res := range a.Results {
		byID[res.CaseID] = res
	}

	var out strings.Builder
	fmt.Fprintf(&out, "dataset: %s  agent: %s\n", a.Dataset, shortID(a.AgentID))
	fmt.Fprintf(&out, "cases: %d  (score = weighted criteria fraction, averaged)\n\n", len(a.Results))

	head := fmt.Sprintf("%-24s %-16s %-16s %s", "case", verdict(a.Version), verdict(b.Version), "delta")
	out.WriteString(head + "\n")
	out.WriteString(strings.Repeat("-", 72) + "\n")

	flips := 0
	for _, bRes := range sortedResults(b) {
		aRes, ok := byID[bRes.CaseID]
		if !ok {
			fmt.Fprintf(&out, "%-24s %-16s %-16s %s\n", bRes.CaseID, "(absent)", mark(bRes.Pass), "?")
			continue
		}
		delta := ""
		switch {
		case aRes.Pass && !bRes.Pass:
			flips++
			delta = "REGRESSION  " + firstFlip(aRes, bRes)
		case !aRes.Pass && bRes.Pass:
			flips++
			delta = "IMPROVEMENT  " + firstFlip(aRes, bRes)
		case aRes.Score != bRes.Score:
			delta = fmt.Sprintf("score %.2f → %.2f", aRes.Score, bRes.Score)
		}
		fmt.Fprintf(&out, "%-24s %-16s %-16s %s\n", bRes.CaseID, mark(aRes.Pass), mark(bRes.Pass), delta)
	}

	out.WriteString(strings.Repeat("-", 72) + "\n")
	fmt.Fprintf(&out, "%-24s score %.2f (%.0f%%)   score %.2f (%.0f%%)\n", "aggregate",
		a.Score, a.PassRate*100, b.Score, b.PassRate*100)
	fmt.Fprintf(&out, "%-24s %+0.2f score, %d case flip(s)\n", "delta", b.Score-a.Score, flips)
	return out.String()
}

// firstFlip names the first criterion whose pass state differs — the
// line an engineer actually reads.
func firstFlip(a, b CaseResult) string {
	bByCrit := map[string]bool{}
	for _, r := range b.Results {
		bByCrit[string(r.Criterion.Kind)+"|"+r.Criterion.Arg+r.Criterion.Path] = r.Pass
	}
	for _, r := range a.Results {
		key := string(r.Criterion.Kind) + "|" + r.Criterion.Arg + r.Criterion.Path
		if bp, ok := bByCrit[key]; ok && bp != r.Pass {
			dir := "now passing"
			if r.Pass {
				dir = "now failing"
			}
			return fmt.Sprintf("[%s %q %s]", r.Criterion.Kind, r.Criterion.Arg+r.Criterion.Path, dir)
		}
	}
	return ""
}

func sortedResults(r *VersionReport) []CaseResult {
	out := append([]CaseResult(nil), r.Results...)
	sort.Slice(out, func(i, j int) bool { return out[i].CaseID < out[j].CaseID })
	return out
}

func mark(pass bool) string {
	if pass {
		return "PASS"
	}
	return "FAIL"
}

func verdict(version int) string { return fmt.Sprintf("v%d", version) }

func shortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

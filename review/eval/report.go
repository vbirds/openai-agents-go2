package eval

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
)

// RenderText formats the report as a human-readable table with a summary.
func (r *Report) RenderText() string {
	var b strings.Builder

	fmt.Fprintf(&b, "%-28s %-4s %-6s %-18s %-7s %-9s %-8s %s\n",
		"CASE", "RUN", "PASS", "VERDICT", "NOISE", "TOKENS", "TIME", "DETAIL")
	for _, res := range r.Results {
		pass := "ok"
		if res.Err != "" {
			pass = "ERR"
		} else if !res.Pass {
			pass = "FAIL"
		}
		fmt.Fprintf(&b, "%-28s %-4d %-6s %-18s %-7d %-9d %-8s %s\n",
			res.Case, res.Run, pass, res.Verdict, res.Unexpected, res.Tokens,
			res.Duration.Round(10e6), detail(res))
	}

	fmt.Fprintf(&b, "\ncases: %d/%d passed | recall: %.0f%% | verdict accuracy: %.0f%% | noise findings: %d | tokens: %d | wall time: %s\n",
		r.Passed, r.Cases, r.Recall*100, r.VerdictAccuracy*100, r.Unexpected,
		r.TotalTokens, r.Duration.Round(10e6))
	return b.String()
}

// LoadReport reads a previously saved JSON report for baseline comparison.
func LoadReport(path string) (*Report, error) {
	data, err := os.ReadFile(path) //nolint:gosec // report path chosen by the caller
	if err != nil {
		return nil, fmt.Errorf("eval: reading baseline report: %w", err)
	}
	var r Report
	if err := json.Unmarshal(data, &r); err != nil {
		return nil, fmt.Errorf("eval: parsing baseline report %s: %w", path, err)
	}
	return &r, nil
}

// RenderComparison summarizes how the current report moved against a
// baseline: aggregate deltas plus per-case pass transitions.
func (r *Report) RenderComparison(baseline *Report) string {
	var b strings.Builder

	b.WriteString("vs baseline:\n")
	fmt.Fprintf(&b, "  recall:           %.0f%% -> %.0f%% (%+.0f pt)\n",
		baseline.Recall*100, r.Recall*100, (r.Recall-baseline.Recall)*100)
	fmt.Fprintf(&b, "  noise findings:   %d -> %d (%+d)\n",
		baseline.Unexpected, r.Unexpected, r.Unexpected-baseline.Unexpected)
	fmt.Fprintf(&b, "  verdict accuracy: %.0f%% -> %.0f%% (%+.0f pt)\n",
		baseline.VerdictAccuracy*100, r.VerdictAccuracy*100, (r.VerdictAccuracy-baseline.VerdictAccuracy)*100)
	fmt.Fprintf(&b, "  tokens:           %d -> %d (%+d)\n",
		baseline.TotalTokens, r.TotalTokens, r.TotalTokens-baseline.TotalTokens)

	basePass := casePassMap(baseline)
	currPass := casePassMap(r)
	var fixed, broken []string
	for name, pass := range currPass {
		old, known := basePass[name]
		switch {
		case !known:
			continue // new case; not a transition
		case pass && !old:
			fixed = append(fixed, name)
		case !pass && old:
			broken = append(broken, name)
		}
	}
	sort.Strings(fixed)
	sort.Strings(broken)
	if len(fixed) > 0 {
		fmt.Fprintf(&b, "  fixed:            %s\n", strings.Join(fixed, ", "))
	}
	if len(broken) > 0 {
		fmt.Fprintf(&b, "  REGRESSED:        %s\n", strings.Join(broken, ", "))
	}
	return b.String()
}

// casePassMap reduces a report to case -> all-runs-passed.
func casePassMap(r *Report) map[string]bool {
	out := map[string]bool{}
	for _, res := range r.Results {
		pass := res.Pass && res.Err == ""
		if seen, ok := out[res.Case]; ok {
			pass = pass && seen
		}
		out[res.Case] = pass
	}
	return out
}

func detail(res CaseResult) string {
	if res.Err != "" {
		return "error: " + res.Err
	}
	var parts []string
	if len(res.Missed) > 0 {
		parts = append(parts, "missed: "+strings.Join(res.Missed, ","))
	}
	if len(res.ForbiddenHits) > 0 {
		parts = append(parts, "forbidden: "+strings.Join(res.ForbiddenHits, ","))
	}
	if !res.VerdictOK {
		parts = append(parts, "wrong verdict")
	}
	if len(parts) == 0 && res.Pass {
		return fmt.Sprintf("matched %d/%d", len(res.Matched), res.Expected)
	}
	return strings.Join(parts, "; ")
}

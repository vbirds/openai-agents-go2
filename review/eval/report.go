package eval

import (
	"fmt"
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

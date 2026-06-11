package eval

import (
	"strings"

	"github.com/MitulShah1/openai-agents-go/review"
)

// lineTolerance is how far a finding's line range may sit outside the
// expected region and still match: review anchors are useful within a few
// lines, exact-to-the-line matching would punish correct findings.
const lineTolerance = 3

// noiseSeverity is the threshold at or above which an unmatched finding
// counts as noise. low/info findings are not scored as noise: they are
// cheap for a human to skip and penalizing them pushes reviewers to hide
// uncertainty instead of reporting it with low severity.
const noiseSeverity = review.SeverityMedium

// scoreCase evaluates a review result against the case expectations.
func scoreCase(c *Case, rev *review.Review) CaseResult {
	res := CaseResult{
		Case:     c.Name,
		Findings: len(rev.Findings),
		Verdict:  string(rev.Verdict),
	}

	// Verdict check.
	res.VerdictOK = true
	if len(c.Expect.Verdicts) > 0 {
		res.VerdictOK = false
		for _, v := range c.Expect.Verdicts {
			if strings.EqualFold(v, string(rev.Verdict)) {
				res.VerdictOK = true
			}
		}
	}

	// Expected findings: each expectation may be satisfied by any finding;
	// one finding may satisfy multiple expectations (they often overlap).
	matchedFinding := make([]bool, len(rev.Findings))
	for _, exp := range c.Expect.Findings {
		matched := false
		for i, f := range rev.Findings {
			if findingMatches(exp, f) {
				matched = true
				matchedFinding[i] = true
			}
		}
		if matched {
			res.Matched = append(res.Matched, exp.ID)
		} else {
			res.Missed = append(res.Missed, exp.ID)
		}
	}

	// Forbidden findings.
	for _, forbid := range c.Expect.Forbid {
		for _, f := range rev.Findings {
			if forbiddenMatches(forbid, f) {
				res.ForbiddenHits = append(res.ForbiddenHits, forbid.ID)
				break
			}
		}
	}

	// Noise: unmatched findings at or above the noise severity.
	for i, f := range rev.Findings {
		if !matchedFinding[i] && f.Severity.Rank() <= noiseSeverity.Rank() {
			res.Unexpected++
		}
	}

	res.Expected = len(c.Expect.Findings)
	res.Pass = len(res.Missed) == 0 &&
		len(res.ForbiddenHits) == 0 &&
		res.VerdictOK &&
		(!c.Expect.Clean || res.Unexpected == 0)
	return res
}

// findingMatches reports whether a finding satisfies an expectation; every
// specified field must match.
func findingMatches(exp ExpectedFinding, f review.Finding) bool {
	if exp.File != "" && !pathMatches(exp.File, f.File) {
		return false
	}
	if len(exp.Lines) == 2 && f.LineStart > 0 {
		lo, hi := exp.Lines[0]-lineTolerance, exp.Lines[1]+lineTolerance
		end := f.LineEnd
		if end < f.LineStart {
			end = f.LineStart
		}
		if f.LineStart > hi || end < lo {
			return false
		}
	}
	if len(exp.Categories) > 0 && !containsFold(exp.Categories, f.Category) {
		return false
	}
	if len(exp.Severities) > 0 && !containsFold(exp.Severities, string(f.Severity)) {
		return false
	}
	if len(exp.Keywords) > 0 && !keywordHit(exp.Keywords, f.Title+" "+f.Body) {
		return false
	}
	return true
}

// forbiddenMatches reports whether a finding violates a forbid rule.
func forbiddenMatches(forbid ForbiddenFinding, f review.Finding) bool {
	if forbid.MinSeverity != "" {
		threshold, err := review.ParseSeverity(forbid.MinSeverity)
		if err == nil && f.Severity.Rank() > threshold.Rank() {
			return false
		}
	}
	if forbid.File != "" && !pathMatches(forbid.File, f.File) {
		return false
	}
	if len(forbid.Keywords) > 0 && !keywordHit(forbid.Keywords, f.Title+" "+f.Body) {
		return false
	}
	return true
}

// pathMatches compares paths leniently: equal, or one is a path-boundary
// suffix of the other ("store.go" matches "internal/store/store.go").
func pathMatches(expected, actual string) bool {
	e := strings.TrimPrefix(strings.ToLower(expected), "./")
	a := strings.TrimPrefix(strings.ToLower(actual), "./")
	if e == a {
		return true
	}
	return strings.HasSuffix(a, "/"+e) || strings.HasSuffix(e, "/"+a)
}

func containsFold(haystack []string, needle string) bool {
	for _, h := range haystack {
		if strings.EqualFold(h, needle) {
			return true
		}
	}
	return false
}

func keywordHit(keywords []string, text string) bool {
	lower := strings.ToLower(text)
	for _, kw := range keywords {
		if kw != "" && strings.Contains(lower, strings.ToLower(kw)) {
			return true
		}
	}
	return false
}

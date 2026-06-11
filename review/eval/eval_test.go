package eval

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/MitulShah1/openai-agents-go/review"
)

// stubRunner returns canned reviews keyed by the eval_case metadata.
type stubRunner struct {
	reviews map[string]*review.Review
	err     map[string]error
}

func (s *stubRunner) Review(_ context.Context, req *review.Request) (*review.Response, error) {
	name := req.Metadata["eval_case"]
	if err := s.err[name]; err != nil {
		return nil, err
	}
	rev, ok := s.reviews[name]
	if !ok {
		return nil, errors.New("stubRunner: no review for case " + name)
	}
	raw, _ := json.Marshal(rev)
	return &review.Response{
		Output: raw,
		Review: rev,
		Usage:  review.Usage{TotalTokens: 100},
	}, nil
}

func corpusDir() string { return "testdata/cases" }

func TestLoadCasesCorpus(t *testing.T) {
	cases, err := LoadCases(corpusDir())
	if err != nil {
		t.Fatalf("LoadCases: %v", err)
	}
	if len(cases) != 5 {
		t.Fatalf("cases = %d, want 5", len(cases))
	}

	byName := map[string]Case{}
	for _, c := range cases {
		byName[c.Name] = c
		if c.Description == "" {
			t.Errorf("case %s has no description", c.Name)
		}
	}

	// Every shipped case must build a valid request.
	for _, c := range cases {
		req, err := c.BuildRequest(true)
		if err != nil {
			t.Errorf("case %s: BuildRequest: %v", c.Name, err)
			continue
		}
		if err := req.Validate(); err != nil {
			t.Errorf("case %s: invalid request: %v", c.Name, err)
		}
	}

	// The cross-file case must attach its workspace (and only when asked).
	cf := byName["cross-file-behavior"]
	req, _ := cf.BuildRequest(true)
	if req.WorkspaceRoot == "" {
		t.Error("cross-file-behavior must attach its workspace")
	}
	req, _ = cf.BuildRequest(false)
	if req.WorkspaceRoot != "" {
		t.Error("workspace must be omitted when disabled")
	}

	// The sql-injection case must load its nested file context.
	sqli := byName["sql-injection"]
	req, _ = sqli.BuildRequest(true)
	if len(req.Files) != 1 || req.Files[0].Path != "store/store.go" {
		t.Errorf("sql-injection files = %+v", req.Files)
	}
}

func TestScoreCaseMatching(t *testing.T) {
	c := &Case{
		Name: "t",
		Expect: Expectation{
			Verdicts: []string{"request_changes"},
			Findings: []ExpectedFinding{{
				ID: "sqli", File: "store/store.go", Lines: []int{8, 9},
				Categories: []string{"security"}, Severities: []string{"critical", "high"},
				Keywords: []string{"injection"},
			}},
		},
	}

	hit := review.Finding{
		Title: "SQL injection in UserByName", Body: "user input concatenated into SQL",
		Severity: review.SeverityHigh, Category: "security",
		File: "internal/store/store.go", LineStart: 10, LineEnd: 10, // within tolerance of [8,9]
	}
	noise := review.Finding{
		Title: "Variable naming", Body: "rename u", Severity: review.SeverityMedium,
		Category: "style", File: "store/store.go", LineStart: 1, LineEnd: 1,
	}
	lowNoise := review.Finding{
		Title: "Consider a comment", Body: "doc", Severity: review.SeverityInfo,
		Category: "documentation", File: "store/store.go",
	}

	res := scoreCase(c, &review.Review{
		Verdict:  review.VerdictRequestChanges,
		Findings: []review.Finding{hit, noise, lowNoise},
	})
	if !res.Pass {
		t.Errorf("expected pass, got %+v", res)
	}
	if len(res.Matched) != 1 || res.Matched[0] != "sqli" {
		t.Errorf("matched = %v", res.Matched)
	}
	// medium noise counts; info noise does not.
	if res.Unexpected != 1 {
		t.Errorf("unexpected = %d, want 1", res.Unexpected)
	}

	// Wrong verdict fails the case.
	res = scoreCase(c, &review.Review{Verdict: review.VerdictApprove, Findings: []review.Finding{hit}})
	if res.Pass || res.VerdictOK {
		t.Error("wrong verdict must fail")
	}

	// Out-of-range line anchor must not match.
	far := hit
	far.LineStart, far.LineEnd = 50, 52
	res = scoreCase(c, &review.Review{Verdict: review.VerdictRequestChanges, Findings: []review.Finding{far}})
	if res.Pass || len(res.Missed) != 1 {
		t.Errorf("far finding must miss, got %+v", res)
	}
}

func TestScoreCaseCleanAndForbidden(t *testing.T) {
	c := &Case{
		Name: "clean",
		Expect: Expectation{
			Clean:    true,
			Verdicts: []string{"approve", "comment"},
			Forbid: []ForbiddenFinding{{
				ID: "invented", Keywords: []string{"race"}, MinSeverity: "medium",
			}},
		},
	}

	// Clean review passes.
	res := scoreCase(c, &review.Review{Verdict: review.VerdictApprove})
	if !res.Pass {
		t.Errorf("clean review must pass: %+v", res)
	}

	// A medium finding fails a clean case as noise.
	res = scoreCase(c, &review.Review{Verdict: review.VerdictApprove, Findings: []review.Finding{
		{Title: "x", Severity: review.SeverityMedium, Category: "bug"},
	}})
	if res.Pass {
		t.Error("noise must fail a clean case")
	}

	// Forbidden keyword at sufficient severity trips the trap.
	res = scoreCase(c, &review.Review{Verdict: review.VerdictApprove, Findings: []review.Finding{
		{Title: "Possible data race", Severity: review.SeverityHigh, Category: "bug"},
	}})
	if len(res.ForbiddenHits) != 1 || res.ForbiddenHits[0] != "invented" {
		t.Errorf("forbidden hits = %v", res.ForbiddenHits)
	}

	// The same finding below the severity floor does not trip it (but still
	// counts as noise for the clean expectation).
	res = scoreCase(c, &review.Review{Verdict: review.VerdictApprove, Findings: []review.Finding{
		{Title: "Possible data race", Severity: review.SeverityLow, Category: "bug"},
	}})
	if len(res.ForbiddenHits) != 0 {
		t.Errorf("low-severity finding must not trip forbid, got %v", res.ForbiddenHits)
	}
}

func TestRunAggregation(t *testing.T) {
	cases, err := LoadCases(corpusDir())
	if err != nil {
		t.Fatal(err)
	}

	runner := &stubRunner{
		reviews: map[string]*review.Review{
			"sql-injection": {Verdict: review.VerdictRequestChanges, Findings: []review.Finding{{
				Title: "SQL injection", Body: "concatenated query", Severity: review.SeverityCritical,
				Category: "security", File: "store/store.go", LineStart: 8, LineEnd: 8,
			}}},
			"swallowed-error": {Verdict: review.VerdictRequestChanges, Findings: []review.Finding{{
				Title: "Nil dereference", Body: "error ignored, user may be nil",
				Severity: review.SeverityHigh, Category: "bug", File: "handler.go", LineStart: 10, LineEnd: 11,
			}}},
			"missing-unlock": {Verdict: review.VerdictApprove}, // miss: expected finding absent
			"clean-refactor": {Verdict: review.VerdictApprove},
			"cross-file-behavior": {Verdict: review.VerdictRequestChanges, Findings: []review.Finding{{
				Title: "Billing skips suspended users", Body: "GenerateInvoices in billing relies on inactive users",
				Severity: review.SeverityHigh, Category: "bug", File: "users/users.go", LineStart: 14, LineEnd: 16,
			}}},
		},
	}

	report, err := Run(context.Background(), runner, cases, Options{Runs: 1, Parallel: 3, UseWorkspace: true})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if report.Cases != 5 || report.Passed != 4 {
		t.Errorf("passed = %d/%d, want 4/5", report.Passed, report.Cases)
	}
	// 4 expectations total (clean-refactor has none), 3 matched.
	if report.Recall < 0.74 || report.Recall > 0.76 {
		t.Errorf("recall = %.2f, want 0.75", report.Recall)
	}
	if report.TotalTokens != 500 {
		t.Errorf("tokens = %d, want 500", report.TotalTokens)
	}

	text := report.RenderText()
	for _, want := range []string{"missing-unlock", "FAIL", "missed: missing-unlock", "4/5 passed", "recall: 75%"} {
		if !strings.Contains(text, want) {
			t.Errorf("report text missing %q:\n%s", want, text)
		}
	}
}

func TestRenderComparison(t *testing.T) {
	baseline := &Report{
		Recall: 0.5, Unexpected: 3, VerdictAccuracy: 0.8, TotalTokens: 1000,
		Results: []CaseResult{
			{Case: "a", Pass: true},
			{Case: "b", Pass: false},
			{Case: "c", Pass: true},
		},
	}
	current := &Report{
		Recall: 0.75, Unexpected: 1, VerdictAccuracy: 1.0, TotalTokens: 1400,
		Results: []CaseResult{
			{Case: "a", Pass: true},
			{Case: "b", Pass: true},       // fixed
			{Case: "c", Pass: false},      // regressed
			{Case: "newcase", Pass: true}, // new case: not a transition
		},
	}

	text := current.RenderComparison(baseline)
	for _, want := range []string{
		"recall:           50% -> 75% (+25 pt)",
		"noise findings:   3 -> 1 (-2)",
		"tokens:           1000 -> 1400 (+400)",
		"fixed:            b",
		"REGRESSED:        c",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("comparison missing %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "newcase") {
		t.Error("new cases must not appear as transitions")
	}
}

func TestLoadReportRoundTrip(t *testing.T) {
	rep := &Report{Recall: 0.9, Cases: 2, Passed: 2}
	data, err := json.Marshal(rep)
	if err != nil {
		t.Fatal(err)
	}
	path := t.TempDir() + "/report.json"
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadReport(path)
	if err != nil {
		t.Fatalf("LoadReport: %v", err)
	}
	if loaded.Recall != 0.9 || loaded.Cases != 2 {
		t.Errorf("round trip mismatch: %+v", loaded)
	}
	if _, err := LoadReport(t.TempDir() + "/missing.json"); err == nil {
		t.Error("missing report must error")
	}
}

func TestRunErrorIsolationAndFilter(t *testing.T) {
	cases, err := LoadCases(corpusDir())
	if err != nil {
		t.Fatal(err)
	}

	runner := &stubRunner{
		reviews: map[string]*review.Review{},
		err:     map[string]error{"sql-injection": errors.New("api down")},
	}
	report, err := Run(context.Background(), runner, cases, Options{Runs: 1, Parallel: 1, Filter: "sql-injection"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if report.Cases != 1 || report.Passed != 0 {
		t.Errorf("expected 0/1 passed, got %d/%d", report.Passed, report.Cases)
	}
	if !strings.Contains(report.RenderText(), "api down") {
		t.Error("case error must surface in the report")
	}

	if _, err := Run(context.Background(), runner, cases, Options{Filter: "no-such-case"}); err == nil {
		t.Error("empty filter result must error")
	}
}

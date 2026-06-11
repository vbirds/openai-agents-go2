package eval

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/MitulShah1/openai-agents-go/review"
)

// Runner reviews requests; *review.Reviewer implements it.
type Runner interface {
	Review(ctx context.Context, req *review.Request) (*review.Response, error)
}

// Options configure an evaluation run.
type Options struct {
	// UseWorkspace attaches each case's workspace/ directory when present
	// (default true via DefaultOptions).
	UseWorkspace bool
	// Runs repeats every case N times; expectations are scored per run and
	// a case passes only if all runs pass (catches flaky quality). Min 1.
	Runs int
	// Parallel caps concurrently running cases. Min 1.
	Parallel int
	// Filter limits the run to cases whose name contains the substring.
	Filter string
}

// DefaultOptions returns the standard evaluation options.
func DefaultOptions() Options {
	return Options{UseWorkspace: true, Runs: 1, Parallel: 2}
}

// CaseResult is the scored outcome of one case run.
type CaseResult struct {
	Case    string `json:"case"`
	Run     int    `json:"run"`
	Pass    bool   `json:"pass"`
	Err     string `json:"error,omitempty"`
	Verdict string `json:"verdict,omitempty"`

	Expected      int      `json:"expected"`
	Matched       []string `json:"matched,omitempty"`
	Missed        []string `json:"missed,omitempty"`
	ForbiddenHits []string `json:"forbidden_hits,omitempty"`
	Unexpected    int      `json:"unexpected"`
	VerdictOK     bool     `json:"verdict_ok"`
	Findings      int      `json:"findings"`

	Tokens     int           `json:"tokens"`
	Turns      int           `json:"turns"`
	ToolCalls  int           `json:"tool_calls"`
	DurationMS int64         `json:"duration_ms"`
	Duration   time.Duration `json:"-"`
}

// Report aggregates an evaluation run.
type Report struct {
	Results []CaseResult `json:"results"`

	Cases  int `json:"cases"`
	Passed int `json:"passed"`

	// Recall is matched expectations / total expectations across all runs.
	Recall float64 `json:"recall"`
	// Unexpected is the total count of noise findings across all runs.
	Unexpected int `json:"unexpected"`
	// VerdictAccuracy is the fraction of runs with an acceptable verdict.
	VerdictAccuracy float64 `json:"verdict_accuracy"`

	TotalTokens int           `json:"total_tokens"`
	Duration    time.Duration `json:"-"`
	DurationMS  int64         `json:"duration_ms"`
}

// Run executes the cases against the runner and aggregates a report.
func Run(ctx context.Context, runner Runner, cases []Case, opts Options) (*Report, error) {
	if opts.Runs < 1 {
		opts.Runs = 1
	}
	if opts.Parallel < 1 {
		opts.Parallel = 1
	}

	var selected []Case
	for _, c := range cases {
		if opts.Filter == "" || strings.Contains(c.Name, opts.Filter) {
			selected = append(selected, c)
		}
	}
	if len(selected) == 0 {
		return nil, fmt.Errorf("eval: no cases match filter %q", opts.Filter)
	}

	type job struct{ caseIdx, run int }
	jobs := make([]job, 0, len(selected)*opts.Runs)
	for i := range selected {
		for r := 1; r <= opts.Runs; r++ {
			jobs = append(jobs, job{caseIdx: i, run: r})
		}
	}

	start := time.Now()
	results := make([]CaseResult, len(jobs))
	sem := make(chan struct{}, opts.Parallel)
	var wg sync.WaitGroup
	for i, j := range jobs {
		wg.Add(1)
		go func(i int, j job) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			results[i] = runCase(ctx, runner, &selected[j.caseIdx], j.run, opts)
		}(i, j)
	}
	wg.Wait()

	sort.SliceStable(results, func(i, j int) bool {
		if results[i].Case != results[j].Case {
			return results[i].Case < results[j].Case
		}
		return results[i].Run < results[j].Run
	})
	return aggregate(results, time.Since(start)), nil
}

// runCase executes and scores a single case run.
func runCase(ctx context.Context, runner Runner, c *Case, run int, opts Options) CaseResult {
	req, err := c.BuildRequest(opts.UseWorkspace)
	if err != nil {
		return CaseResult{Case: c.Name, Run: run, Err: err.Error(), Expected: len(c.Expect.Findings)}
	}

	caseStart := time.Now()
	resp, err := runner.Review(ctx, req)
	if err != nil {
		return CaseResult{Case: c.Name, Run: run, Err: err.Error(),
			Expected: len(c.Expect.Findings), Duration: time.Since(caseStart)}
	}
	if resp.Review == nil {
		return CaseResult{Case: c.Name, Run: run,
			Err:      "response has no typed Review (evaluation requires the built-in schema)",
			Expected: len(c.Expect.Findings), Duration: time.Since(caseStart)}
	}

	res := scoreCase(c, resp.Review)
	res.Run = run
	res.Tokens = resp.Usage.TotalTokens
	res.Turns = resp.Turns
	res.ToolCalls = resp.ToolCalls
	res.Duration = time.Since(caseStart)
	res.DurationMS = res.Duration.Milliseconds()
	return res
}

func aggregate(results []CaseResult, elapsed time.Duration) *Report {
	rep := &Report{Results: results, Duration: elapsed, DurationMS: elapsed.Milliseconds()}

	casePassed := map[string]bool{}
	expectations, matched, verdictOK := 0, 0, 0
	for _, r := range results {
		pass := r.Pass && r.Err == ""
		if seen, ok := casePassed[r.Case]; !ok {
			casePassed[r.Case] = pass
		} else {
			casePassed[r.Case] = seen && pass
		}
		expectations += r.Expected
		matched += len(r.Matched)
		rep.Unexpected += r.Unexpected
		if r.VerdictOK && r.Err == "" {
			verdictOK++
		}
		rep.TotalTokens += r.Tokens
	}

	rep.Cases = len(casePassed)
	for _, pass := range casePassed {
		if pass {
			rep.Passed++
		}
	}
	if expectations > 0 {
		rep.Recall = float64(matched) / float64(expectations)
	} else {
		rep.Recall = 1
	}
	if len(results) > 0 {
		rep.VerdictAccuracy = float64(verdictOK) / float64(len(results))
	}
	return rep
}

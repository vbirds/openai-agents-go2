// Command codereview-eval runs the code review evaluation corpus against a
// reviewer configuration and reports recall, noise, and verdict accuracy.
// Use it to compare configurations (models, single vs deep review, prompt
// changes) and to catch quality regressions before shipping them.
//
//	# Evaluate the default single-agent reviewer
//	codereview-eval -cases review/eval/testdata/cases
//
//	# Compare: deep review with a cheaper triage model, 3 runs per case
//	codereview-eval -cases review/eval/testdata/cases -deep -triage-model gpt-4o-mini -runs 3
//
//	# CI quality gate
//	codereview-eval -cases review/eval/testdata/cases -fail-under 0.8 -out report.json
//
// Requires OPENAI_API_KEY. Exit codes: 0 success, 1 usage error, 2 run
// failure, 3 recall below -fail-under.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/MitulShah1/openai-agents-go/review"
	"github.com/MitulShah1/openai-agents-go/review/eval"
)

const (
	exitOK = iota
	exitUsage
	exitFailure
	exitBelowThreshold
)

func main() {
	os.Exit(run())
}

func run() int {
	fs := flag.NewFlagSet("codereview-eval", flag.ContinueOnError)
	casesDir := fs.String("cases", "review/eval/testdata/cases", "directory containing evaluation cases")
	filter := fs.String("case", "", "only run cases whose name contains this substring")
	runs := fs.Int("runs", 1, "repeat each case N times; a case passes only if all runs pass")
	parallel := fs.Int("parallel", 2, "cases evaluated concurrently")
	noWorkspace := fs.Bool("no-workspace", false, "ignore case workspace/ directories (disables exploration)")

	model := fs.String("model", os.Getenv("OPENAI_MODEL"), "model to evaluate (env OPENAI_MODEL)")
	baseURL := fs.String("base-url", "", "OpenAI-compatible API base URL (env OPENAI_BASE_URL)")
	deep := fs.Bool("deep", false, "evaluate deep-review mode (multi-agent pipeline)")
	triageModel := fs.String("triage-model", "", "triage model for deep mode")
	timeout := fs.Duration("timeout", 10*time.Minute, "per-case review timeout")

	out := fs.String("out", "", "write the JSON report to a file")
	baseline := fs.String("baseline", "", "compare against a previous JSON report (-out from an earlier run)")
	failUnder := fs.Float64("fail-under", 0, "exit 3 if recall falls below this fraction (0 disables)")
	verbose := fs.Bool("v", false, "verbose reviewer logging to stderr")

	if err := fs.Parse(os.Args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return exitOK
		}
		return exitUsage
	}
	if *failUnder < 0 || *failUnder > 1 {
		fmt.Fprintln(os.Stderr, "error: -fail-under must be within [0, 1]")
		return exitUsage
	}
	if os.Getenv("OPENAI_API_KEY") == "" && *baseURL == "" && os.Getenv("OPENAI_BASE_URL") == "" {
		fmt.Fprintln(os.Stderr, "error: OPENAI_API_KEY is not set")
		return exitUsage
	}

	cases, err := eval.LoadCases(*casesDir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return exitUsage
	}

	var ropts []review.Option
	if *model != "" {
		ropts = append(ropts, review.WithModel(*model))
	}
	if *baseURL != "" {
		ropts = append(ropts, review.WithBaseURL(*baseURL))
	}
	if *deep {
		ropts = append(ropts, review.WithDeepReview(true))
	}
	if *triageModel != "" {
		ropts = append(ropts, review.WithTriageModel(*triageModel))
	}
	ropts = append(ropts, review.WithTimeout(*timeout))
	if *verbose {
		ropts = append(ropts, review.WithLogger(slog.New(slog.NewTextHandler(os.Stderr, nil))))
	}
	reviewer, err := review.New(ropts...)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return exitUsage
	}

	opts := eval.DefaultOptions()
	opts.Runs = *runs
	opts.Parallel = *parallel
	opts.UseWorkspace = !*noWorkspace
	opts.Filter = *filter

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	report, err := eval.Run(ctx, reviewer, cases, opts)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return exitFailure
	}

	fmt.Print(report.RenderText())

	if *baseline != "" {
		base, err := eval.LoadReport(*baseline)
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			return exitFailure
		}
		fmt.Println()
		fmt.Print(report.RenderComparison(base))
	}

	if *out != "" {
		data, err := json.MarshalIndent(report, "", "  ")
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			return exitFailure
		}
		if err := os.WriteFile(*out, append(data, '\n'), 0o600); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			return exitFailure
		}
	}

	if *failUnder > 0 && report.Recall < *failUnder {
		fmt.Fprintf(os.Stderr, "recall %.0f%% is below the -fail-under threshold of %.0f%%\n",
			report.Recall*100, *failUnder*100)
		return exitBelowThreshold
	}
	return exitOK
}

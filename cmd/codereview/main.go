// Command codereview runs an AI code review over a diff and/or set of files
// and prints the result as JSON (or markdown) conforming to a JSON Schema.
//
// Examples:
//
//	# Review the last commit in the current git repository
//	codereview -git HEAD~1..HEAD
//
//	# Agentic review: let the reviewer explore the project (read-only) to
//	# verify callers, usages, and tests of the changed code
//	codereview -git HEAD~1..HEAD -workspace .
//
//	# Deep review: triage -> parallel specialist reviewers -> adjudicator;
//	# specialists are configurable via .codereview.yaml at the workspace root
//	codereview -git origin/main..HEAD -workspace . -deep
//
//	# Review uncommitted changes in an svn working copy, or a committed revision
//	codereview -svn wc
//	codereview -svn 12345
//
//	# Review a patch file with extra guidance, gate CI on high-severity findings
//	codereview -diff change.patch -prompt "Focus on concurrency" -fail-on high
//
//	# Review staged changes from stdin with a custom output schema
//	git diff --cached | codereview -diff - -schema schema.json -format json
//
// Authentication uses the OPENAI_API_KEY environment variable (and optional
// OPENAI_BASE_URL for OpenAI-compatible endpoints).
//
// Exit codes: 0 success, 1 usage error, 2 review failed, 3 findings at or
// above the -fail-on threshold.
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
	"path/filepath"
	"syscall"
	"time"

	"github.com/MitulShah1/openai-agents-go/review"
)

const (
	exitOK = iota
	exitUsage
	exitFailure
	exitFindings
)

const (
	formatJSON     = "json"
	formatMarkdown = "markdown"
)

type cliOptions struct {
	diffPath   string
	gitRange   string
	svnTarget  string
	prompt     string
	promptFile string
	schemaPath string
	workspace  string
	maxTurns   int
	deep       bool
	configPath string

	model           string
	baseURL         string
	temperature     float64
	tempSet         bool
	maxOutputTokens int
	timeout         time.Duration
	maxRetries      int
	maxInputKB      int

	outPath string
	format  string
	failOn  string
	verbose bool
}

func main() {
	os.Exit(run())
}

func run() int {
	opts, files, err := parseFlags(os.Args[1:])
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return exitOK
		}
		fmt.Fprintln(os.Stderr, "error:", err)
		return exitUsage
	}

	req, err := buildRequest(opts, files)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return exitUsage
	}

	reviewer, err := buildReviewer(opts)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return exitUsage
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	resp, err := reviewer.Review(ctx, req)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		var outErr *review.OutputError
		if errors.As(err, &outErr) && outErr.RawOutput != "" {
			fmt.Fprintln(os.Stderr, "last model output:")
			fmt.Fprintln(os.Stderr, outErr.RawOutput)
		}
		return exitFailure
	}

	for _, w := range resp.Warnings {
		fmt.Fprintln(os.Stderr, "warning:", w)
	}
	if opts.verbose {
		fmt.Fprintf(os.Stderr, "model=%s tokens=%d retries=%d turns=%d tool_calls=%d duration=%s\n",
			resp.Model, resp.Usage.TotalTokens, resp.Retries, resp.Turns, resp.ToolCalls,
			resp.Duration.Round(time.Millisecond))
		if len(resp.Specialists) > 0 {
			fmt.Fprintf(os.Stderr, "specialists=%v\n", resp.Specialists)
		}
	}

	if err := writeOutput(opts, resp); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return exitFailure
	}

	if opts.failOn != "" && resp.Review != nil {
		threshold, err := review.ParseSeverity(opts.failOn)
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			return exitUsage
		}
		if resp.Review.HasBlocking(threshold) {
			fmt.Fprintf(os.Stderr, "review found findings at or above severity %q\n", threshold)
			return exitFindings
		}
	}
	return exitOK
}

func parseFlags(args []string) (*cliOptions, []string, error) {
	opts := &cliOptions{}
	fs := flag.NewFlagSet("codereview", flag.ContinueOnError)
	fs.Usage = func() {
		_, _ = fmt.Fprintf(fs.Output(), `Usage: codereview [flags] [file ...]

Runs an AI code review over a diff and/or files and prints a JSON (or
markdown) report. Positional arguments are file paths included as full-file
context. Requires OPENAI_API_KEY (and optionally OPENAI_BASE_URL).

Flags:
`)
		fs.PrintDefaults()
	}

	fs.StringVar(&opts.diffPath, "diff", "", "path to a unified diff file, or '-' to read the diff from stdin")
	fs.StringVar(&opts.gitRange, "git", "", "git revision range to review (e.g. 'HEAD~1..HEAD', 'main..HEAD'); changed files are loaded automatically")
	fs.StringVar(&opts.svnTarget, "svn", "", "svn changes to review: 'wc' for working-copy changes, 'N:M' for a revision range, or a revision number; changed files are loaded automatically")
	fs.StringVar(&opts.prompt, "prompt", "", "extra review guidance for the model")
	fs.StringVar(&opts.promptFile, "prompt-file", "", "read review guidance from a file")
	fs.StringVar(&opts.schemaPath, "schema", "", "path to a custom output JSON Schema; default is the built-in review schema")
	fs.StringVar(&opts.workspace, "workspace", "", "enable agentic exploration: project root the reviewer may read (read-only) to inspect callers, usages, and tests; use '.' for the current directory")
	fs.IntVar(&opts.maxTurns, "max-turns", 0, "exploration turn budget (default 16; only with -workspace)")
	fs.BoolVar(&opts.deep, "deep", false, "deep review: triage routes the change to specialist reviewers running in parallel; an adjudicator verifies findings before the final result")
	fs.StringVar(&opts.configPath, "review-config", "", "path to a .codereview.yaml specialist configuration (default: auto-discovered at the -workspace root)")

	fs.StringVar(&opts.model, "model", envOr("OPENAI_MODEL", ""), "model to use (default gpt-4o; env OPENAI_MODEL)")
	fs.StringVar(&opts.baseURL, "base-url", "", "OpenAI-compatible API base URL (env OPENAI_BASE_URL)")
	fs.Float64Var(&opts.temperature, "temperature", 0, "sampling temperature (default: model default)")
	fs.IntVar(&opts.maxOutputTokens, "max-output-tokens", 0, "cap on response tokens (default: model default)")
	fs.DurationVar(&opts.timeout, "timeout", review.DefaultTimeout, "overall run timeout")
	fs.IntVar(&opts.maxRetries, "max-retries", review.DefaultMaxOutputRetries, "retries when output fails schema validation")
	fs.IntVar(&opts.maxInputKB, "max-input-kb", review.DefaultMaxInputBytes/1024, "input size budget in KiB; larger inputs are truncated")

	fs.StringVar(&opts.outPath, "out", "", "write the report to a file instead of stdout")
	fs.StringVar(&opts.format, "format", "json", "output format: json or markdown (markdown requires the built-in schema)")
	fs.StringVar(&opts.failOn, "fail-on", "", "exit 3 if findings exist at or above this severity (critical|high|medium|low|info); built-in schema only")
	fs.BoolVar(&opts.verbose, "v", false, "verbose logging to stderr")

	if err := fs.Parse(args); err != nil {
		return nil, nil, err
	}
	opts.tempSet = false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "temperature" {
			opts.tempSet = true
		}
	})

	if opts.format != formatJSON && opts.format != formatMarkdown {
		return nil, nil, fmt.Errorf("invalid -format %q (expected json or markdown)", opts.format)
	}
	if opts.format == formatMarkdown && opts.schemaPath != "" {
		return nil, nil, fmt.Errorf("-format markdown requires the built-in schema (remove -schema)")
	}
	if opts.failOn != "" && opts.schemaPath != "" {
		return nil, nil, fmt.Errorf("-fail-on requires the built-in schema (remove -schema)")
	}
	sources := 0
	for _, set := range []bool{opts.diffPath != "", opts.gitRange != "", opts.svnTarget != ""} {
		if set {
			sources++
		}
	}
	if sources > 1 {
		return nil, nil, fmt.Errorf("-diff, -git, and -svn are mutually exclusive")
	}
	if opts.maxTurns < 0 {
		return nil, nil, fmt.Errorf("-max-turns must be positive")
	}
	if opts.maxTurns > 0 && opts.workspace == "" {
		return nil, nil, fmt.Errorf("-max-turns requires -workspace")
	}
	if opts.configPath != "" && !opts.deep {
		return nil, nil, fmt.Errorf("-review-config requires -deep")
	}
	if opts.prompt != "" && opts.promptFile != "" {
		return nil, nil, fmt.Errorf("-prompt and -prompt-file are mutually exclusive")
	}
	return opts, fs.Args(), nil
}

func buildReviewer(opts *cliOptions) (*review.Reviewer, error) {
	if os.Getenv("OPENAI_API_KEY") == "" && opts.baseURL == "" && os.Getenv("OPENAI_BASE_URL") == "" {
		return nil, fmt.Errorf("OPENAI_API_KEY is not set")
	}

	var ropts []review.Option
	if opts.model != "" {
		ropts = append(ropts, review.WithModel(opts.model))
	}
	if opts.baseURL != "" {
		ropts = append(ropts, review.WithBaseURL(opts.baseURL))
	}
	if opts.tempSet {
		ropts = append(ropts, review.WithTemperature(opts.temperature))
	}
	if opts.maxOutputTokens > 0 {
		ropts = append(ropts, review.WithMaxOutputTokens(opts.maxOutputTokens))
	}
	if opts.maxTurns > 0 {
		ropts = append(ropts, review.WithMaxTurns(opts.maxTurns))
	}
	if opts.deep {
		ropts = append(ropts, review.WithDeepReview(true))
		if path := discoverReviewConfig(opts); path != "" {
			fmt.Fprintf(os.Stderr, "using review config %s\n", path)
			ropts = append(ropts, review.WithConfigFile(path))
		}
	}
	ropts = append(ropts,
		review.WithTimeout(opts.timeout),
		review.WithMaxOutputRetries(opts.maxRetries),
		review.WithMaxInputBytes(opts.maxInputKB*1024),
	)
	if opts.verbose {
		ropts = append(ropts, review.WithLogger(slog.New(slog.NewTextHandler(os.Stderr, nil))))
	}
	return review.New(ropts...)
}

func writeOutput(opts *cliOptions, resp *review.Response) error {
	var out []byte
	switch opts.format {
	case formatMarkdown:
		out = []byte(renderMarkdown(resp))
	default:
		pretty, err := json.MarshalIndent(resp.Output, "", "  ")
		if err != nil {
			return fmt.Errorf("formatting output: %w", err)
		}
		out = pretty
		out = append(out, '\n')
	}

	if opts.outPath == "" || opts.outPath == "-" {
		_, err := os.Stdout.Write(out)
		return err
	}
	if err := os.WriteFile(opts.outPath, out, 0o600); err != nil {
		return fmt.Errorf("writing %s: %w", opts.outPath, err)
	}
	return nil
}

// discoverReviewConfig returns the explicit -review-config path, or the
// .codereview.yaml at the workspace root when one exists.
func discoverReviewConfig(opts *cliOptions) string {
	if opts.configPath != "" {
		return opts.configPath
	}
	if opts.workspace == "" {
		return ""
	}
	candidate := filepath.Join(opts.workspace, review.ConfigFileName)
	if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
		return candidate
	}
	return ""
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

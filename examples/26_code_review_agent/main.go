// Package main demonstrates a minimal-but-real Code Review agent built with the
// OpenAI Agents Go SDK.
//
// What it shows:
//   - Structured review output (ReviewReport) via jsonschema ResponseFormat
//   - Tools to read files and inspect the changed-file set from a unified diff
//   - Automatic loading of the REVIEWED repo's convention files (AGENT.md,
//     CONTRIBUTING.md, ...) so the reviewer enforces project rules. The SDK does
//     NOT auto-load these — we load them explicitly and inject them.
//   - Input guardrails (committed-secrets detection + prompt-injection defense),
//     because PR/diff content is untrusted external input.
//   - Hallucination filtering: findings pointing at files outside the diff are
//     dropped, and a CI-friendly exit code is derived from the verdict.
//
// Usage:
//
//	export OPENAI_API_KEY=...
//	go run ./examples/26_code_review_agent -repo /path/to/repo -diff change.diff
//	# or pipe the diff in:
//	git diff main... | go run ./examples/26_code_review_agent -repo .
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"

	agents "github.com/MitulShah1/openai-agents-go"
	"github.com/MitulShah1/openai-agents-go/guardrail"
	"github.com/MitulShah1/openai-agents-go/guardrail/moderation"
	"github.com/MitulShah1/openai-agents-go/guardrail/security"
	"github.com/MitulShah1/openai-agents-go/jsonschema"
	"github.com/MitulShah1/openai-agents-go/tools"
)

// Finding is a single review comment.
type Finding struct {
	File       string `json:"file"`
	Line       int    `json:"line"`
	Severity   string `json:"severity"` // critical|high|medium|low|nit
	Category   string `json:"category"` // correctness|security|performance|style
	Title      string `json:"title"`
	Detail     string `json:"detail"`
	Suggestion string `json:"suggestion"`
}

// ReviewReport is the structured output the reviewer must produce.
type ReviewReport struct {
	Summary  string    `json:"summary"`
	Verdict  string    `json:"verdict"` // approve|comment|request_changes
	Findings []Finding `json:"findings"`
}

// conventionFiles is the ordered set of repo convention files we look for.
// First-match-wins per name; all existing ones are concatenated.
var conventionFiles = []string{
	"AGENT.md",
	"AGENTS.md",
	"CLAUDE.md",
	"CONTRIBUTING.md",
	".cursorrules",
	"docs/CODE_REVIEW.md",
}

// maxConventionBytes caps how much convention text we inline into the prompt to
// control token cost. The full text is always reachable via the read_conventions
// tool if it gets truncated here.
const maxConventionBytes = 12 * 1024

func main() {
	var repoRoot, diffPath, model string
	flag.StringVar(&repoRoot, "repo", ".", "path to the repository being reviewed")
	flag.StringVar(&diffPath, "diff", "", "path to a unified diff file (defaults to stdin)")
	flag.StringVar(&model, "model", openai.ChatModelGPT4o, "OpenAI model to use")
	flag.Parse()

	apiKey := os.Getenv("OPENAI_API_KEY")
	if apiKey == "" {
		fmt.Println("Please set OPENAI_API_KEY environment variable.")
		return
	}

	diffText, err := readDiff(diffPath)
	if err != nil {
		fmt.Printf("Error reading diff: %v\n", err)
		os.Exit(2)
	}
	if strings.TrimSpace(diffText) == "" {
		fmt.Println("Empty diff — nothing to review.")
		return
	}

	changed := changedFiles(diffText)
	conventions, conventionSrc, truncated := loadConventions(repoRoot)

	client := openai.NewClient(option.WithAPIKey(apiKey))
	runner := agents.NewRunner(&client)

	agent := agents.NewAgent("CodeReviewer")
	agent.Model = model
	// Inject conventions directly into the instructions (Approach A): this
	// guarantees the rules take effect on every run, unlike a tool the model
	// might forget to call.
	agent.Instructions = buildInstructions(conventions)
	agent.Tools = []tools.Tool{
		readFileTool(repoRoot),
		listChangedFilesTool(changed),
		readConventionsTool(repoRoot),
	}
	agent.ResponseFormat = jsonschema.
		JSONSchema("review_report", reviewReportSchema()).
		// Optional fields (suggestion, line) are incompatible with OpenAI strict
		// mode, so we disable it here.
		WithStrict(false)
	agent.InputGuardrails = []*guardrail.Guardrail{
		secretsGuardrail(),
		injectionGuardrail(),
	}

	fmt.Printf("Reviewing %d changed file(s).\n", len(changed))
	if conventionSrc != "" {
		note := conventionSrc
		if truncated {
			note += " (truncated)"
		}
		fmt.Printf("Loaded conventions from: %s\n", note)
	} else {
		fmt.Println("No convention files found — reviewing with defaults only.")
	}
	fmt.Println(strings.Repeat("-", 60))

	messages := []openai.ChatCompletionMessageParamUnion{
		openai.UserMessage("Review the following unified diff. Use read_file to " +
			"fetch full context before judging.\n\n<diff>\n" + diffText + "\n</diff>"),
	}

	result, err := runner.Run(context.Background(), agent, messages)
	if err != nil {
		fmt.Printf("Review run failed: %v\n", err)
		os.Exit(2)
	}

	var report ReviewReport
	if err := json.Unmarshal([]byte(result.FinalOutput), &report); err != nil {
		fmt.Printf("Could not parse structured report: %v\n\nRaw output:\n%s\n", err, result.FinalOutput)
		os.Exit(2)
	}

	report.Findings = filterFindings(report.Findings, changed)
	printReport(report, result.Usage.TotalTokens)

	os.Exit(exitCode(report))
}

// buildInstructions returns a dynamic instruction function that always includes
// the loaded project conventions.
func buildInstructions(conventions string) func(context.Context) string {
	base := `You are a senior software engineer performing a precise code review.

Rules:
- Review ONLY the changes in the provided diff, but call read_file to load the
  full surrounding context before judging — the diff alone hides bugs.
- Report only real, actionable issues. Do NOT invent problems to seem thorough.
- Map every finding to a real file and a real line number that exists in the diff.
- Prefer fewer, high-signal findings. Demote trivia to "nit" severity.
- Respect the project conventions below; flag violations of them explicitly.
- Set verdict to "request_changes" if any critical/high issue exists, otherwise
  "comment" for medium/low, or "approve" if the change is clean.`

	return func(_ context.Context) string {
		if strings.TrimSpace(conventions) == "" {
			return base + "\n\n(No project convention files were found.)"
		}
		return base + "\n\n## Project conventions (MUST enforce)\n" + conventions +
			"\n\nIf the convention text was truncated, call read_conventions for the full version."
	}
}

// loadConventions reads the reviewed repo's convention files and concatenates
// them, capped at maxConventionBytes. Returns the text, a human-readable source
// list, and whether the text was truncated.
func loadConventions(repoRoot string) (text, sources string, truncated bool) {
	var b strings.Builder
	var found []string

	for _, name := range conventionFiles {
		data, err := os.ReadFile(filepath.Join(repoRoot, name)) //nolint:gosec // fixed convention filenames under the operator-provided repo root
		if err != nil {
			continue
		}
		found = append(found, name)
		fmt.Fprintf(&b, "\n### %s\n%s\n", name, strings.TrimSpace(string(data)))
	}

	out := b.String()
	if len(out) > maxConventionBytes {
		out = out[:maxConventionBytes]
		truncated = true
	}
	return strings.TrimSpace(out), strings.Join(found, ", "), truncated
}

// --- Tools -----------------------------------------------------------------

func readFileTool(repoRoot string) tools.Tool {
	return tools.FromFunc("read_file",
		"Read the full current content of a file in the reviewed repo, with line numbers.",
		func(args struct {
			Path string `json:"path" jsonschema:"description=Repo-relative path of the file to read"`
		}) (any, error) {
			clean := filepath.Clean(args.Path)
			if strings.HasPrefix(clean, "..") || filepath.IsAbs(clean) {
				return nil, fmt.Errorf("refusing to read path outside the repo: %s", args.Path)
			}
			data, err := os.ReadFile(filepath.Join(repoRoot, clean)) //nolint:gosec // path is cleaned and confined to the repo root above
			if err != nil {
				return nil, err
			}
			return withLineNumbers(string(data)), nil
		})
}

func listChangedFilesTool(changed []string) tools.Tool {
	return tools.FromFunc("list_changed_files",
		"List the files changed in this diff. Findings must target one of these files.",
		func(_ struct{}) (any, error) {
			return changed, nil
		})
}

func readConventionsTool(repoRoot string) tools.Tool {
	return tools.FromFunc("read_conventions",
		"Read the reviewed repo's full coding-convention files (AGENT.md, CONTRIBUTING.md, ...).",
		func(_ struct{}) (any, error) {
			text, sources, _ := loadConventions(repoRoot)
			if text == "" {
				return "No convention files found in this repository.", nil
			}
			return "Sources: " + sources + "\n" + text, nil
		})
}

// --- Guardrails ------------------------------------------------------------

func secretsGuardrail() *guardrail.Guardrail {
	// Tripwire halts the run if a diff appears to commit credentials.
	return security.NewSecrets(security.SecretsConfig{Tripwire: true})
}

// injectionGuardrail adapts moderation.PromptInjectionGuardrail (which exposes
// Validate(ctx, input) error) to the *guardrail.Guardrail shape required by
// agent.InputGuardrails.
func injectionGuardrail() *guardrail.Guardrail {
	inj := moderation.NewInjection(moderation.PromptInjectionConfig{Tripwire: true})
	return guardrail.NewGuardrail("prompt_injection", func(ctx context.Context, input string) (*guardrail.Result, error) {
		if err := inj.Validate(ctx, input); err != nil {
			return &guardrail.Result{
				Passed:            false,
				TripwireTriggered: inj.IsTripwire(),
				Message:           err.Error(),
			}, nil
		}
		return &guardrail.Result{Passed: true, Message: "no prompt injection detected"}, nil
	})
}

// --- Schema ----------------------------------------------------------------

func reviewReportSchema() *jsonschema.Schema {
	finding := jsonschema.Object().
		WithProperty("file", jsonschema.String().WithDescription("Repo-relative file path")).
		WithProperty("line", jsonschema.Integer().WithDescription("Line number in the changed file")).
		WithProperty("severity", jsonschema.String().WithEnum("critical", "high", "medium", "low", "nit")).
		WithProperty("category", jsonschema.String().WithEnum("correctness", "security", "performance", "style")).
		WithProperty("title", jsonschema.String().WithDescription("One-line summary of the issue")).
		WithProperty("detail", jsonschema.String().WithDescription("Why it is a problem")).
		WithProperty("suggestion", jsonschema.String().WithDescription("How to fix it (text, not an auto-applied patch)")).
		WithRequired("file", "line", "severity", "category", "title", "detail")

	return jsonschema.Object().
		WithProperty("summary", jsonschema.String().WithDescription("Overall summary of the change")).
		WithProperty("verdict", jsonschema.String().WithEnum("approve", "comment", "request_changes")).
		WithProperty("findings", jsonschema.Array(finding)).
		WithRequired("summary", "verdict", "findings")
}

// --- Diff helpers ----------------------------------------------------------

func readDiff(path string) (string, error) {
	if path != "" {
		data, err := os.ReadFile(path) //nolint:gosec // diff path is supplied by the operator via the -diff flag
		return string(data), err
	}
	data, err := io.ReadAll(os.Stdin)
	return string(data), err
}

// changedFiles extracts the post-change file paths from a unified diff
// (lines like "+++ b/path/to/file").
func changedFiles(diff string) []string {
	seen := map[string]bool{}
	var files []string
	for _, line := range strings.Split(diff, "\n") {
		if !strings.HasPrefix(line, "+++ ") {
			continue
		}
		p := strings.TrimSpace(strings.TrimPrefix(line, "+++ "))
		p = strings.TrimPrefix(p, "b/")
		if p == "" || p == "/dev/null" || seen[p] {
			continue
		}
		seen[p] = true
		files = append(files, p)
	}
	return files
}

func withLineNumbers(content string) string {
	var b strings.Builder
	for i, line := range strings.Split(content, "\n") {
		fmt.Fprintf(&b, "%d\t%s\n", i+1, line)
	}
	return b.String()
}

// filterFindings drops findings that point at files not present in the diff —
// a cheap defense against hallucinated locations.
func filterFindings(findings []Finding, changed []string) []Finding {
	set := map[string]bool{}
	for _, f := range changed {
		set[f] = true
	}
	kept := findings[:0]
	for _, f := range findings {
		if set[f.File] {
			kept = append(kept, f)
		}
	}
	return kept
}

// --- Output ----------------------------------------------------------------

func printReport(r ReviewReport, tokens int) {
	fmt.Printf("Verdict: %s\n", strings.ToUpper(r.Verdict))
	fmt.Printf("Summary: %s\n\n", r.Summary)

	if len(r.Findings) == 0 {
		fmt.Println("No actionable findings.")
	}
	for i, f := range r.Findings {
		fmt.Printf("%d. [%s/%s] %s:%d\n   %s\n", i+1,
			strings.ToUpper(f.Severity), f.Category, f.File, f.Line, f.Title)
		if f.Detail != "" {
			fmt.Printf("   %s\n", f.Detail)
		}
		if f.Suggestion != "" {
			fmt.Printf("   Suggestion: %s\n", f.Suggestion)
		}
		fmt.Println()
	}
	fmt.Printf("%s\nTokens used: %d\n", strings.Repeat("-", 60), tokens)
}

// exitCode maps the review verdict to a CI-friendly process exit code.
func exitCode(r ReviewReport) int {
	if r.Verdict == "request_changes" {
		return 1
	}
	for _, f := range r.Findings {
		if f.Severity == "critical" || f.Severity == "high" {
			return 1
		}
	}
	return 0
}

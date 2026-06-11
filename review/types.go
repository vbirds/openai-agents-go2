package review

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// File is a source file given to the reviewer as context.
type File struct {
	// Path is the display path of the file (e.g. "internal/server/http.go").
	// It is what the model will reference in finding locations.
	Path string `json:"path"`

	// Content is the full file content.
	Content string `json:"content"`

	// Language optionally names the language for syntax hints (e.g. "go").
	// If empty it is inferred from the path extension where possible.
	Language string `json:"language,omitempty"`
}

// Request describes a single code review job.
type Request struct {
	// Prompt is reviewer guidance for this run, e.g. "focus on SQL injection"
	// or team-specific conventions. Optional.
	Prompt string

	// Diff is a unified diff (git diff / patch format) describing the change
	// under review. Optional if Files is set, but strongly recommended:
	// the diff is what the reviewer is asked to judge.
	Diff string

	// Files are full file contents providing context around the diff
	// (typically the post-change versions of the files touched by the diff).
	// Optional.
	Files []File

	// Schema is an optional raw JSON Schema document describing the desired
	// output shape. When nil, the built-in review schema is used and
	// Response.Review is populated.
	Schema json.RawMessage

	// SchemaName names the schema for the structured-outputs API.
	// Defaults to "code_review" when empty.
	SchemaName string

	// Metadata is arbitrary contextual information rendered into the prompt,
	// e.g. {"repository": "acme/api", "pull_request": "#42"}. Optional.
	Metadata map[string]string

	// WorkspaceRoot enables agentic exploration: when set to a project
	// directory, the reviewer gets read-only tools (read_file, grep,
	// list_dir) sandboxed to that directory and will autonomously inspect
	// callers, usages, and tests of the changed code before judging it.
	// Leave empty for single-shot review of the provided inputs only.
	WorkspaceRoot string

	// Specialists overrides the configured specialist roster for this
	// request only (deep-review mode). Empty uses the Reviewer's roster.
	Specialists []Specialist
}

// Validate checks that the request contains enough material to review.
func (r *Request) Validate() error {
	if r == nil {
		return fmt.Errorf("%w: request is nil", ErrInvalidRequest)
	}
	if strings.TrimSpace(r.Diff) == "" && len(r.Files) == 0 {
		return fmt.Errorf("%w: at least one of Diff or Files must be provided", ErrInvalidRequest)
	}
	for i, f := range r.Files {
		if strings.TrimSpace(f.Path) == "" {
			return fmt.Errorf("%w: files[%d] has an empty path", ErrInvalidRequest, i)
		}
	}
	if r.Schema != nil && !json.Valid(r.Schema) {
		return fmt.Errorf("%w: schema is not valid JSON", ErrInvalidRequest)
	}
	for i := range r.Specialists {
		if err := r.Specialists[i].Validate(); err != nil {
			return fmt.Errorf("%w: specialists[%d]: %v", ErrInvalidRequest, i, err)
		}
	}
	return nil
}

// Usage reports token consumption for a review run, accumulated across
// retries.
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// Response is the result of a review run.
type Response struct {
	// Output is the raw JSON produced by the model. It is guaranteed to be
	// valid JSON that validates against the requested schema.
	Output json.RawMessage `json:"output"`

	// Review is the typed result, populated only when the built-in schema
	// was used (Request.Schema == nil).
	Review *Review `json:"review,omitempty"`

	// Model is the model that produced the review.
	Model string `json:"model"`

	// Usage is the total token usage, including any validation retries.
	Usage Usage `json:"usage"`

	// Duration is the wall-clock time of the run.
	Duration time.Duration `json:"duration"`

	// Retries is the number of corrective retries that were needed before
	// the output validated against the schema.
	Retries int `json:"retries"`

	// Turns is the total number of agent loop iterations across all
	// attempts. Greater than 1 indicates the reviewer explored the
	// workspace with tools before answering.
	Turns int `json:"turns"`

	// ToolCalls is the total number of exploration tool invocations.
	ToolCalls int `json:"tool_calls"`

	// Specialists lists the specialist reviewers that ran, in deep-review
	// mode. Nil for single-agent reviews.
	Specialists []string `json:"specialists,omitempty"`

	// Warnings lists non-fatal conditions encountered during the run, such
	// as input truncation or fallback to prompt-enforced schema mode.
	Warnings []string `json:"warnings,omitempty"`
}

// Severity classifies how important a finding is.
type Severity string

// Severity levels, from most to least important.
const (
	SeverityCritical Severity = "critical"
	SeverityHigh     Severity = "high"
	SeverityMedium   Severity = "medium"
	SeverityLow      Severity = "low"
	SeverityInfo     Severity = "info"
)

// Rank returns a sortable rank for the severity; lower is more severe.
// Unknown severities rank last.
func (s Severity) Rank() int {
	switch s {
	case SeverityCritical:
		return 0
	case SeverityHigh:
		return 1
	case SeverityMedium:
		return 2
	case SeverityLow:
		return 3
	case SeverityInfo:
		return 4
	default:
		return 5
	}
}

// ParseSeverity converts a string into a known Severity.
func ParseSeverity(s string) (Severity, error) {
	sev := Severity(strings.ToLower(strings.TrimSpace(s)))
	switch sev {
	case SeverityCritical, SeverityHigh, SeverityMedium, SeverityLow, SeverityInfo:
		return sev, nil
	default:
		return "", fmt.Errorf("unknown severity %q (expected critical|high|medium|low|info)", s)
	}
}

// Verdict is the overall recommendation of the review.
type Verdict string

// Possible review verdicts.
const (
	VerdictApprove        Verdict = "approve"
	VerdictRequestChanges Verdict = "request_changes"
	VerdictComment        Verdict = "comment"
)

// Review is the typed form of the built-in result schema.
type Review struct {
	// Summary is a short overall assessment of the change.
	Summary string `json:"summary"`

	// Verdict is the overall recommendation.
	Verdict Verdict `json:"verdict"`

	// Confidence is the model's confidence in the verdict, in [0, 1].
	Confidence float64 `json:"confidence"`

	// Findings are the individual review comments, ordered by severity.
	Findings []Finding `json:"findings"`
}

// HasBlocking reports whether the review contains a finding at or above the
// given severity threshold.
func (r *Review) HasBlocking(threshold Severity) bool {
	for _, f := range r.Findings {
		if f.Severity.Rank() <= threshold.Rank() {
			return true
		}
	}
	return false
}

// Finding is a single review comment anchored to a code location.
type Finding struct {
	// Title is a one-line summary of the issue.
	Title string `json:"title"`

	// Body explains the issue and why it matters.
	Body string `json:"body"`

	// Severity classifies the importance of the issue.
	Severity Severity `json:"severity"`

	// Category classifies the kind of issue (bug, security, performance,
	// style, maintainability, testing, documentation, other).
	Category string `json:"category"`

	// Confidence is the model's confidence in the finding, in [0, 1].
	Confidence float64 `json:"confidence"`

	// File is the path of the affected file, matching a path from the
	// request's diff or files.
	File string `json:"file"`

	// LineStart and LineEnd delimit the affected lines in the post-change
	// version of the file (1-based, inclusive). Zero when not applicable.
	LineStart int `json:"line_start"`
	LineEnd   int `json:"line_end"`

	// Suggestion is a concrete proposed fix; empty when there is none.
	Suggestion string `json:"suggestion"`
}

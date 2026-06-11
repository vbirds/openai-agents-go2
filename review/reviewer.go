package review

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/openai/openai-go/v3"

	agents "github.com/MitulShah1/openai-agents-go"
	libjs "github.com/MitulShah1/openai-agents-go/jsonschema"
)

// Reviewer runs code reviews. It is safe for concurrent use; each Review
// call is independent.
type Reviewer struct {
	runner *agents.Runner
	cfg    config
}

// New creates a Reviewer. Without options it reads OPENAI_API_KEY (and
// OPENAI_BASE_URL) from the environment and uses gpt-4o.
func New(opts ...Option) (*Reviewer, error) {
	cfg := config{
		model:            DefaultModel,
		timeout:          DefaultTimeout,
		maxOutputRetries: DefaultMaxOutputRetries,
		maxInputBytes:    DefaultMaxInputBytes,
		specialists:      BuiltinSpecialists(),
	}
	for _, opt := range opts {
		if err := opt(&cfg); err != nil {
			return nil, err
		}
	}
	if cfg.logger == nil {
		cfg.logger = slog.New(slog.DiscardHandler)
	}
	provider, err := cfg.buildProvider()
	if err != nil {
		return nil, fmt.Errorf("review: building model provider: %w", err)
	}
	return &Reviewer{
		runner: agents.NewRunnerWithProvider(provider),
		cfg:    cfg,
	}, nil
}

// Review runs a code review and returns output conforming to the requested
// schema. The returned Response.Output always validates against the schema;
// if the model cannot produce conforming output within the configured
// retries, an error wrapping ErrInvalidOutput is returned.
//
// In deep-review mode (WithDeepReview) the request is routed through a
// multi-agent pipeline: triage selects specialist reviewers that run in
// parallel, and an adjudicator verifies their candidate findings against
// the code before synthesizing the final result.
func (rv *Reviewer) Review(ctx context.Context, req *Request) (*Response, error) {
	start := time.Now()

	if err := req.Validate(); err != nil {
		return nil, err
	}

	rawSchema := req.Schema
	usingDefaultSchema := rawSchema == nil
	if usingDefaultSchema {
		rawSchema = DefaultSchema()
	}
	schema, err := compileSchema(rawSchema, req.SchemaName)
	if err != nil {
		return nil, err
	}

	var ws *workspace
	if req.WorkspaceRoot != "" {
		if ws, err = newWorkspace(req.WorkspaceRoot); err != nil {
			return nil, err
		}
	}

	// Teach the reviewer the project's own rules: discover AGENTS.md-style
	// instruction files from the workspace unless the caller provided
	// conventions ("-" opts out). Work on a copy; requests are caller-owned.
	if req.Conventions == "" && ws != nil {
		if content, found := DiscoverConventions(ws.root); content != "" {
			reqCopy := *req
			reqCopy.Conventions = content
			req = &reqCopy
			rv.cfg.logger.InfoContext(ctx, "project conventions discovered", "files", found)
		}
	}

	if rv.cfg.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, rv.cfg.timeout)
		defer cancel()
	}

	if rv.cfg.deepReview {
		return rv.reviewDeep(ctx, req, schema, usingDefaultSchema, ws, start)
	}
	return rv.reviewSingle(ctx, req, schema, usingDefaultSchema, ws, start)
}

// reviewSingle is the single-agent review path: one (optionally exploring)
// agent produces the final output directly.
func (rv *Reviewer) reviewSingle(
	ctx context.Context,
	req *Request,
	schema *outputSchema,
	usingDefaultSchema bool,
	ws *workspace,
	start time.Time,
) (*Response, error) {
	var warnings []string

	instructions := rv.composeInstructions(rv.baseInstructions(), ws)
	agent, w := rv.buildAgent("code-reviewer", instructions, schema, ws)
	warnings = append(warnings, w...)

	userMsg, w := buildUserMessage(req, rv.cfg.maxInputBytes)
	warnings = append(warnings, w...)
	for _, warning := range w {
		rv.cfg.logger.WarnContext(ctx, "review input adjusted", "warning", warning)
	}

	stats := &runStats{}
	output, err := rv.runValidated(ctx, agent, []openai.ChatCompletionMessageParamUnion{
		openai.UserMessage(userMsg),
	}, schema, rv.runConfig(ws, 0), rv.cfg.maxOutputRetries, stats)
	if err != nil {
		return nil, err
	}

	return rv.buildResponse(output, usingDefaultSchema, stats, warnings, nil, start)
}

// baseInstructions returns the configured or default core review prompt.
func (rv *Reviewer) baseInstructions() string {
	if rv.cfg.instructions != "" {
		return rv.cfg.instructions
	}
	return DefaultInstructions
}

// composeInstructions augments a base prompt with exploration guidance and
// org-wide extra instructions.
func (rv *Reviewer) composeInstructions(base string, ws *workspace) string {
	if ws != nil {
		base += explorationInstructions
	}
	if rv.cfg.extraInstr != "" {
		base += "\n\n" + rv.cfg.extraInstr
	}
	return base
}

// buildAgent constructs an agent for a compiled schema, choosing between
// native structured outputs and prompt-enforced JSON, and attaching
// exploration tools when a workspace is given.
func (rv *Reviewer) buildAgent(name, instructions string, schema *outputSchema, ws *workspace) (*agents.Agent, []string) {
	var warnings []string

	agent := agents.NewAgent(name)
	agent.Model = rv.cfg.model
	agent.Temperature = rv.cfg.temperature
	agent.MaxTokens = rv.cfg.maxOutputTokens
	if ws != nil {
		agent.Tools = explorationTools(ws)
	}

	if schema.native != nil {
		format := libjs.JSONSchema(schema.name, schema.native).
			WithDescription("Structured output for " + name)
		if !isStrictCompatible(schema.raw) {
			// OpenAI rejects strict schemas unless every object closes
			// additionalProperties and requires all of its properties.
			format = format.WithStrict(false)
		}
		agent.ResponseFormat = format
	} else {
		// The schema uses keywords the structured-outputs API cannot
		// represent; enforce it via the prompt and validate client-side.
		instructions += fmt.Sprintf(promptModeInstructions, string(schema.raw))
		warnings = append(warnings, fmt.Sprintf(
			"output schema uses keywords not supported by native structured outputs (%s); falling back to prompt-enforced JSON with client-side validation",
			strings.Join(schema.lostKeywords, ", ")))
	}

	agent.Instructions = instructions
	return agent, warnings
}

// runConfig builds the per-run configuration. maxTurns overrides the turn
// budget; 0 selects the default (exploration budget with a workspace,
// single-shot without).
func (rv *Reviewer) runConfig(ws *workspace, maxTurns int) *agents.RunConfig {
	cfg := agents.DefaultRunConfig()
	cfg.MaxTurns = 1
	if ws != nil {
		cfg.MaxTurns = rv.cfg.maxTurns
		if cfg.MaxTurns == 0 {
			cfg.MaxTurns = DefaultExplorationTurns
		}
	}
	if maxTurns > 0 {
		cfg.MaxTurns = maxTurns
	}
	cfg.Timeout = rv.cfg.timeout
	cfg.TraceWorkflowName = "Code review"
	return cfg
}

// runStats accumulates execution metrics across runs and pipeline stages.
type runStats struct {
	usage      Usage
	turns      int
	toolCalls  int
	retries    int
	lastOutput string
}

func (s *runStats) merge(o *runStats) {
	s.usage.PromptTokens += o.usage.PromptTokens
	s.usage.CompletionTokens += o.usage.CompletionTokens
	s.usage.TotalTokens += o.usage.TotalTokens
	s.turns += o.turns
	s.toolCalls += o.toolCalls
	s.retries += o.retries
}

// runValidated runs an agent until its output parses and validates against
// the schema, feeding validation errors back to the model for up to
// maxRetries corrective rounds. Metrics are accumulated into stats.
func (rv *Reviewer) runValidated(
	ctx context.Context,
	agent *agents.Agent,
	messages []openai.ChatCompletionMessageParamUnion,
	schema *outputSchema,
	runConfig *agents.RunConfig,
	maxRetries int,
	stats *runStats,
) (json.RawMessage, error) {
	var lastErr error

	for attempt := 0; attempt <= maxRetries; attempt++ {
		rv.cfg.logger.InfoContext(ctx, "running agent",
			"agent", agent.Name, "model", agent.Model, "attempt", attempt+1, "max_turns", runConfig.MaxTurns)

		result, err := rv.runner.Run(ctx, agent, messages, agents.WithConfig(runConfig))
		if err != nil {
			if errors.Is(err, agents.ErrMaxTurnsExceeded) {
				return nil, fmt.Errorf("review: %s did not finish within %d turns (raise WithMaxTurns or narrow the change): %w", agent.Name, runConfig.MaxTurns, err)
			}
			return nil, fmt.Errorf("review: %s run failed: %w", agent.Name, err)
		}
		stats.usage.PromptTokens += result.Usage.PromptTokens
		stats.usage.CompletionTokens += result.Usage.CompletionTokens
		stats.usage.TotalTokens += result.Usage.TotalTokens
		stats.turns += len(result.Steps)
		for _, step := range result.Steps {
			stats.toolCalls += len(step.ToolCalls)
			for _, tc := range step.ToolCalls {
				rv.cfg.logger.InfoContext(ctx, "exploration tool call",
					"agent", agent.Name, "tool", tc.ToolName, "args", clip(tc.Arguments, 200), "duration", tc.Duration)
			}
		}
		stats.lastOutput = result.FinalOutput

		output, validationErr := rv.parseAndValidate(schema, result.FinalOutput)
		if validationErr == nil {
			return output, nil
		}

		lastErr = validationErr
		stats.retries++
		rv.cfg.logger.WarnContext(ctx, "agent output failed validation, retrying",
			"agent", agent.Name, "attempt", attempt+1, "error", validationErr)

		// Feed the invalid output and the validation error back so the model
		// can correct itself on the next attempt.
		messages = append(messages,
			openai.AssistantMessage(result.FinalOutput),
			openai.UserMessage(fmt.Sprintf(
				"Your previous response was rejected: %v\n\nRespond again with a single JSON document that strictly conforms to the required output schema. Output raw JSON only.",
				validationErr)),
		)
	}

	return nil, &OutputError{RawOutput: stats.lastOutput, Reason: lastErr}
}

// buildResponse assembles the final Response, parsing the typed Review when
// the built-in schema was used.
func (rv *Reviewer) buildResponse(
	output json.RawMessage,
	usingDefaultSchema bool,
	stats *runStats,
	warnings []string,
	specialists []string,
	start time.Time,
) (*Response, error) {
	resp := &Response{
		Output:      output,
		Model:       rv.cfg.model,
		Usage:       stats.usage,
		Duration:    time.Since(start),
		Retries:     stats.retries,
		Turns:       stats.turns,
		ToolCalls:   stats.toolCalls,
		Specialists: specialists,
		Warnings:    warnings,
	}
	if usingDefaultSchema {
		var parsed Review
		if err := json.Unmarshal(output, &parsed); err != nil {
			return nil, &OutputError{RawOutput: string(output),
				Reason: fmt.Errorf("output validated but failed to parse as Review: %w", err)}
		}
		normalizeFindings(&parsed)
		resp.Review = &parsed
	}
	rv.cfg.logger.Info("review complete",
		"duration", resp.Duration, "total_tokens", resp.Usage.TotalTokens,
		"retries", resp.Retries, "turns", resp.Turns, "tool_calls", resp.ToolCalls,
		"specialists", specialists)
	return resp, nil
}

// parseAndValidate extracts JSON from the model output and validates it
// against the schema. It returns the canonical raw JSON on success.
func (rv *Reviewer) parseAndValidate(schema *outputSchema, output string) (json.RawMessage, error) {
	raw, err := extractJSON(output)
	if err != nil {
		return nil, err
	}
	var instance any
	if err := json.Unmarshal(raw, &instance); err != nil {
		return nil, fmt.Errorf("output is not valid JSON: %w", err)
	}
	if err := schema.validate(instance); err != nil {
		return nil, fmt.Errorf("output does not validate against the schema: %w", err)
	}
	return raw, nil
}

// extractJSON pulls a JSON document out of a model response, tolerating
// markdown code fences and stray prose around the document.
func extractJSON(s string) (json.RawMessage, error) {
	trimmed := strings.TrimSpace(s)
	if trimmed == "" {
		return nil, fmt.Errorf("output is empty")
	}
	if json.Valid([]byte(trimmed)) {
		return json.RawMessage(trimmed), nil
	}

	// Strip a markdown code fence if present.
	if idx := strings.Index(trimmed, "```"); idx >= 0 {
		rest := trimmed[idx+3:]
		if nl := strings.IndexByte(rest, '\n'); nl >= 0 {
			rest = rest[nl+1:]
		}
		if end := strings.Index(rest, "```"); end >= 0 {
			candidate := strings.TrimSpace(rest[:end])
			if json.Valid([]byte(candidate)) {
				return json.RawMessage(candidate), nil
			}
		}
	}

	// Last resort: the outermost braces.
	if start, end := strings.IndexByte(trimmed, '{'), strings.LastIndexByte(trimmed, '}'); start >= 0 && end > start {
		candidate := trimmed[start : end+1]
		if json.Valid([]byte(candidate)) {
			return json.RawMessage(candidate), nil
		}
	}
	return nil, fmt.Errorf("output does not contain a valid JSON document")
}

// normalizeFindings repairs common anchor defects in model output so
// downstream consumers (PR annotations, SARIF, -fail-on) get clean
// locations: diff-style a/ b/ path prefixes are stripped, inverted line
// ranges are swapped, and negative line numbers are clamped to 0.
func normalizeFindings(rev *Review) {
	for i := range rev.Findings {
		f := &rev.Findings[i]
		f.File = strings.TrimPrefix(f.File, "./")
		if strings.HasPrefix(f.File, "a/") || strings.HasPrefix(f.File, "b/") {
			f.File = f.File[2:]
		}
		if f.LineStart < 0 {
			f.LineStart = 0
		}
		if f.LineEnd < 0 {
			f.LineEnd = 0
		}
		if f.LineEnd != 0 && f.LineStart != 0 && f.LineEnd < f.LineStart {
			f.LineStart, f.LineEnd = f.LineEnd, f.LineStart
		}
	}
}

// clip shortens a string for log output.
func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// isStrictCompatible reports whether a schema satisfies the requirements of
// OpenAI strict mode: every object sets additionalProperties to false and
// lists all of its properties as required.
func isStrictCompatible(raw json.RawMessage) bool {
	var node map[string]any
	if err := json.Unmarshal(raw, &node); err != nil {
		return false
	}
	return strictCompatibleNode(node)
}

func strictCompatibleNode(node map[string]any) bool {
	if t, _ := node["type"].(string); t == "object" {
		ap, ok := node["additionalProperties"].(bool)
		if !ok || ap {
			return false
		}
		props, _ := node["properties"].(map[string]any)
		required, _ := node["required"].([]any)
		requiredSet := make(map[string]bool, len(required))
		for _, r := range required {
			if name, ok := r.(string); ok {
				requiredSet[name] = true
			}
		}
		for name := range props {
			if !requiredSet[name] {
				return false
			}
		}
	}
	for _, v := range node {
		switch child := v.(type) {
		case map[string]any:
			if !strictCompatibleNode(child) {
				return false
			}
		case []any:
			for _, item := range child {
				if m, ok := item.(map[string]any); ok {
					if !strictCompatibleNode(m) {
						return false
					}
				}
			}
		}
	}
	return true
}

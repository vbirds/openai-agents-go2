package review

import (
	"context"
	"encoding/json"
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

// Review runs a single code review and returns output conforming to the
// requested schema. The returned Response.Output always validates against
// the schema; if the model cannot produce conforming output within the
// configured retries, an error wrapping ErrInvalidOutput is returned.
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

	var warnings []string
	agent, w := rv.buildAgent(schema)
	warnings = append(warnings, w...)

	userMsg, w := buildUserMessage(req, rv.cfg.maxInputBytes)
	warnings = append(warnings, w...)
	for _, warning := range w {
		rv.cfg.logger.WarnContext(ctx, "review input adjusted", "warning", warning)
	}

	if rv.cfg.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, rv.cfg.timeout)
		defer cancel()
	}

	runConfig := agents.DefaultRunConfig()
	runConfig.MaxTurns = 1
	runConfig.Timeout = rv.cfg.timeout
	runConfig.TraceWorkflowName = "Code review"

	messages := []openai.ChatCompletionMessageParamUnion{
		openai.UserMessage(userMsg),
	}

	var usage Usage
	var lastOutput string
	var lastErr error

	for attempt := 0; attempt <= rv.cfg.maxOutputRetries; attempt++ {
		rv.cfg.logger.InfoContext(ctx, "running review",
			"model", rv.cfg.model, "attempt", attempt+1)

		result, err := rv.runner.Run(ctx, agent, messages, agents.WithConfig(runConfig))
		if err != nil {
			return nil, fmt.Errorf("review: model run failed: %w", err)
		}
		usage.PromptTokens += result.Usage.PromptTokens
		usage.CompletionTokens += result.Usage.CompletionTokens
		usage.TotalTokens += result.Usage.TotalTokens
		lastOutput = result.FinalOutput

		output, validationErr := rv.parseAndValidate(schema, result.FinalOutput)
		if validationErr == nil {
			resp := &Response{
				Output:   output,
				Model:    rv.cfg.model,
				Usage:    usage,
				Duration: time.Since(start),
				Retries:  attempt,
				Warnings: warnings,
			}
			if usingDefaultSchema {
				var parsed Review
				if err := json.Unmarshal(output, &parsed); err != nil {
					return nil, &OutputError{RawOutput: string(output),
						Reason: fmt.Errorf("output validated but failed to parse as Review: %w", err)}
				}
				resp.Review = &parsed
			}
			rv.cfg.logger.InfoContext(ctx, "review complete",
				"duration", resp.Duration, "total_tokens", usage.TotalTokens, "retries", attempt)
			return resp, nil
		}

		lastErr = validationErr
		rv.cfg.logger.WarnContext(ctx, "review output failed validation, retrying",
			"attempt", attempt+1, "error", validationErr)

		// Feed the invalid output and the validation error back so the model
		// can correct itself on the next attempt.
		messages = append(messages,
			openai.AssistantMessage(result.FinalOutput),
			openai.UserMessage(fmt.Sprintf(
				"Your previous response was rejected: %v\n\nRespond again with a single JSON document that strictly conforms to the required output schema. Output raw JSON only.",
				validationErr)),
		)
	}

	return nil, &OutputError{RawOutput: lastOutput, Reason: lastErr}
}

// buildAgent constructs the underlying agent for a compiled schema, choosing
// between native structured outputs and prompt-enforced JSON.
func (rv *Reviewer) buildAgent(schema *outputSchema) (*agents.Agent, []string) {
	var warnings []string

	instructions := rv.cfg.instructions
	if instructions == "" {
		instructions = DefaultInstructions
	}
	if rv.cfg.extraInstr != "" {
		instructions += "\n\n" + rv.cfg.extraInstr
	}

	agent := agents.NewAgent("code-reviewer")
	agent.Model = rv.cfg.model
	agent.Temperature = rv.cfg.temperature
	agent.MaxTokens = rv.cfg.maxOutputTokens

	if schema.native != nil {
		format := libjs.JSONSchema(schema.name, schema.native).
			WithDescription("Code review result")
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

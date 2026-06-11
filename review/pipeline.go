package review

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/openai/openai-go/v3"
)

// Deep-review pipeline limits.
const (
	// maxSpecialistConcurrency caps how many specialists run in parallel.
	maxSpecialistConcurrency = 4
	// DefaultSpecialistTurns is the per-specialist exploration budget when
	// the specialist does not set its own.
	DefaultSpecialistTurns = 10
	// maxCandidateBytes caps the candidate-findings JSON handed to the
	// adjudicator.
	maxCandidateBytes = 96 * 1024
	// triageDiffBudget caps the diff excerpt shown to the triage agent.
	triageDiffBudget = 16 * 1024
)

// specialistCoreInstructions are the ground rules shared by every
// specialist, prepended to the specialist's own instructions.
const specialistCoreInstructions = `You are one of several focused reviewers examining a code change (a unified diff, optionally with full file contents). Review the CHANGE, not the whole codebase. Another agent will verify and merge findings from all reviewers, so:

- Report only findings inside your mandate, defined below. Other reviewers cover everything else.
- Every finding must be anchored: file path exactly as given, line numbers in the POST-CHANGE version.
- Explain WHY each finding is a problem and its impact, with enough detail that a verifier can check it.
- Set the confidence score honestly: findings you verified deserve high confidence, suspicions deserve low.
- Zero findings is a valid result. Do not pad.

Respond with a single JSON document in the required output format and nothing else.`

// adjudicatorInstructions drive the final pipeline stage: verify candidate
// findings against the code, then synthesize the final result.
const adjudicatorInstructions = `You are the adjudicator of a multi-reviewer code review. You receive a code change and candidate findings produced by specialist reviewers. Your job is to produce the final review, keeping only findings that hold up.

For each candidate finding:
1. Check it against the code. If repository exploration tools are available, read the referenced location and, where relevant, the callers or tests it implicates.
2. DROP findings that are wrong, unverifiable, outside the change, or restate the same issue as another finding (keep the best-written one, merging useful detail).
3. CALIBRATE what you keep: correct inaccurate file/line anchors, adjust severity to the definitions in the output format, and set confidence to reflect what you actually verified.

You may add a finding the specialists missed only if you verified it yourself during adjudication.

Then synthesize the overall result from the verified findings alone. Be decisive: request changes when verified findings warrant it, approve when the change is clean. An empty findings list with an approve verdict is the correct output for a clean change.

Respond with a single JSON document in the required output format and nothing else.`

// triageInstructions drive specialist selection.
const triageInstructions = `You route a code change to specialist reviewers. You receive the list of available specialists with descriptions of what each hunts for, the changed file paths, and an excerpt of the diff.

Select every specialist whose mandate plausibly applies to this change; skip those clearly irrelevant. When unsure, include the specialist: a wasted review is cheaper than a missed defect.

Respond with a single JSON document in the required output format and nothing else.`

// candidateFinding is a specialist finding tagged with its origin for the
// adjudicator.
type candidateFinding struct {
	ReportedBy string `json:"reported_by"`
	Finding
}

// reviewDeep runs the multi-agent pipeline: rule/LLM triage -> parallel
// specialists -> adjudication and synthesis into the requested schema.
func (rv *Reviewer) reviewDeep(
	ctx context.Context,
	req *Request,
	schema *outputSchema,
	usingDefaultSchema bool,
	ws *workspace,
	start time.Time,
) (*Response, error) {
	var warnings []string
	stats := &runStats{}

	userMsg, w := buildUserMessage(req, rv.cfg.maxInputBytes)
	warnings = append(warnings, w...)

	roster := req.Specialists
	if len(roster) == 0 {
		roster = rv.cfg.specialists
	}
	if len(roster) == 0 {
		warnings = append(warnings, "deep review requested but no specialists are configured; falling back to single-agent review")
		resp, err := rv.reviewSingle(ctx, req, schema, usingDefaultSchema, ws, start)
		if resp != nil {
			resp.Warnings = append(warnings, resp.Warnings...)
		}
		return resp, err
	}

	selected, triageWarnings := rv.selectSpecialists(ctx, req, roster, stats)
	warnings = append(warnings, triageWarnings...)

	candidates, specWarnings := rv.runSpecialists(ctx, selected, ws, userMsg, stats)
	warnings = append(warnings, specWarnings...)

	output, adjWarnings, err := rv.runAdjudicator(ctx, schema, ws, userMsg, candidates, stats)
	warnings = append(warnings, adjWarnings...)
	if err != nil {
		return nil, err
	}

	names := make([]string, len(selected))
	for i, s := range selected {
		names[i] = s.Name
	}
	return rv.buildResponse(output, usingDefaultSchema, stats, warnings, names, start)
}

// selectSpecialists resolves the roster via rule triggers first and LLM
// triage for the rest. Triage failures degrade to running everything.
func (rv *Reviewer) selectSpecialists(
	ctx context.Context,
	req *Request,
	roster []Specialist,
	stats *runStats,
) (selected []Specialist, warnings []string) {
	changedPaths := matchPathsFromRequest(req)

	var undecided []Specialist
	for _, s := range roster {
		switch {
		case s.Triggers.matches(changedPaths, req.Diff):
			selected = append(selected, s)
		case s.Triggers.rulesDefined():
			// Rules defined but none matched: definitively not selected.
		default:
			undecided = append(undecided, s)
		}
	}
	if len(undecided) == 0 {
		return selected, nil
	}

	chosen, err := rv.runTriage(ctx, undecided, changedPaths, req.Diff, stats)
	if err != nil {
		// Recall-safe degradation: run everything the triage could not
		// rule out; the adjudicator gates the extra noise.
		warnings = append(warnings,
			fmt.Sprintf("triage failed (%v); running all %d undecided specialists", err, len(undecided)))
		return append(selected, undecided...), warnings
	}
	for _, s := range undecided {
		if chosen[s.Name] {
			selected = append(selected, s)
		}
	}
	if len(selected) == 0 {
		// Nothing rule-matched and triage declined everything: degrade to
		// the full roster rather than reviewing with no specialists.
		warnings = append(warnings, "triage selected no specialists; running the full roster")
		return roster, warnings
	}
	return selected, warnings
}

// runTriage asks a single-shot agent to pick relevant specialists.
func (rv *Reviewer) runTriage(
	ctx context.Context,
	undecided []Specialist,
	changedPaths []string,
	diff string,
	stats *runStats,
) (map[string]bool, error) {
	names := make([]string, len(undecided))
	for i, s := range undecided {
		names[i] = s.Name
	}
	rawSchema, err := triageSchemaJSON(names)
	if err != nil {
		return nil, err
	}
	schema, err := compileSchema(rawSchema, "review_triage")
	if err != nil {
		return nil, err
	}

	var b strings.Builder
	b.WriteString("## Available specialists\n\n")
	for _, s := range undecided {
		fmt.Fprintf(&b, "- %s: %s\n", s.Name, s.Description)
	}
	b.WriteString("\n## Changed files\n\n")
	for _, p := range changedPaths {
		fmt.Fprintf(&b, "- %s\n", p)
	}
	b.WriteString("\n## Diff excerpt\n\n```diff\n")
	excerpt, _ := truncateTo(diff, triageDiffBudget, "diff")
	b.WriteString(excerpt)
	b.WriteString("\n```\n\nSelect the relevant specialists now.")

	agent, _ := rv.buildAgent("review-triage", triageInstructions, schema, nil)
	if rv.cfg.triageModel != "" {
		agent.Model = rv.cfg.triageModel
	}

	output, err := rv.runValidated(ctx, agent, []openai.ChatCompletionMessageParamUnion{
		openai.UserMessage(b.String()),
	}, schema, rv.runConfig(nil, 1), 1, stats)
	if err != nil {
		return nil, err
	}

	var result struct {
		Selected []string `json:"selected"`
	}
	if err := json.Unmarshal(output, &result); err != nil {
		return nil, err
	}
	chosen := make(map[string]bool, len(result.Selected))
	for _, n := range result.Selected {
		chosen[n] = true
	}
	rv.cfg.logger.InfoContext(ctx, "triage complete", "selected", result.Selected)
	return chosen, nil
}

// runSpecialists fans the review out to the selected specialists in
// parallel and merges their candidate findings. Individual specialist
// failures degrade to warnings; they never fail the review.
func (rv *Reviewer) runSpecialists(
	ctx context.Context,
	selected []Specialist,
	ws *workspace,
	userMsg string,
	stats *runStats,
) ([]candidateFinding, []string) {
	type specialistResult struct {
		findings []Finding
		stats    runStats
		err      error
	}
	results := make([]specialistResult, len(selected))

	sem := make(chan struct{}, maxSpecialistConcurrency)
	var wg sync.WaitGroup
	for i, spec := range selected {
		wg.Add(1)
		go func(i int, spec Specialist) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			results[i].findings, results[i].err = rv.runSpecialist(ctx, spec, ws, userMsg, &results[i].stats)
		}(i, spec)
	}
	wg.Wait()

	var candidates []candidateFinding
	var warnings []string
	for i, spec := range selected {
		stats.merge(&results[i].stats)
		if results[i].err != nil {
			warnings = append(warnings,
				fmt.Sprintf("specialist %q failed and was skipped: %v", spec.Name, results[i].err))
			continue
		}
		allowed := categorySet(spec.Categories)
		for _, f := range results[i].findings {
			if allowed != nil && !allowed[strings.ToLower(f.Category)] {
				rv.cfg.logger.Info("dropping out-of-mandate finding",
					"specialist", spec.Name, "category", f.Category, "title", f.Title)
				continue
			}
			candidates = append(candidates, candidateFinding{ReportedBy: spec.Name, Finding: f})
		}
	}
	return candidates, warnings
}

// runSpecialist executes one specialist and returns its findings.
func (rv *Reviewer) runSpecialist(
	ctx context.Context,
	spec Specialist,
	ws *workspace,
	userMsg string,
	stats *runStats,
) ([]Finding, error) {
	schema, err := compileSchema(json.RawMessage(specialistSchemaJSON), "specialist_findings")
	if err != nil {
		return nil, err
	}

	instructions := specialistCoreInstructions + "\n\n# Your mandate\n\n" + spec.Instructions
	if len(spec.Categories) > 0 {
		instructions += "\n\nOnly use these finding categories: " + strings.Join(spec.Categories, ", ") + "."
	}
	instructions = rv.composeInstructions(instructions, ws)

	agent, _ := rv.buildAgent("specialist-"+spec.Name, instructions, schema, ws)
	if spec.Model != "" {
		agent.Model = spec.Model
	}

	turns := 0
	if ws != nil {
		turns = spec.MaxTurns
		if turns == 0 {
			turns = DefaultSpecialistTurns
		}
	}

	output, err := rv.runValidated(ctx, agent, []openai.ChatCompletionMessageParamUnion{
		openai.UserMessage(userMsg),
	}, schema, rv.runConfig(ws, turns), 1, stats)
	if err != nil {
		return nil, err
	}

	var result struct {
		Findings []Finding `json:"findings"`
	}
	if err := json.Unmarshal(output, &result); err != nil {
		return nil, err
	}
	rv.cfg.logger.InfoContext(ctx, "specialist complete",
		"specialist", spec.Name, "findings", len(result.Findings))
	return result.Findings, nil
}

// runAdjudicator verifies the candidate findings against the change (and
// the workspace, when available) and synthesizes the final output in the
// requested schema.
func (rv *Reviewer) runAdjudicator(
	ctx context.Context,
	schema *outputSchema,
	ws *workspace,
	userMsg string,
	candidates []candidateFinding,
	stats *runStats,
) (json.RawMessage, []string, error) {
	instructions := rv.composeInstructions(adjudicatorInstructions, ws)
	agent, warnings := rv.buildAgent("review-adjudicator", instructions, schema, ws)

	var b strings.Builder
	b.WriteString("## Candidate findings from specialist reviewers\n\n")
	if len(candidates) == 0 {
		b.WriteString("No candidate findings were reported. Verify the change looks clean and produce the final result.\n")
	} else {
		data, err := json.MarshalIndent(candidates, "", "  ")
		if err != nil {
			return nil, warnings, fmt.Errorf("review: encoding candidate findings: %w", err)
		}
		text := string(data)
		if len(text) > maxCandidateBytes {
			text, _ = truncateTo(text, maxCandidateBytes, "candidate findings")
			warnings = append(warnings, "candidate findings truncated to fit the adjudication budget")
		}
		b.WriteString("```json\n")
		b.WriteString(text)
		b.WriteString("\n```\n")
	}
	b.WriteString("\nAdjudicate the candidates and produce the final review now in the required JSON output format.")

	messages := []openai.ChatCompletionMessageParamUnion{
		openai.UserMessage(userMsg),
		openai.UserMessage(b.String()),
	}
	output, err := rv.runValidated(ctx, agent, messages, schema, rv.runConfig(ws, 0), rv.cfg.maxOutputRetries, stats)
	return output, warnings, err
}

// categorySet normalizes a category whitelist; nil means "allow all".
func categorySet(categories []string) map[string]bool {
	if len(categories) == 0 {
		return nil
	}
	set := make(map[string]bool, len(categories))
	for _, c := range categories {
		set[strings.ToLower(strings.TrimSpace(c))] = true
	}
	return set
}

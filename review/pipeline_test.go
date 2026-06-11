package review

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/packages/ssestream"

	"github.com/MitulShah1/openai-agents-go/models"
)

// routingModel answers by inspecting the request, so it works regardless of
// the order parallel pipeline stages call it. Routes are matched against
// the system prompt; the first matching route wins.
type routingModel struct {
	mu       sync.Mutex
	routes   []route
	requests []openai.ChatCompletionNewParams
}

type route struct {
	match   string // substring of the system prompt
	content string // canned assistant response
	fail    bool   // simulate a model failure instead
}

func (m *routingModel) GetResponse(_ context.Context, params openai.ChatCompletionNewParams, _ models.ModelSettings) (*models.ModelResponse, error) {
	m.mu.Lock()
	m.requests = append(m.requests, params)
	m.mu.Unlock()

	system := ""
	if len(params.Messages) > 0 && params.Messages[0].OfSystem != nil {
		system = params.Messages[0].OfSystem.Content.OfString.Value
	}
	for _, r := range m.routes {
		if strings.Contains(system, r.match) {
			if r.fail {
				return nil, errors.New("routingModel: simulated failure")
			}
			return &models.ModelResponse{
				Completion: &openai.ChatCompletion{
					Choices: []openai.ChatCompletionChoice{{
						Message:      openai.ChatCompletionMessage{Role: "assistant", Content: r.content},
						FinishReason: "stop",
					}},
				},
				Usage: models.ModelUsage{PromptTokens: 100, CompletionTokens: 50, TotalTokens: 150},
			}, nil
		}
	}
	return nil, errors.New("routingModel: no route matched system prompt")
}

func (m *routingModel) StreamResponse(context.Context, openai.ChatCompletionNewParams, models.ModelSettings) (*ssestream.Stream[openai.ChatCompletionChunk], error) {
	return nil, errors.New("routingModel: streaming not supported")
}

func (m *routingModel) ModelName() string { return "routing-model" }

type routingProvider struct{ model *routingModel }

func (p *routingProvider) GetModel(string) (models.Model, error) { return p.model, nil }

// systemPrompts returns the system prompt of every recorded request.
func (m *routingModel) systemPrompts() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []string
	for _, r := range m.requests {
		if len(r.Messages) > 0 && r.Messages[0].OfSystem != nil {
			out = append(out, r.Messages[0].OfSystem.Content.OfString.Value)
		}
	}
	return out
}

func findingJSON(title, category string) string {
	f := Finding{
		Title: title, Body: "details for " + title, Severity: SeverityHigh,
		Category: category, Confidence: 0.8, File: "server.go",
		LineStart: 42, LineEnd: 44, Suggestion: "",
	}
	data, _ := json.Marshal(map[string]any{"findings": []Finding{f}})
	return string(data)
}

func TestDeepReviewPipeline(t *testing.T) {
	model := &routingModel{routes: []route{
		// Specialists are matched by their mandate text.
		{match: "correctness specialist", content: findingJSON("Nil deref", "bug")},
		{match: "security specialist", content: findingJSON("SQL injection", "security")},
		{match: "adjudicator", content: validReviewJSON},
	}}

	r, err := New(
		WithModelProvider(&routingProvider{model: model}),
		WithDeepReview(true),
		// Limit the roster to two rule-triggered specialists so no LLM
		// triage call is needed.
		WithSpecialists(
			BuiltinSpecialists()[0], // correctness: Always
			BuiltinSpecialists()[1], // security: keyword-triggered
		),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	req := defaultRequest()
	req.Diff += "+row := db.Query(\"SELECT * FROM users WHERE name = '\" + name + \"'\")\n"

	resp, err := r.Review(context.Background(), req)
	if err != nil {
		t.Fatalf("Review: %v", err)
	}

	if len(resp.Specialists) != 2 {
		t.Errorf("specialists = %v, want correctness+security", resp.Specialists)
	}
	if resp.Review == nil || resp.Review.Verdict != VerdictRequestChanges {
		t.Errorf("unexpected final review: %+v", resp.Review)
	}
	// 2 specialists + 1 adjudicator, no triage (all rule-resolved).
	prompts := model.systemPrompts()
	if len(prompts) != 3 {
		t.Fatalf("model calls = %d, want 3", len(prompts))
	}
	for _, p := range prompts {
		if strings.Contains(p, "route a code change") {
			t.Error("triage must be skipped when all specialists are rule-resolved")
		}
	}
	// Usage accumulates across all stages.
	if resp.Usage.TotalTokens != 450 {
		t.Errorf("usage = %d, want 450", resp.Usage.TotalTokens)
	}

	// The adjudicator must receive both specialists' candidates, tagged.
	adjMsg := lastUserMessage(t, model, "adjudicator")
	for _, want := range []string{"Nil deref", "SQL injection", `"reported_by": "correctness"`, `"reported_by": "security"`} {
		if !strings.Contains(adjMsg, want) {
			t.Errorf("adjudicator message missing %q", want)
		}
	}
}

// lastUserMessage returns the concatenated user messages of the first
// recorded request whose system prompt contains marker.
func lastUserMessage(t *testing.T, m *routingModel, marker string) string {
	t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, r := range m.requests {
		if len(r.Messages) == 0 || r.Messages[0].OfSystem == nil {
			continue
		}
		if !strings.Contains(r.Messages[0].OfSystem.Content.OfString.Value, marker) {
			continue
		}
		var b strings.Builder
		for _, msg := range r.Messages {
			if u := msg.OfUser; u != nil {
				b.WriteString(u.Content.OfString.Value)
			}
		}
		return b.String()
	}
	t.Fatalf("no request with system prompt containing %q", marker)
	return ""
}

func TestDeepReviewTriageRouting(t *testing.T) {
	perfSelected := `{"selected": ["performance"]}`
	model := &routingModel{routes: []route{
		{match: "route a code change", content: perfSelected},
		{match: "performance specialist", content: findingJSON("N+1 query", "performance")},
		{match: "custom-llm-routed specialist text", content: findingJSON("Custom issue", "other")},
		{match: "adjudicator", content: validReviewJSON},
	}}

	custom := Specialist{
		Name:         "custom",
		Description:  "A custom specialist for the test.",
		Instructions: "custom-llm-routed specialist text",
	}
	r, err := New(
		WithModelProvider(&routingProvider{model: model}),
		WithDeepReview(true),
		WithSpecialists(BuiltinSpecialists()[3], custom), // performance + custom: both LLM-routed
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	resp, err := r.Review(context.Background(), defaultRequest())
	if err != nil {
		t.Fatalf("Review: %v", err)
	}

	// Triage selected only "performance"; "custom" must not run.
	if len(resp.Specialists) != 1 || resp.Specialists[0] != "performance" {
		t.Errorf("specialists = %v, want [performance]", resp.Specialists)
	}
	for _, p := range model.systemPrompts() {
		if strings.Contains(p, "custom-llm-routed specialist text") {
			t.Error("unselected specialist must not run")
		}
	}

	// The triage request must offer both names via the schema enum.
	m := model
	m.mu.Lock()
	var triageReq *openai.ChatCompletionNewParams
	for i := range m.requests {
		if m.requests[i].ResponseFormat.OfJSONSchema != nil &&
			m.requests[i].ResponseFormat.OfJSONSchema.JSONSchema.Name == "review_triage" {
			triageReq = &m.requests[i]
		}
	}
	m.mu.Unlock()
	if triageReq == nil {
		t.Fatal("no triage request recorded")
	}
	schemaJSON, _ := json.Marshal(triageReq.ResponseFormat.OfJSONSchema.JSONSchema.Schema)
	if !strings.Contains(string(schemaJSON), "performance") || !strings.Contains(string(schemaJSON), "custom") {
		t.Errorf("triage schema must enumerate roster names, got %s", schemaJSON)
	}
}

func TestDeepReviewSpecialistFailureIsolation(t *testing.T) {
	model := &routingModel{routes: []route{
		{match: "correctness specialist", content: findingJSON("Nil deref", "bug")},
		{match: "security specialist", fail: true},
		{match: "adjudicator", content: validReviewJSON},
	}}

	r, err := New(
		WithModelProvider(&routingProvider{model: model}),
		WithDeepReview(true),
		WithSpecialists(BuiltinSpecialists()[0], BuiltinSpecialists()[1]),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	req := defaultRequest()
	req.Diff += "+db.Query(\"SELECT 1 WHERE name='\" + name + \"'\")\n" // trigger security

	resp, err := r.Review(context.Background(), req)
	if err != nil {
		t.Fatalf("Review must survive a specialist failure: %v", err)
	}
	found := false
	for _, w := range resp.Warnings {
		if strings.Contains(w, `specialist "security" failed`) {
			found = true
		}
	}
	if !found {
		t.Errorf("expected failure warning, got %v", resp.Warnings)
	}
	// The surviving specialist's finding still reaches the adjudicator.
	if adjMsg := lastUserMessage(t, model, "adjudicator"); !strings.Contains(adjMsg, "Nil deref") {
		t.Error("surviving specialist finding missing from adjudication")
	}
}

func TestDeepReviewCategoryWhitelist(t *testing.T) {
	// The correctness specialist reports a "style" finding, which is
	// outside its category whitelist and must be dropped in code.
	styleFinding, _ := json.Marshal(map[string]any{"findings": []Finding{
		{Title: "Bad style", Body: "x", Severity: SeverityLow, Category: "style",
			Confidence: 0.9, File: "server.go", LineStart: 1, LineEnd: 1},
		{Title: "Real bug", Body: "x", Severity: SeverityHigh, Category: "bug",
			Confidence: 0.9, File: "server.go", LineStart: 2, LineEnd: 2},
	}})
	model := &routingModel{routes: []route{
		{match: "correctness specialist", content: string(styleFinding)},
		{match: "adjudicator", content: validReviewJSON},
	}}

	r, err := New(
		WithModelProvider(&routingProvider{model: model}),
		WithDeepReview(true),
		WithSpecialists(BuiltinSpecialists()[0]),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if _, err := r.Review(context.Background(), defaultRequest()); err != nil {
		t.Fatalf("Review: %v", err)
	}
	adjMsg := lastUserMessage(t, model, "adjudicator")
	if strings.Contains(adjMsg, "Bad style") {
		t.Error("out-of-mandate finding must be dropped before adjudication")
	}
	if !strings.Contains(adjMsg, "Real bug") {
		t.Error("in-mandate finding must be kept")
	}
}

func TestDeepReviewRequestSpecialistsOverride(t *testing.T) {
	model := &routingModel{routes: []route{
		{match: "only-specialist-marker", content: findingJSON("Only finding", "other")},
		{match: "adjudicator", content: validReviewJSON},
	}}

	r, err := New(
		WithModelProvider(&routingProvider{model: model}),
		WithDeepReview(true), // configured roster: all built-ins
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	req := defaultRequest()
	req.Specialists = []Specialist{{
		Name:         "only",
		Instructions: "only-specialist-marker",
		Triggers:     Trigger{Always: true},
	}}

	resp, err := r.Review(context.Background(), req)
	if err != nil {
		t.Fatalf("Review: %v", err)
	}
	if len(resp.Specialists) != 1 || resp.Specialists[0] != "only" {
		t.Errorf("specialists = %v, want [only]", resp.Specialists)
	}
}

func TestSpecialistTriggerMatching(t *testing.T) {
	cases := []struct {
		trigger Trigger
		paths   []string
		diff    string
		want    bool
	}{
		{Trigger{Always: true}, nil, "", true},
		{Trigger{Paths: []string{"api/**"}}, []string{"api/v1/users.go"}, "", true},
		{Trigger{Paths: []string{"api/**"}}, []string{"internal/api.go"}, "", false},
		{Trigger{Paths: []string{"**/*.sql"}}, []string{"db/migrations/001.sql"}, "", true},
		{Trigger{Paths: []string{"*.go"}}, []string{"sub/main.go"}, "", false}, // * does not cross /
		{Trigger{Paths: []string{"*.go"}}, []string{"main.go"}, "", true},
		{Trigger{Keywords: []string{"go func"}}, nil, "+\tgo func() {\n", true},
		{Trigger{Keywords: []string{"Mutex"}}, nil, "+var mu sync.mutex\n", true}, // case-insensitive
		{Trigger{Keywords: []string{"sql"}}, nil, "+fmt.Println(1)\n", false},
		{Trigger{}, nil, "anything", false}, // no rules -> never rule-matched
	}
	for i, tc := range cases {
		if got := tc.trigger.matches(tc.paths, tc.diff); got != tc.want {
			t.Errorf("case %d: matches = %v, want %v", i, got, tc.want)
		}
	}
}

func TestMatchPathsFromRequest(t *testing.T) {
	req := &Request{
		Diff: "--- a/old.go\n+++ b/new.go\n@@ -1 +1 @@\n-x\n+y\n" +
			"Index: svn-style.go\n--- svn-style.go\t(revision 1)\n+++ svn-style.go\t(revision 2)\n",
		Files: []File{{Path: "explicit.go", Content: "x"}},
	}
	paths := matchPathsFromRequest(req)
	want := map[string]bool{"old.go": true, "new.go": true, "svn-style.go": true, "explicit.go": true}
	if len(paths) != len(want) {
		t.Fatalf("paths = %v, want keys %v", paths, want)
	}
	for _, p := range paths {
		if !want[p] {
			t.Errorf("unexpected path %q", p)
		}
	}
}

func TestSpecialistValidate(t *testing.T) {
	valid := Specialist{Name: "x", Instructions: "do x", Triggers: Trigger{Always: true}}
	if err := valid.Validate(); err != nil {
		t.Errorf("valid specialist rejected: %v", err)
	}
	noDesc := Specialist{Name: "x", Instructions: "do x"} // no rules, no description
	if err := noDesc.Validate(); err == nil {
		t.Error("specialist without description or rules must be rejected")
	}
	if err := (&Specialist{Instructions: "i"}).Validate(); err == nil {
		t.Error("nameless specialist must be rejected")
	}
	if err := (&Specialist{Name: "x"}).Validate(); err == nil {
		t.Error("instruction-less specialist must be rejected")
	}
}

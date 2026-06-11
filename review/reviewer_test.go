package review

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/packages/ssestream"

	"github.com/MitulShah1/openai-agents-go/models"
)

// fakeModel returns scripted responses and records the requests it receives.
type fakeModel struct {
	responses []string
	calls     int
	requests  []openai.ChatCompletionNewParams
}

func (f *fakeModel) GetResponse(_ context.Context, params openai.ChatCompletionNewParams, _ models.ModelSettings) (*models.ModelResponse, error) {
	f.requests = append(f.requests, params)
	if f.calls >= len(f.responses) {
		return nil, errors.New("fakeModel: no more scripted responses")
	}
	content := f.responses[f.calls]
	f.calls++
	return &models.ModelResponse{
		Completion: &openai.ChatCompletion{
			Choices: []openai.ChatCompletionChoice{{
				Message:      openai.ChatCompletionMessage{Role: "assistant", Content: content},
				FinishReason: "stop",
			}},
		},
		Usage: models.ModelUsage{PromptTokens: 100, CompletionTokens: 50, TotalTokens: 150},
	}, nil
}

func (f *fakeModel) StreamResponse(context.Context, openai.ChatCompletionNewParams, models.ModelSettings) (*ssestream.Stream[openai.ChatCompletionChunk], error) {
	return nil, errors.New("fakeModel: streaming not supported")
}

func (f *fakeModel) ModelName() string { return "fake-model" }

type fakeProvider struct{ model *fakeModel }

func (p *fakeProvider) GetModel(string) (models.Model, error) { return p.model, nil }

func newTestReviewer(t *testing.T, model *fakeModel, opts ...Option) *Reviewer {
	t.Helper()
	opts = append(opts, WithModelProvider(&fakeProvider{model: model}))
	r, err := New(opts...)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return r
}

const validReviewJSON = `{
	"summary": "The change introduces a nil pointer dereference.",
	"verdict": "request_changes",
	"confidence": 0.9,
	"findings": [{
		"title": "Nil pointer dereference",
		"body": "user may be nil when the lookup fails.",
		"severity": "high",
		"category": "bug",
		"confidence": 0.85,
		"file": "server.go",
		"line_start": 42,
		"line_end": 44,
		"suggestion": "Check the error before using user."
	}]
}`

func defaultRequest() *Request {
	return &Request{
		Prompt: "Focus on correctness.",
		Diff:   "--- a/server.go\n+++ b/server.go\n@@ -40,3 +40,5 @@\n+user, _ := lookup(id)\n+fmt.Println(user.Name)\n",
		Files:  []File{{Path: "server.go", Content: "package main\n\nfunc main() {}\n"}},
	}
}

func TestReviewDefaultSchema(t *testing.T) {
	model := &fakeModel{responses: []string{validReviewJSON}}
	r := newTestReviewer(t, model)

	resp, err := r.Review(context.Background(), defaultRequest())
	if err != nil {
		t.Fatalf("Review: %v", err)
	}
	if resp.Review == nil {
		t.Fatal("expected typed Review for default schema")
	}
	if resp.Review.Verdict != VerdictRequestChanges {
		t.Errorf("verdict = %q, want %q", resp.Review.Verdict, VerdictRequestChanges)
	}
	if len(resp.Review.Findings) != 1 {
		t.Fatalf("findings = %d, want 1", len(resp.Review.Findings))
	}
	f := resp.Review.Findings[0]
	if f.Severity != SeverityHigh || f.File != "server.go" || f.LineStart != 42 {
		t.Errorf("unexpected finding: %+v", f)
	}
	if resp.Usage.TotalTokens != 150 {
		t.Errorf("usage = %d, want 150", resp.Usage.TotalTokens)
	}
	if resp.Retries != 0 {
		t.Errorf("retries = %d, want 0", resp.Retries)
	}

	// Default schema must be enforced natively via response_format.
	if len(model.requests) != 1 {
		t.Fatalf("model calls = %d, want 1", len(model.requests))
	}
	if model.requests[0].ResponseFormat.OfJSONSchema == nil {
		t.Error("expected native json_schema response format for the default schema")
	}
}

func TestReviewRetriesOnInvalidOutput(t *testing.T) {
	model := &fakeModel{responses: []string{
		"I think this code looks fine!",                  // not JSON
		`{"summary": "x", "verdict": "maybe"}`,           // fails schema validation
		"```json\n" + validReviewJSON + "\n```",          // valid, fenced
	}}
	r := newTestReviewer(t, model, WithMaxOutputRetries(2))

	resp, err := r.Review(context.Background(), defaultRequest())
	if err != nil {
		t.Fatalf("Review: %v", err)
	}
	if resp.Retries != 2 {
		t.Errorf("retries = %d, want 2", resp.Retries)
	}
	// Usage accumulates across attempts.
	if resp.Usage.TotalTokens != 450 {
		t.Errorf("usage = %d, want 450", resp.Usage.TotalTokens)
	}
}

func TestReviewFailsAfterRetriesExhausted(t *testing.T) {
	model := &fakeModel{responses: []string{"nope", "still nope"}}
	r := newTestReviewer(t, model, WithMaxOutputRetries(1))

	_, err := r.Review(context.Background(), defaultRequest())
	if !errors.Is(err, ErrInvalidOutput) {
		t.Fatalf("err = %v, want ErrInvalidOutput", err)
	}
	var outErr *OutputError
	if !errors.As(err, &outErr) || outErr.RawOutput != "still nope" {
		t.Errorf("expected OutputError carrying the last output, got %#v", err)
	}
}

func TestReviewCustomSchemaNative(t *testing.T) {
	schema := json.RawMessage(`{
		"type": "object",
		"additionalProperties": false,
		"properties": {
			"risk": {"type": "string", "enum": ["low", "medium", "high"]},
			"notes": {"type": "array", "items": {"type": "string"}}
		},
		"required": ["risk", "notes"]
	}`)
	model := &fakeModel{responses: []string{`{"risk": "low", "notes": ["lgtm"]}`}}
	r := newTestReviewer(t, model)

	req := defaultRequest()
	req.Schema = schema
	req.SchemaName = "risk_report"

	resp, err := r.Review(context.Background(), req)
	if err != nil {
		t.Fatalf("Review: %v", err)
	}
	if resp.Review != nil {
		t.Error("Review must be nil for custom schemas")
	}
	var out struct {
		Risk  string   `json:"risk"`
		Notes []string `json:"notes"`
	}
	if err := json.Unmarshal(resp.Output, &out); err != nil {
		t.Fatalf("unmarshal output: %v", err)
	}
	if out.Risk != "low" {
		t.Errorf("risk = %q, want low", out.Risk)
	}
	if model.requests[0].ResponseFormat.OfJSONSchema == nil {
		t.Fatal("expected native json_schema response format")
	}
	if name := model.requests[0].ResponseFormat.OfJSONSchema.JSONSchema.Name; name != "risk_report" {
		t.Errorf("schema name = %q, want risk_report", name)
	}
}

func TestReviewCustomSchemaPromptFallback(t *testing.T) {
	// oneOf is not representable by the SDK schema type, forcing prompt mode.
	schema := json.RawMessage(`{
		"type": "object",
		"properties": {
			"result": {"oneOf": [{"type": "string"}, {"type": "integer"}]}
		},
		"required": ["result"]
	}`)
	model := &fakeModel{responses: []string{`{"result": 5}`}}
	r := newTestReviewer(t, model)

	req := defaultRequest()
	req.Schema = schema

	resp, err := r.Review(context.Background(), req)
	if err != nil {
		t.Fatalf("Review: %v", err)
	}
	if model.requests[0].ResponseFormat.OfJSONSchema != nil {
		t.Error("expected no native response format in prompt-fallback mode")
	}
	foundWarning := false
	for _, w := range resp.Warnings {
		if strings.Contains(w, "prompt-enforced") {
			foundWarning = true
		}
	}
	if !foundWarning {
		t.Errorf("expected prompt-fallback warning, got %v", resp.Warnings)
	}

	// The schema must be embedded in the system prompt.
	sys := model.requests[0].Messages[0].OfSystem
	if sys == nil || !strings.Contains(sys.Content.OfString.Value, `"oneOf"`) {
		t.Error("expected schema embedded in system prompt")
	}

	// And the output must still be validated client-side: a wrong-type value
	// would have been rejected. Validate the negative case explicitly.
	model2 := &fakeModel{responses: []string{`{"result": true}`, `{"result": "ok"}`}}
	r2 := newTestReviewer(t, model2, WithMaxOutputRetries(1))
	resp2, err := r2.Review(context.Background(), req)
	if err != nil {
		t.Fatalf("Review (retry case): %v", err)
	}
	if resp2.Retries != 1 {
		t.Errorf("retries = %d, want 1 (client-side validation must reject boolean)", resp2.Retries)
	}
}

func TestReviewInvalidRequest(t *testing.T) {
	r := newTestReviewer(t, &fakeModel{})

	cases := []*Request{
		nil,
		{},
		{Prompt: "review please"},                    // no diff, no files
		{Diff: "x", Schema: json.RawMessage(`{bad`)}, // invalid schema JSON
		{Files: []File{{Path: "", Content: "x"}}},    // empty path
	}
	for i, req := range cases {
		if _, err := r.Review(context.Background(), req); !errors.Is(err, ErrInvalidRequest) {
			t.Errorf("case %d: err = %v, want ErrInvalidRequest", i, err)
		}
	}
}

func TestReviewInvalidSchema(t *testing.T) {
	r := newTestReviewer(t, &fakeModel{})
	req := defaultRequest()
	req.Schema = json.RawMessage(`{"type": "object", "properties": {"x": {"type": 123}}}`)
	if _, err := r.Review(context.Background(), req); !errors.Is(err, ErrInvalidSchema) {
		t.Errorf("err = %v, want ErrInvalidSchema", err)
	}
}

func TestNewOptionValidation(t *testing.T) {
	if _, err := New(WithTemperature(3)); err == nil {
		t.Error("expected error for temperature out of range")
	}
	if _, err := New(WithModel("")); err == nil {
		t.Error("expected error for empty model")
	}
	if _, err := New(WithMaxInputBytes(10)); err == nil {
		t.Error("expected error for tiny input budget")
	}
}

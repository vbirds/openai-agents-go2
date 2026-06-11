package main

import (
	"strings"
	"testing"

	"github.com/MitulShah1/openai-agents-go/review"
)

func TestParseFlags(t *testing.T) {
	opts, files, err := parseFlags([]string{
		"-diff", "change.patch", "-prompt", "be strict", "-fail-on", "high", "a.go", "b.go",
	})
	if err != nil {
		t.Fatalf("parseFlags: %v", err)
	}
	if opts.diffPath != "change.patch" || opts.prompt != "be strict" || opts.failOn != "high" {
		t.Errorf("unexpected opts: %+v", opts)
	}
	if len(files) != 2 || files[0] != "a.go" {
		t.Errorf("files = %v", files)
	}
}

func TestParseFlagsRejectsConflicts(t *testing.T) {
	cases := [][]string{
		{"-diff", "x.patch", "-git", "HEAD~1"},
		{"-prompt", "a", "-prompt-file", "b"},
		{"-format", "xml"},
		{"-format", "markdown", "-schema", "s.json"},
		{"-fail-on", "high", "-schema", "s.json"},
	}
	for _, args := range cases {
		if _, _, err := parseFlags(args); err == nil {
			t.Errorf("parseFlags(%v): expected error", args)
		}
	}
}

func TestRenderMarkdown(t *testing.T) {
	resp := &review.Response{
		Model: "gpt-4o",
		Review: &review.Review{
			Summary:    "One bug found.",
			Verdict:    review.VerdictRequestChanges,
			Confidence: 0.9,
			Findings: []review.Finding{
				{
					Title: "Low issue", Body: "minor", Severity: review.SeverityLow,
					Category: "style", Confidence: 0.5, File: "a.go", LineStart: 1, LineEnd: 1,
				},
				{
					Title: "Nil dereference", Body: "crashes", Severity: review.SeverityCritical,
					Category: "bug", Confidence: 0.95, File: "b.go", LineStart: 10, LineEnd: 12,
					Suggestion: "if err != nil {\n\treturn err\n}",
				},
			},
		},
	}
	md := renderMarkdown(resp)

	if !strings.Contains(md, "Request Changes") {
		t.Error("missing verdict")
	}
	// Findings must be sorted by severity: critical before low.
	if strings.Index(md, "Nil dereference") > strings.Index(md, "Low issue") {
		t.Error("findings not sorted by severity")
	}
	if !strings.Contains(md, "`b.go`:10-12") {
		t.Error("missing location reference")
	}
	if !strings.Contains(md, "```suggestion") {
		t.Error("multi-line suggestion not fenced")
	}

	resp.Review.Findings = nil
	if md := renderMarkdown(resp); !strings.Contains(md, "No findings") {
		t.Error("expected empty-findings message")
	}
}

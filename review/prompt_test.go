package review

import (
	"strings"
	"testing"
)

func TestBuildUserMessageSections(t *testing.T) {
	req := &Request{
		Prompt: "Watch for SQL injection.",
		Diff:   "--- a/x.go\n+++ b/x.go\n@@ -1 +1 @@\n-old\n+new\n",
		Files:  []File{{Path: "x.go", Content: "package x\n\nvar v = 1\n"}},
		Metadata: map[string]string{
			"repository":   "acme/api",
			"pull_request": "#42",
		},
	}
	msg, warnings := buildUserMessage(req, DefaultMaxInputBytes)
	if len(warnings) != 0 {
		t.Errorf("unexpected warnings: %v", warnings)
	}

	for _, want := range []string{
		"- repository: acme/api",
		"- pull_request: #42",
		"Watch for SQL injection.",
		"```diff",
		"+new",
		"### x.go",
		"```go",
		"1| package x",
		"3| var v = 1",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("message missing %q\nmessage:\n%s", want, msg)
		}
	}

	// Metadata, prompt, diff, files: in that order.
	if strings.Index(msg, "repository") > strings.Index(msg, "SQL injection") ||
		strings.Index(msg, "SQL injection") > strings.Index(msg, "```diff") ||
		strings.Index(msg, "```diff") > strings.Index(msg, "### x.go") {
		t.Error("sections out of order")
	}
}

func TestBuildUserMessageTruncatesOversizedInput(t *testing.T) {
	bigLine := strings.Repeat("x", 100) + "\n"
	req := &Request{
		Diff:  strings.Repeat("+"+bigLine, 100),
		Files: []File{{Path: "big.go", Content: strings.Repeat(bigLine, 200)}},
	}
	const budget = 8 * 1024
	msg, warnings := buildUserMessage(req, budget)

	if len(msg) > budget+1024 {
		t.Errorf("message size %d exceeds budget %d by too much", len(msg), budget)
	}
	if len(warnings) == 0 {
		t.Fatal("expected truncation warnings")
	}
	if !strings.Contains(msg, "TRUNCATED") {
		t.Error("expected truncation notice in message")
	}
}

func TestBuildUserMessageOmitsFileWhenBudgetExhausted(t *testing.T) {
	req := &Request{
		Diff: strings.Repeat("+filler line for the diff\n", 200),
		Files: []File{
			{Path: "a.go", Content: "package a\n"},
		},
	}
	// Budget barely covers the diff; the file should be omitted with a warning.
	_, warnings := buildUserMessage(req, 2048)
	found := false
	for _, w := range warnings {
		if strings.Contains(w, "a.go") || strings.Contains(w, "truncated") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected truncation/omission warning, got %v", warnings)
	}
}

func TestNumberLines(t *testing.T) {
	got := numberLines("a\nb\nc\n")
	want := "1| a\n2| b\n3| c\n"
	if got != want {
		t.Errorf("numberLines = %q, want %q", got, want)
	}

	// Width aligns for files with 10+ lines.
	got = numberLines(strings.Repeat("l\n", 10))
	if !strings.Contains(got, " 1| l") || !strings.Contains(got, "10| l") {
		t.Errorf("expected aligned line numbers, got:\n%s", got)
	}
}

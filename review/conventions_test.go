package review

import (
	"context"
	"strings"
	"testing"
)

func TestDiscoverConventions(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, root+"/AGENTS.md", "Use table-driven tests.\n")
	mustWrite(t, root+"/CLAUDE.md", "Errors must be wrapped with %w.\n")
	mustWrite(t, root+"/CONTRIBUTING.md", "")           // empty: skipped
	mustWrite(t, root+"/README.md", "not a convention") // not probed

	content, found := DiscoverConventions(root)
	if len(found) != 2 || found[0] != "AGENTS.md" || found[1] != "CLAUDE.md" {
		t.Errorf("found = %v, want [AGENTS.md CLAUDE.md]", found)
	}
	for _, want := range []string{"### AGENTS.md", "table-driven", "### CLAUDE.md", "%w"} {
		if !strings.Contains(content, want) {
			t.Errorf("content missing %q", want)
		}
	}
	if strings.Contains(content, "not a convention") {
		t.Error("README.md must not be picked up")
	}

	// A single oversized file is clipped.
	big := t.TempDir()
	mustWrite(t, big+"/AGENTS.md", strings.Repeat("rule\n", 10000))
	content, _ = DiscoverConventions(big)
	if len(content) > maxConventionFileBytes+1024 {
		t.Errorf("oversized convention file not clipped: %d bytes", len(content))
	}
	if !strings.Contains(content, "[truncated]") {
		t.Error("expected truncation marker")
	}

	// No files: empty result.
	if content, found := DiscoverConventions(t.TempDir()); content != "" || found != nil {
		t.Errorf("empty root must yield nothing, got %q, %v", content, found)
	}
}

func TestConventionsInjectedIntoPrompt(t *testing.T) {
	req := defaultRequest()
	req.Conventions = "All exported functions need doc comments."
	msg, _ := buildUserMessage(req, DefaultMaxInputBytes)
	if !strings.Contains(msg, "## Project conventions") ||
		!strings.Contains(msg, "doc comments") {
		t.Errorf("conventions missing from prompt:\n%s", clip(msg, 400))
	}

	// "-" opts out.
	req.Conventions = "-"
	msg, _ = buildUserMessage(req, DefaultMaxInputBytes)
	if strings.Contains(msg, "## Project conventions") {
		t.Error("conventions section must be omitted for \"-\"")
	}
}

func TestWorkspaceConventionsAutoDiscovery(t *testing.T) {
	ws, root := newTestWorkspace(t)
	mustWrite(t, root+"/AGENTS.md", "Never use panic in library code.")

	model := &fakeModel{responses: []string{validReviewJSON}}
	r := newTestReviewer(t, model)

	req := defaultRequest()
	req.WorkspaceRoot = ws.root
	if _, err := r.Review(context.Background(), req); err != nil {
		t.Fatalf("Review: %v", err)
	}

	user := model.requests[0].Messages[1].OfUser
	if user == nil || !strings.Contains(user.Content.OfString.Value, "Never use panic") {
		t.Error("discovered conventions must reach the model prompt")
	}
	// The caller's request must not be mutated by discovery.
	if req.Conventions != "" {
		t.Errorf("request mutated: Conventions = %q", req.Conventions)
	}
}

func TestNormalizeFindings(t *testing.T) {
	rev := &Review{Findings: []Finding{
		{File: "b/internal/server.go", LineStart: 12, LineEnd: 10},
		{File: "./pkg/x.go", LineStart: -3, LineEnd: -1},
		{File: "a/y.go", LineStart: 5, LineEnd: 0},
	}}
	normalizeFindings(rev)

	if f := rev.Findings[0]; f.File != "internal/server.go" || f.LineStart != 10 || f.LineEnd != 12 {
		t.Errorf("finding 0 not normalized: %+v", f)
	}
	if f := rev.Findings[1]; f.File != "pkg/x.go" || f.LineStart != 0 || f.LineEnd != 0 {
		t.Errorf("finding 1 not normalized: %+v", f)
	}
	// LineEnd 0 means "not set"; it must not swap with a valid start.
	if f := rev.Findings[2]; f.File != "y.go" || f.LineStart != 5 || f.LineEnd != 0 {
		t.Errorf("finding 2 not normalized: %+v", f)
	}
}

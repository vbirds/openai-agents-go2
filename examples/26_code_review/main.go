// Example 26: Code review agent.
//
// Demonstrates the review package: reviewing a diff with full-file context
// using the built-in result schema, and again with a custom output schema.
//
// Run with: OPENAI_API_KEY=sk-... go run ./examples/26_code_review
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/MitulShah1/openai-agents-go/review"
)

const fileContent = `package store

import "database/sql"

type Store struct{ db *sql.DB }

func (s *Store) UserByName(name string) (*User, error) {
	row := s.db.QueryRow("SELECT id, name FROM users WHERE name = '" + name + "'")
	u := &User{}
	if err := row.Scan(&u.ID, &u.Name); err != nil {
		return nil, err
	}
	return u, nil
}

type User struct {
	ID   int
	Name string
}
`

const diff = `--- a/store/store.go
+++ b/store/store.go
@@ -5,6 +5,15 @@
 type Store struct{ db *sql.DB }

+func (s *Store) UserByName(name string) (*User, error) {
+	row := s.db.QueryRow("SELECT id, name FROM users WHERE name = '" + name + "'")
+	u := &User{}
+	if err := row.Scan(&u.ID, &u.Name); err != nil {
+		return nil, err
+	}
+	return u, nil
+}
`

func main() {
	if os.Getenv("OPENAI_API_KEY") == "" {
		log.Fatal("OPENAI_API_KEY is required")
	}

	reviewer, err := review.New(
		review.WithModel("gpt-4o"),
		review.WithTemperature(0.1),
		review.WithTimeout(2*time.Minute),
	)
	if err != nil {
		log.Fatal(err)
	}

	ctx := context.Background()

	// --- Built-in schema: typed findings ---------------------------------
	resp, err := reviewer.Review(ctx, &review.Request{
		Prompt: "This service handles untrusted user input. Pay extra attention to injection issues.",
		Diff:   diff,
		Files:  []review.File{{Path: "store/store.go", Content: fileContent}},
		Metadata: map[string]string{
			"repository": "acme/store",
			"change":     "add user lookup",
		},
	})
	if err != nil {
		log.Fatal(err)
	}

	fmt.Printf("Verdict: %s (confidence %.0f%%)\n", resp.Review.Verdict, resp.Review.Confidence*100)
	fmt.Printf("Summary: %s\n\n", resp.Review.Summary)
	for _, f := range resp.Review.Findings {
		fmt.Printf("[%s/%s] %s:%d-%d %s\n  %s\n",
			f.Severity, f.Category, f.File, f.LineStart, f.LineEnd, f.Title, f.Body)
		if f.Suggestion != "" {
			fmt.Printf("  Suggestion: %s\n", f.Suggestion)
		}
	}
	fmt.Printf("\nTokens: %d, duration: %s\n\n", resp.Usage.TotalTokens, resp.Duration.Round(time.Millisecond))

	// --- Custom output schema --------------------------------------------
	customSchema := json.RawMessage(`{
		"type": "object",
		"additionalProperties": false,
		"properties": {
			"merge_safe": {"type": "boolean", "description": "Whether this change is safe to merge as-is."},
			"risk_level": {"type": "string", "enum": ["low", "medium", "high", "critical"]},
			"blockers": {
				"type": "array",
				"description": "Issues that must be fixed before merging.",
				"items": {"type": "string"}
			}
		},
		"required": ["merge_safe", "risk_level", "blockers"]
	}`)

	resp2, err := reviewer.Review(ctx, &review.Request{
		Diff:       diff,
		Files:      []review.File{{Path: "store/store.go", Content: fileContent}},
		Schema:     customSchema,
		SchemaName: "merge_gate",
	})
	if err != nil {
		log.Fatal(err)
	}

	var gate struct {
		MergeSafe bool     `json:"merge_safe"`
		RiskLevel string   `json:"risk_level"`
		Blockers  []string `json:"blockers"`
	}
	if err := json.Unmarshal(resp2.Output, &gate); err != nil {
		log.Fatal(err)
	}
	fmt.Printf("Merge gate: safe=%v risk=%s blockers=%v\n", gate.MergeSafe, gate.RiskLevel, gate.Blockers)
}

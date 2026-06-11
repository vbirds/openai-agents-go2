// Package review provides a production-ready code review agent built on top
// of the openai-agents-go SDK.
//
// The package accepts a set of files, a unified diff, an optional reviewer
// prompt, and an optional output JSON Schema, and returns a review result
// that conforms to that schema.
//
// Basic usage with the built-in result schema:
//
//	reviewer, err := review.New(review.WithAPIKey(os.Getenv("OPENAI_API_KEY")))
//	if err != nil { ... }
//
//	resp, err := reviewer.Review(ctx, &review.Request{
//		Prompt: "Focus on concurrency bugs.",
//		Diff:   diffText,
//		Files:  []review.File{{Path: "server.go", Content: src}},
//	})
//	if err != nil { ... }
//	for _, f := range resp.Review.Findings {
//		fmt.Printf("[%s] %s:%d %s\n", f.Severity, f.File, f.LineStart, f.Title)
//	}
//
// Custom output schemas are supported by setting Request.Schema to a raw JSON
// Schema document. When the schema only uses keywords supported by OpenAI
// structured outputs it is enforced natively by the model; otherwise the
// schema is embedded into the prompt and the output is validated client-side.
// In both cases the returned Response.Output is guaranteed to validate
// against the schema.
package review

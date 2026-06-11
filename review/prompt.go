package review

import (
	"fmt"
	"path"
	"sort"
	"strings"
)

// DefaultInstructions is the system prompt used by the reviewer. It can be
// replaced with WithInstructions or extended with WithExtraInstructions.
const DefaultInstructions = `You are an expert code reviewer operating inside an automated review pipeline. You are given a code change (a unified diff), optionally the full contents of the affected files for context, and optionally extra guidance from the requester.

Review the CHANGE, not the whole codebase: flag issues introduced or made worse by the diff. Use the full file contents only to understand context. Pre-existing problems are worth a finding only if the change directly interacts with them, and must be clearly labeled as pre-existing.

Prioritize, in order:
1. Correctness: logic errors, broken edge cases, race conditions, resource leaks, error-handling mistakes, broken invariants.
2. Security: injection, authn/authz flaws, secrets in code, unsafe deserialization, path traversal, SSRF.
3. Reliability and performance: unbounded growth, missing timeouts, N+1 patterns, blocking calls on hot paths.
4. Maintainability, tests, and documentation: only when the issue is concrete and actionable.

Rules for findings:
- Every finding must be anchored: reference the file path exactly as given, and line numbers in the POST-CHANGE version of the file. Use the line numbers shown in the provided file contents and diff hunk headers; do not guess.
- Be precise and concrete. Explain WHY it is a problem and what the impact is, not just what to change.
- Do not pad the review: zero findings is a valid result for a clean change. Do not invent style nits to appear thorough.
- Do not report issues you cannot substantiate from the provided code. If you are unsure, lower the confidence score instead of omitting the uncertainty.
- One finding per distinct issue; do not bundle unrelated problems.

Respond with a single JSON document in the required output format and nothing else: no markdown fences, no commentary before or after.`

// explorationInstructions is appended to the system prompt when a workspace
// is attached, turning the single-shot reviewer into an agentic one.
const explorationInstructions = `

You have read-only tools to explore the repository the change belongs to: read_file (line-numbered file contents), grep (regex search returning path:line matches), and list_dir. The diff and any provided file contents are a starting point, not the whole picture - use the tools to verify your findings against the actual codebase.

Investigation strategy:
1. Identify every function, type, constant, and interface the diff adds, removes, or changes behaviorally (signatures, return values, error semantics, locking, validation).
2. For each changed symbol, grep for its usages. Read the call sites: do callers still hold after this change? Look for missed updates, violated assumptions, and broken invariants.
3. Check the tests covering the changed code (grep for the symbol in *_test.* or test directories). Note behavior changes that no test covers.
4. When the diff touches error handling, concurrency, or resource lifetimes, read enough surrounding code to confirm whether the issue is real before reporting it.
5. Investigate before asserting: a finding you verified against call sites deserves high confidence; one you could not verify must say so and carry low confidence.

Budget your exploration: prefer a few targeted grep/read_file calls over reading whole directories, and stop exploring once additional reads stop changing your conclusions. When you are done investigating, output the final JSON review - do not call tools in your final response and do not narrate your exploration.`

// promptModeInstructions spells the output schema out in the system prompt.
// It is always appended - even when response_format enforces the schema
// natively - because OpenAI-compatible backends routinely ignore
// response_format, and a model that never saw the schema cannot comply.
const promptModeInstructions = `

Your entire response MUST be a single JSON document that validates against the following JSON Schema. Output raw JSON only - no markdown code fences, no surrounding text.

OUTPUT JSON SCHEMA:
%s`

// truncationNotice is inserted where input was cut to fit the byte budget.
const truncationNotice = "\n... [TRUNCATED: input exceeded the size budget; %d more bytes omitted] ...\n"

// buildUserMessage assembles the user message from the request, enforcing
// maxBytes across the variable-size sections. It returns the message and any
// warnings (e.g. truncation) to surface to the caller.
func buildUserMessage(req *Request, maxBytes int) (string, []string) {
	var warnings []string
	var b strings.Builder

	if len(req.Metadata) > 0 {
		b.WriteString("## Context\n\n")
		keys := make([]string, 0, len(req.Metadata))
		for k := range req.Metadata {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			fmt.Fprintf(&b, "- %s: %s\n", k, req.Metadata[k])
		}
		b.WriteString("\n")
	}

	if conv := strings.TrimSpace(req.Conventions); conv != "" && conv != "-" {
		b.WriteString("## Project conventions and guidelines\n\nFollow these project-specific rules when judging the change:\n\n")
		text, w := truncateTo(conv, remaining(maxBytes, b.Len())/4, "project conventions")
		warnings = append(warnings, w...)
		b.WriteString(text)
		b.WriteString("\n\n")
	}

	if strings.TrimSpace(req.Prompt) != "" {
		b.WriteString("## Review instructions from the requester\n\n")
		b.WriteString(strings.TrimSpace(req.Prompt))
		b.WriteString("\n\n")
	}

	// The diff is the subject of the review; give it the budget first.
	if strings.TrimSpace(req.Diff) != "" {
		b.WriteString("## Diff under review (unified format)\n\n```diff\n")
		diff, w := truncateTo(req.Diff, remaining(maxBytes, b.Len()), "diff")
		warnings = append(warnings, w...)
		b.WriteString(diff)
		if !strings.HasSuffix(diff, "\n") {
			b.WriteString("\n")
		}
		b.WriteString("```\n\n")
	}

	if len(req.Files) > 0 {
		b.WriteString("## Full file contents (post-change), with line numbers\n\n")
		for _, f := range req.Files {
			header := fmt.Sprintf("### %s\n\n```%s\n", f.Path, languageOf(f))
			budget := remaining(maxBytes, b.Len()+len(header))
			if budget <= 0 {
				warnings = append(warnings,
					fmt.Sprintf("file %s omitted: input size budget exhausted", f.Path))
				continue
			}
			body, w := truncateTo(numberLines(f.Content), budget, "file "+f.Path)
			warnings = append(warnings, w...)
			b.WriteString(header)
			b.WriteString(body)
			if !strings.HasSuffix(body, "\n") {
				b.WriteString("\n")
			}
			b.WriteString("```\n\n")
		}
	}

	b.WriteString("Produce the review now in the required JSON output format.")
	return b.String(), warnings
}

// remaining computes the budget left for a section, reserving a small margin
// for fixed trailing text.
func remaining(maxBytes, used int) int {
	const reserve = 256
	return maxBytes - used - reserve
}

// truncateTo cuts s to at most budget bytes at a line boundary, appending a
// truncation notice and recording a warning labeled with what was cut.
func truncateTo(s string, budget int, label string) (string, []string) {
	if len(s) <= budget {
		return s, nil
	}
	if budget < 0 {
		budget = 0
	}
	cut := s[:budget]
	if i := strings.LastIndexByte(cut, '\n'); i > 0 {
		cut = cut[:i+1]
	}
	omitted := len(s) - len(cut)
	return cut + fmt.Sprintf(truncationNotice, omitted),
		[]string{fmt.Sprintf("%s truncated: %d bytes omitted to fit the input size budget", label, omitted)}
}

// numberLines prefixes each line of content with its 1-based line number so
// the model can produce accurate locations.
func numberLines(content string) string {
	lines := strings.Split(content, "\n")
	// Drop a sole trailing empty element produced by a final newline.
	if n := len(lines); n > 1 && lines[n-1] == "" {
		lines = lines[:n-1]
	}
	width := len(fmt.Sprint(len(lines)))
	var b strings.Builder
	b.Grow(len(content) + len(lines)*(width+2))
	for i, line := range lines {
		fmt.Fprintf(&b, "%*d| %s\n", width, i+1, line)
	}
	return b.String()
}

// languageOf returns the fenced-code-block language tag for a file.
func languageOf(f File) string {
	if f.Language != "" {
		return f.Language
	}
	switch strings.ToLower(path.Ext(f.Path)) {
	case ".go":
		return "go"
	case ".py":
		return "python"
	case ".js", ".mjs", ".cjs":
		return "javascript"
	case ".ts", ".tsx":
		return "typescript"
	case ".java":
		return "java"
	case ".rb":
		return "ruby"
	case ".rs":
		return "rust"
	case ".c", ".h":
		return "c"
	case ".cc", ".cpp", ".hpp":
		return "cpp"
	case ".cs":
		return "csharp"
	case ".sh":
		return "bash"
	case ".sql":
		return "sql"
	case ".yaml", ".yml":
		return "yaml"
	case ".json":
		return "json"
	default:
		return ""
	}
}

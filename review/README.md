# review — Code Review Agent

A production-ready code review agent built on the openai-agents-go SDK. Give it
files, a unified diff, an optional reviewer prompt, and an optional output JSON
Schema; get back a review result that is **guaranteed to validate against that
schema**.

Two ways to use it:

- **Go API** — the `review` package, for embedding in services and bots.
- **CLI** — `cmd/codereview`, for terminals and CI pipelines.

## Design

```
            ┌────────────────────── review.Reviewer ──────────────────────┐
 Request    │                                                             │
 ──────────►│ validate ─► compile schema ─► build prompt ─► agent loop ─┐ │
 diff,files │              │                  (truncation       │  ▲    │ │
 prompt,    │              │ native? ──────►   + line nums)     ▼  │    │ │
 schema,    │              │ prompt-mode? ─►  schema in    read_file    │ │
 workspace  │              ▼                  prompt       grep         │ │
            │                                              list_dir     │ │
            │                                            (sandboxed,    ▼ │
            │        client-side JSON Schema validation ◄── extract JSON  │
            │              │ invalid? feed error back, retry (≤ N)        │
            └──────────────┼──────────────────────────────────────────────┘
                           ▼
              Response{Output, Review, Usage, Turns, ToolCalls, ...}
```

Deep-review mode (`WithDeepReview` / `-deep`) replaces the single agent run
with a multi-agent pipeline:

```
                       ┌─ correctness ──┐
 diff ─► triage ───────┼─ security ─────┼─► adjudicator ─► final JSON
        (rules first,  ├─ concurrency ──┤   verifies every candidate
         LLM for the   └─ custom... ────┘   against the code, dedups,
         undecided)      parallel,          calibrates severity and
                         category-gated     confidence, synthesizes
```

Key decisions:

- **Specialists are data, not code.** Built-ins (correctness, security,
  concurrency, performance) and externally configured specialists follow the
  same execution path. Configure via Go options (`WithSpecialists`,
  `AddSpecialist`, `DisableSpecialists`), per-request (`Request.Specialists`),
  or a repo-level `.codereview.yaml` (`WithConfigFile`, auto-discovered by
  the CLI at the workspace root). Precedence: request > options/config file >
  built-ins (options apply in order).
- **Rules before LLM.** Specialist triggers (`always`, path globs, diff
  keywords) are evaluated in code; only undecided specialists go to a
  single-shot LLM triage (use `WithTriageModel` for a cheaper model). Triage
  failure degrades to running everything — recall-safe, because of the next
  point.
- **The adjudicator is the quality gate.** Specialist findings never reach
  the user directly: a category whitelist filters out-of-mandate findings in
  code, then an adjudicator (with workspace tools, when available) re-checks
  every candidate against the code, drops what it cannot verify, merges
  duplicates, calibrates severity/confidence, and synthesizes the final
  result in the requested schema. Specialist failures degrade to warnings,
  never failed reviews.
- **Agentic exploration (optional).** Setting `Request.WorkspaceRoot` gives
  the reviewer read-only tools — `read_file` (line-numbered), `grep` (RE2,
  `path:line` matches), `list_dir` — sandboxed to the project directory
  (symlink escapes and `..` traversal are rejected; `.git`, `node_modules`
  etc. are excluded). The agent follows a built-in investigation strategy:
  identify changed symbols, grep their usages, read call sites and tests,
  verify findings against real code before reporting them. Turn budget is
  capped (`WithMaxTurns`, default 16). Without a workspace the review is
  single-shot — cheaper, and usable where the filesystem isn't available
  (e.g. reviewing webhook-delivered diffs server-side).

- **Schema-first output.** When no schema is given, a built-in review schema is
  used (codex-cli-inspired: verdict + severity-ranked findings anchored to
  file/line locations) and the result is also returned as a typed
  `*review.Review`.
- **Dual schema enforcement.** Schemas that fit OpenAI structured outputs are
  enforced natively via `response_format` (with strict mode when the schema
  qualifies). Schemas using unsupported keywords (`oneOf`, `$ref`, `format`,
  ...) automatically fall back to prompt-embedded enforcement. In **both**
  modes the output is validated client-side with a full JSON Schema validator,
  so non-OpenAI backends behind `OPENAI_BASE_URL` are covered too.
- **Self-correcting retries.** Invalid output is fed back to the model with the
  validation error for up to `WithMaxOutputRetries` corrective rounds
  (default 2). Transport-level retries are already handled by the OpenAI SDK.
- **Bounded input.** The rendered prompt is capped (`WithMaxInputBytes`,
  default 400 KiB), prioritizing the diff over file contents; anything cut is
  reported in `Response.Warnings`. File contents are line-numbered so findings
  carry accurate locations.
- **Operational hygiene.** Context/timeout aware, `slog` logging, accumulated
  token usage across retries, sentinel errors (`ErrInvalidRequest`,
  `ErrInvalidSchema`, `ErrInvalidOutput`) for programmatic handling.

## Go API

```go
import "github.com/MitulShah1/openai-agents-go/review"

reviewer, err := review.New(
    review.WithAPIKey(os.Getenv("OPENAI_API_KEY")), // optional; env is read by default
    review.WithModel("gpt-4o"),
    review.WithTemperature(0.1),
    review.WithTimeout(2*time.Minute),
)
if err != nil { ... }

resp, err := reviewer.Review(ctx, &review.Request{
    Prompt: "Focus on concurrency and error handling.",
    Diff:   diffText, // unified diff
    Files:  []review.File{{Path: "server.go", Content: src}},
    Metadata: map[string]string{"repository": "acme/api", "pull_request": "#42"},
})
if err != nil { ... }

fmt.Println(resp.Review.Verdict) // approve | request_changes | comment
for _, f := range resp.Review.Findings {
    fmt.Printf("[%s] %s:%d %s\n", f.Severity, f.File, f.LineStart, f.Title)
}
```

### Agentic exploration

Point the reviewer at the project root and it will autonomously inspect
callers, usages, and tests of the changed code (read-only) before judging:

```go
resp, err := reviewer.Review(ctx, &review.Request{
    Diff:          diffText,
    WorkspaceRoot: "/path/to/repo", // enables read_file / grep / list_dir
})
// resp.Turns and resp.ToolCalls report how much exploration happened.
```

### Deep review (multi-agent)

```go
reviewer, err := review.New(
    review.WithDeepReview(true),
    review.WithTriageModel("gpt-4o-mini"),          // cheap routing
    review.AddSpecialist(review.Specialist{         // org-specific expert
        Name:        "api-compat",
        Description: "Detects breaking changes to exported APIs.",
        Instructions: "You check exclusively for breaking API changes: ...",
        Categories:  []string{"bug", "other"},
        Triggers:    review.Trigger{Paths: []string{"api/**"}},
    }),
    review.DisableSpecialists("performance"),
)
// resp.Specialists lists who ran; warnings record any stage degradations.
```

Or configure per repository with `.codereview.yaml` (CLI auto-discovers it
at the `-workspace` root):

```yaml
specialists:
  - name: api-compat
    description: Detects breaking changes to exported APIs.
    triggers:
      paths: ["api/**", "**/*.proto"]
    instructions: |
      You check exclusively for breaking API changes: ...
    categories: [bug, other]
  - name: security      # tune a built-in: only set fields override
    model: gpt-4o
disable: [performance]
```

### Custom output schema

```go
resp, err := reviewer.Review(ctx, &review.Request{
    Diff:       diffText,
    Schema:     json.RawMessage(`{"type":"object","properties":{...},"required":[...]}`),
    SchemaName: "merge_gate",
})
// resp.Output is raw JSON validated against your schema; resp.Review is nil.
```

### Custom backends

```go
review.WithBaseURL("https://my-gateway/v1") // OpenAI-compatible endpoint
review.WithClient(&client)                  // pre-configured *openai.Client
review.WithModelProvider(provider)          // any models.ModelProvider
```

See [`examples/26_code_review`](../examples/26_code_review/main.go) for a
runnable example.

## CLI

```bash
go install github.com/MitulShah1/openai-agents-go/cmd/codereview@latest
export OPENAI_API_KEY=sk-...
```

```bash
# Review the last commit (changed files are loaded automatically)
codereview -git HEAD~1..HEAD

# Agentic review: explore the project (read-only) to verify callers,
# usages, and tests of the changed code before judging it
codereview -git HEAD~1..HEAD -workspace .

# Deep review: parallel specialists + verification, with repo-level
# specialist config auto-discovered at the workspace root
codereview -git origin/main..HEAD -workspace . -deep

# Subversion: review uncommitted working-copy changes, a committed
# revision, or a revision range
codereview -svn wc
codereview -svn 12345
codereview -svn 12300:12345

# Review a patch with guidance, render markdown for a PR comment
codereview -diff change.patch -prompt "Focus on security" -format markdown -out review.md

# Review staged changes from stdin with a custom output schema
git diff --cached | codereview -diff - -schema merge_gate.json

# CI gate: exit 3 when findings of severity >= high exist
codereview -git origin/main..HEAD -fail-on high
```

| Flag | Description |
|------|-------------|
| `-diff path` | Unified diff file, `-` for stdin |
| `-git range` | Review a git revision range; loads changed files as context |
| `-svn target` | Review svn changes: `wc` (working copy), `N:M` (revision range), or a revision number; loads changed files as context |
| `[file ...]` | Positional args: extra files included as full-file context |
| `-prompt` / `-prompt-file` | Reviewer guidance |
| `-schema path` | Custom output JSON Schema (default: built-in review schema) |
| `-workspace path` | Enable agentic exploration rooted at this directory (read-only) |
| `-max-turns n` | Exploration turn budget (default 16; requires `-workspace`) |
| `-deep` | Multi-agent pipeline: triage → parallel specialists → adjudicator |
| `-review-config path` | Specialist config file (default: `.codereview.yaml` at the workspace root) |
| `-model`, `-base-url`, `-temperature`, `-max-output-tokens` | Model settings (env: `OPENAI_MODEL`, `OPENAI_BASE_URL`) |
| `-timeout`, `-max-retries`, `-max-input-kb` | Run limits |
| `-format json\|markdown`, `-out path` | Output control |
| `-fail-on severity` | Exit 3 on findings at/above `critical\|high\|medium\|low\|info` |
| `-v` | Verbose logging to stderr |

Exit codes: `0` success · `1` usage error · `2` review failed · `3` findings at
or above the `-fail-on` threshold.

## Built-in result schema

```json
{
  "summary": "Short overall assessment.",
  "verdict": "approve | request_changes | comment",
  "confidence": 0.9,
  "findings": [
    {
      "title": "One-line issue summary",
      "body": "Why it matters and the impact.",
      "severity": "critical | high | medium | low | info",
      "category": "bug | security | performance | style | maintainability | testing | documentation | other",
      "confidence": 0.85,
      "file": "path/as/in/diff.go",
      "line_start": 42,
      "line_end": 44,
      "suggestion": "Concrete fix, or empty string."
    }
  ]
}
```

`review.DefaultSchema()` returns the full schema document.

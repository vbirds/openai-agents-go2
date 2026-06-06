# 26 — Code Review Agent

A minimal-but-real code review agent built on the SDK. It reads a unified diff,
reviews it against the **reviewed repository's own convention files**, and emits a
structured, CI-consumable report.

## Run

```bash
export OPENAI_API_KEY=...

# Review a saved diff
go run . -repo /path/to/repo -diff change.diff

# Or pipe a diff in
git diff main... | go run . -repo .
```

Flags: `-repo` (repo root, default `.`), `-diff` (diff file, default stdin),
`-model` (default `gpt-4o`).

The process exits `1` when the verdict is `request_changes` or any
critical/high finding exists — wire this into CI to gate merges.

## What it demonstrates

- **Structured output** — `ReviewReport{summary, verdict, findings[]}` via a
  `jsonschema` `ResponseFormat`, parsed back into Go structs.
- **Tools** — `read_file` (full file with line numbers, path-traversal guarded),
  `list_changed_files`, and `read_conventions`.
- **Automatic convention loading** — the SDK does **not** auto-read any
  `AGENT.md`/`CLAUDE.md`-style file. This example loads them explicitly from the
  reviewed repo and injects them into the agent instructions so the rules apply on
  every run. The candidate files are `AGENT.md`, `AGENTS.md`, `CLAUDE.md`,
  `CONTRIBUTING.md`, `.cursorrules`, `docs/CODE_REVIEW.md`. Inlined text is capped
  (token control); the full text stays reachable via the `read_conventions` tool.
- **Input guardrails** — committed-secret detection and prompt-injection defense,
  because diff/PR content is untrusted external input.
- **Hallucination filtering** — findings pointing at files outside the diff are
  dropped before reporting.

## Notes / limitations

- Auto-fix is intentionally **not** included: the repo's `diff.Apply` patch
  applicator is currently a skeleton. This example only produces review comments
  and textual suggestions.
- For larger setups, split the single reviewer into a Triage + specialized
  reviewers (`handoff` package) and post findings as inline PR comments via the
  GitHub integration.

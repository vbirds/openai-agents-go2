package review

import (
	"fmt"
	"regexp"
	"strings"
)

// Trigger controls when a specialist is selected for a review. Rule-based
// triggers (Always, Paths, Keywords) are evaluated in code before any model
// call; a specialist with no rules defined is offered to the LLM triage
// stage, which decides based on its Description.
type Trigger struct {
	// Always unconditionally selects the specialist.
	Always bool `yaml:"always" json:"always,omitempty"`

	// Paths selects the specialist when any changed file matches one of
	// these glob patterns ("**" matches across directories), e.g.
	// "api/**" or "**/*.sql".
	Paths []string `yaml:"paths" json:"paths,omitempty"`

	// Keywords selects the specialist when the diff contains any of these
	// case-insensitive substrings, e.g. "go func" or "password".
	Keywords []string `yaml:"keywords" json:"keywords,omitempty"`
}

// rulesDefined reports whether any rule-based trigger is configured.
func (t Trigger) rulesDefined() bool {
	return t.Always || len(t.Paths) > 0 || len(t.Keywords) > 0
}

// matches evaluates the rule-based triggers against the changed paths and
// diff content.
func (t Trigger) matches(changedPaths []string, diff string) bool {
	if t.Always {
		return true
	}
	for _, pattern := range t.Paths {
		for _, p := range changedPaths {
			if globMatch(pattern, p) {
				return true
			}
		}
	}
	if len(t.Keywords) > 0 {
		lower := strings.ToLower(diff)
		for _, kw := range t.Keywords {
			if kw != "" && strings.Contains(lower, strings.ToLower(kw)) {
				return true
			}
		}
	}
	return false
}

// Specialist is a focused reviewer participating in deep-review mode.
// Specialists are data, not code: the built-in roster and externally
// configured ones (WithSpecialists, AddSpecialist, WithConfigFile,
// Request.Specialists) follow exactly the same execution path.
type Specialist struct {
	// Name identifies the specialist, e.g. "security".
	Name string `yaml:"name" json:"name"`

	// Description tells the triage agent when this specialist is relevant.
	// Required when no rule-based Triggers are defined.
	Description string `yaml:"description" json:"description"`

	// Instructions is the specialist's focused system prompt: what to hunt
	// for and what is out of scope. The shared review ground rules and
	// exploration guidance are added automatically.
	Instructions string `yaml:"instructions" json:"instructions"`

	// Categories whitelists the finding categories this specialist may
	// produce (see the built-in schema's category enum). Findings outside
	// the whitelist are dropped in code before adjudication. Empty allows
	// all categories.
	Categories []string `yaml:"categories" json:"categories,omitempty"`

	// Triggers controls when the specialist runs; see Trigger.
	Triggers Trigger `yaml:"triggers" json:"triggers,omitempty"`

	// Model optionally overrides the reviewer's model for this specialist.
	Model string `yaml:"model" json:"model,omitempty"`

	// MaxTurns optionally overrides the exploration turn budget for this
	// specialist (only effective with a workspace).
	MaxTurns int `yaml:"max_turns" json:"max_turns,omitempty"`
}

// Validate checks that the specialist is well-formed.
func (s *Specialist) Validate() error {
	if strings.TrimSpace(s.Name) == "" {
		return fmt.Errorf("review: specialist name must not be empty")
	}
	if strings.TrimSpace(s.Instructions) == "" {
		return fmt.Errorf("review: specialist %q must have instructions", s.Name)
	}
	if !s.Triggers.rulesDefined() && strings.TrimSpace(s.Description) == "" {
		return fmt.Errorf("review: specialist %q needs a description (used by triage) or rule-based triggers", s.Name)
	}
	if s.MaxTurns < 0 {
		return fmt.Errorf("review: specialist %q max_turns must not be negative", s.Name)
	}
	return nil
}

// BuiltinSpecialists returns the default specialist roster used by
// deep-review mode. The returned slice is a fresh copy that callers may
// modify freely.
func BuiltinSpecialists() []Specialist {
	return []Specialist{
		{
			Name:        "correctness",
			Description: "Logic errors, broken edge cases, error-handling mistakes, and violated invariants. Relevant to every change.",
			Instructions: `You are a correctness specialist. Hunt exclusively for ways this change produces wrong behavior:
- logic errors, inverted or off-by-one conditions, broken edge cases (empty, nil, zero, overflow, unicode)
- error handling: swallowed errors, wrong error propagation, missing cleanup on error paths
- violated invariants and broken caller assumptions: signatures, nil-ability, ordering, units, time zones
- API misuse of the standard library or project-internal helpers
- behavior changes that existing tests do not cover

Out of scope for you: style, naming, formatting, performance, and architecture opinions. Do not report them.`,
			Categories: []string{"bug", "testing", "maintainability", "other"},
			Triggers:   Trigger{Always: true},
		},
		{
			Name:        "security",
			Description: "Injection, authentication/authorization flaws, secrets handling, unsafe input processing, and other vulnerabilities.",
			Instructions: `You are a security specialist. Hunt exclusively for vulnerabilities introduced or worsened by this change:
- injection: SQL, command, template, header, log injection from untrusted input
- authn/authz: missing or weakened permission checks, confused-deputy patterns, insecure session handling
- secrets: credentials or tokens in code, logs, or error messages
- unsafe input processing: path traversal, SSRF, unsafe deserialization, zip slip, unchecked sizes
- crypto misuse: weak algorithms, predictable randomness, missing TLS verification

Treat every external value (request params, file contents, env, DB values) as attacker-controlled until you verify sanitization. Out of scope for you: bugs without a security impact, style, performance.`,
			Categories: []string{"security", "bug"},
			Triggers: Trigger{Keywords: []string{
				"sql", "query", "exec", "password", "token", "secret", "auth",
				"http", "url", "crypto", "rand", "unmarshal", "deserialize",
				"upload", "path", "cookie", "session", "cmd",
			}},
		},
		{
			Name:        "concurrency",
			Description: "Data races, deadlocks, goroutine leaks, and unsafe shared-state access in concurrent code.",
			Instructions: `You are a concurrency specialist. Hunt exclusively for concurrency defects introduced or worsened by this change:
- data races: shared state written without synchronization, loop-variable capture, lazily initialized fields
- locking: missing unlocks (especially on error paths), lock-ordering deadlocks, locks held across blocking calls
- channels and goroutines: leaks (blocked forever sends/receives), missing context cancellation, unbounded spawning
- misuse of sync primitives: copied mutexes/WaitGroups, Add after Wait, atomics mixed with plain access

Out of scope for you: anything not related to concurrent execution.`,
			Categories: []string{"bug", "performance"},
			Triggers: Trigger{Keywords: []string{
				"go func", "goroutine", "mutex", "sync.", "atomic", "chan ",
				"channel", "waitgroup", "lock", "once.", "context.",
			}},
		},
		{
			Name:        "performance",
			Description: "Algorithmic regressions, N+1 query patterns, unbounded memory growth, missing timeouts, and hot-path inefficiencies. Relevant when the change touches loops, queries, caches, or I/O.",
			Instructions: `You are a performance specialist. Hunt exclusively for performance and resource problems introduced or worsened by this change:
- algorithmic regressions: accidental O(n^2), repeated work inside loops, queries or RPCs per loop iteration (N+1)
- memory: unbounded growth of maps/slices/caches, large allocations on hot paths, missing pooling where it existed before
- I/O and reliability under load: missing timeouts or contexts, unbatched writes, sync I/O on hot paths, unclosed resources

Only report issues with a plausible real-world impact; do not report micro-optimizations. Out of scope for you: correctness, security, style.`,
			Categories: []string{"performance", "bug"},
		},
	}
}

// matchPathsFromRequest collects the changed file paths visible in a
// request: explicit file paths plus paths referenced by the diff headers.
func matchPathsFromRequest(req *Request) []string {
	seen := map[string]bool{}
	var paths []string
	add := func(p string) {
		p = strings.TrimSpace(p)
		if p == "" || p == "/dev/null" || seen[p] {
			return
		}
		seen[p] = true
		paths = append(paths, p)
	}
	for _, f := range req.Files {
		add(f.Path)
	}
	for _, line := range strings.Split(req.Diff, "\n") {
		switch {
		case strings.HasPrefix(line, "+++ "), strings.HasPrefix(line, "--- "):
			p := strings.TrimSpace(line[4:])
			// Strip git's a/ and b/ prefixes and trailing tab metadata.
			if i := strings.IndexByte(p, '\t'); i >= 0 {
				p = p[:i]
			}
			p = strings.TrimPrefix(p, "a/")
			p = strings.TrimPrefix(p, "b/")
			add(p)
		case strings.HasPrefix(line, "Index: "):
			add(line[len("Index: "):])
		}
	}
	return paths
}

// globMatch matches a path against a glob pattern where "*" and "?" do not
// cross "/" boundaries and "**" matches any number of path segments.
func globMatch(pattern, path string) bool {
	var b strings.Builder
	b.WriteString("^")
	for i := 0; i < len(pattern); i++ {
		switch c := pattern[i]; c {
		case '*':
			if i+1 < len(pattern) && pattern[i+1] == '*' {
				b.WriteString(".*")
				i++
			} else {
				b.WriteString("[^/]*")
			}
		case '?':
			b.WriteString("[^/]")
		default:
			b.WriteString(regexp.QuoteMeta(string(c)))
		}
	}
	b.WriteString("$")
	re, err := regexp.Compile(b.String())
	if err != nil {
		return false
	}
	return re.MatchString(path)
}

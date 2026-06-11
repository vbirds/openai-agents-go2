// Package eval provides a regression harness for measuring code review
// quality: a corpus of changes with known expected findings is run through
// a Reviewer configuration, and the results are scored for recall (expected
// issues found), noise (unexpected findings), and verdict accuracy.
//
// A case is a directory:
//
//	cases/<name>/
//	    case.yaml      metadata and expectations
//	    diff.patch     the change under review
//	    files/         optional full-file context (post-change), tree mirrors paths
//	    workspace/     optional project tree enabling agentic exploration
//
// Because results come from a live model, eval runs are integration tests:
// they need an API key, cost tokens, and are not perfectly deterministic.
// Use them to compare configurations and catch quality regressions, not as
// unit tests.
package eval

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/MitulShah1/openai-agents-go/review"
)

// Case is one evaluation scenario.
type Case struct {
	// Name is the case directory name.
	Name string `yaml:"-"`
	// Dir is the absolute case directory.
	Dir string `yaml:"-"`

	// Description says what the case exercises.
	Description string `yaml:"description"`
	// Prompt is optional reviewer guidance passed with the request.
	Prompt string `yaml:"prompt"`
	// Expect defines the scoring expectations.
	Expect Expectation `yaml:"expect"`
}

// Expectation scores a review result.
type Expectation struct {
	// Clean asserts the change is fine: any finding at or above medium
	// severity counts as unexpected noise.
	Clean bool `yaml:"clean"`

	// Verdicts lists acceptable verdicts (empty: not checked).
	Verdicts []string `yaml:"verdicts"`

	// Findings are issues the review must report.
	Findings []ExpectedFinding `yaml:"findings"`

	// Forbid lists findings that must NOT be reported (false-positive
	// traps).
	Forbid []ForbiddenFinding `yaml:"forbid"`
}

// ExpectedFinding describes one issue the review must catch. A reported
// finding matches when every specified field matches.
type ExpectedFinding struct {
	// ID names the expectation in reports.
	ID string `yaml:"id"`
	// File matches the finding's file path (path-boundary suffix match;
	// empty matches any file).
	File string `yaml:"file"`
	// Lines is the [start, end] region the finding must overlap (with a
	// small tolerance). Empty disables the line check.
	Lines []int `yaml:"lines"`
	// Categories lists acceptable categories (empty: any).
	Categories []string `yaml:"categories"`
	// Severities lists acceptable severities (empty: any).
	Severities []string `yaml:"severities"`
	// Keywords requires at least one (case-insensitive) hit in the
	// finding's title or body (empty: any).
	Keywords []string `yaml:"keywords"`
}

// ForbiddenFinding describes findings that must not appear. A reported
// finding is a violation when every specified field matches.
type ForbiddenFinding struct {
	// ID names the trap in reports.
	ID string `yaml:"id"`
	// File matches the finding's file path (empty: any).
	File string `yaml:"file"`
	// Keywords requires at least one hit in title or body (empty: any).
	Keywords []string `yaml:"keywords"`
	// MinSeverity ignores findings below this severity (default "info":
	// everything counts).
	MinSeverity string `yaml:"min_severity"`
}

// Validate checks the case definition.
func (c *Case) Validate() error {
	if !c.Expect.Clean && len(c.Expect.Findings) == 0 && len(c.Expect.Verdicts) == 0 && len(c.Expect.Forbid) == 0 {
		return fmt.Errorf("eval: case %s: expectations are empty (set clean, findings, verdicts, or forbid)", c.Name)
	}
	for i, f := range c.Expect.Findings {
		if f.ID == "" {
			return fmt.Errorf("eval: case %s: findings[%d] needs an id", c.Name, i)
		}
		if len(f.Lines) != 0 && len(f.Lines) != 2 {
			return fmt.Errorf("eval: case %s: finding %q lines must be [start, end]", c.Name, f.ID)
		}
	}
	for i, f := range c.Expect.Forbid {
		if f.ID == "" {
			return fmt.Errorf("eval: case %s: forbid[%d] needs an id", c.Name, i)
		}
		if f.MinSeverity != "" {
			if _, err := review.ParseSeverity(f.MinSeverity); err != nil {
				return fmt.Errorf("eval: case %s: forbid %q: %v", c.Name, f.ID, err)
			}
		}
	}
	return nil
}

// LoadCases reads every case directory under root, sorted by name.
func LoadCases(root string) ([]Case, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, fmt.Errorf("eval: reading cases directory: %w", err)
	}
	var cases []Case
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		c, err := LoadCase(filepath.Join(root, e.Name()))
		if err != nil {
			return nil, err
		}
		cases = append(cases, *c)
	}
	if len(cases) == 0 {
		return nil, fmt.Errorf("eval: no cases found under %s", root)
	}
	sort.Slice(cases, func(i, j int) bool { return cases[i].Name < cases[j].Name })
	return cases, nil
}

// LoadCase reads a single case directory.
func LoadCase(dir string) (*Case, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(filepath.Join(abs, "case.yaml")) //nolint:gosec // corpus path chosen by the caller
	if err != nil {
		return nil, fmt.Errorf("eval: case %s: %w", filepath.Base(abs), err)
	}
	c := Case{Name: filepath.Base(abs), Dir: abs}
	if err := yaml.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("eval: case %s: parsing case.yaml: %w", c.Name, err)
	}
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

// BuildRequest assembles the review request for a case. useWorkspace
// attaches the case's workspace/ directory (when present) to enable
// agentic exploration.
func (c *Case) BuildRequest(useWorkspace bool) (*review.Request, error) {
	req := &review.Request{
		Prompt:   c.Prompt,
		Metadata: map[string]string{"eval_case": c.Name},
	}

	diffPath := filepath.Join(c.Dir, "diff.patch")
	if data, err := os.ReadFile(diffPath); err == nil { //nolint:gosec // path is under the case dir
		req.Diff = string(data)
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("eval: case %s: %w", c.Name, err)
	}

	filesDir := filepath.Join(c.Dir, "files")
	if info, err := os.Stat(filesDir); err == nil && info.IsDir() {
		err := filepath.WalkDir(filesDir, func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			content, err := os.ReadFile(p) //nolint:gosec // path is under the case dir
			if err != nil {
				return err
			}
			rel, err := filepath.Rel(filesDir, p)
			if err != nil {
				return err
			}
			req.Files = append(req.Files, review.File{
				Path:    filepath.ToSlash(rel),
				Content: string(content),
			})
			return nil
		})
		if err != nil {
			return nil, fmt.Errorf("eval: case %s: reading files: %w", c.Name, err)
		}
		sort.Slice(req.Files, func(i, j int) bool { return req.Files[i].Path < req.Files[j].Path })
	}

	if useWorkspace {
		wsDir := filepath.Join(c.Dir, "workspace")
		if info, err := os.Stat(wsDir); err == nil && info.IsDir() {
			req.WorkspaceRoot = wsDir
		}
	}

	if strings.TrimSpace(req.Diff) == "" && len(req.Files) == 0 {
		return nil, fmt.Errorf("eval: case %s: neither diff.patch nor files/ present", c.Name)
	}
	return req, nil
}

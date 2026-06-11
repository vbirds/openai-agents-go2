package main

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"

	"github.com/MitulShah1/openai-agents-go/review"
)

// maxContextFiles bounds how many changed files are auto-loaded from a git
// range, so a sweeping refactor does not blow the input budget.
const maxContextFiles = 50

// buildRequest collects the diff, context files, prompt, and schema into a
// review request.
func buildRequest(opts *cliOptions, filePaths []string) (*review.Request, error) {
	req := &review.Request{}

	switch {
	case opts.gitRange != "":
		diff, changed, err := gitDiff(opts.gitRange)
		if err != nil {
			return nil, err
		}
		req.Diff = diff
		// Load post-change contents of the changed files for context;
		// explicitly listed files are added below and take precedence.
		for _, p := range changed {
			if len(req.Files) >= maxContextFiles {
				fmt.Fprintf(os.Stderr, "warning: more than %d changed files; remaining files passed via diff only\n", maxContextFiles)
				break
			}
			if containsPath(filePaths, p) {
				continue
			}
			content, err := os.ReadFile(p) //nolint:gosec // paths come from git diff in the user's own repo
			if err != nil {
				// Deleted or renamed-away files have no post-change content.
				continue
			}
			req.Files = append(req.Files, review.File{Path: p, Content: string(content)})
		}
	case opts.diffPath == "-":
		data, err := io.ReadAll(os.Stdin)
		if err != nil {
			return nil, fmt.Errorf("reading diff from stdin: %w", err)
		}
		req.Diff = string(data)
	case opts.diffPath != "":
		data, err := os.ReadFile(opts.diffPath)
		if err != nil {
			return nil, fmt.Errorf("reading diff: %w", err)
		}
		req.Diff = string(data)
	}

	for _, p := range filePaths {
		content, err := os.ReadFile(p) //nolint:gosec // paths are the user's own CLI arguments
		if err != nil {
			return nil, fmt.Errorf("reading file: %w", err)
		}
		req.Files = append(req.Files, review.File{Path: p, Content: string(content)})
	}

	switch {
	case opts.promptFile != "":
		data, err := os.ReadFile(opts.promptFile)
		if err != nil {
			return nil, fmt.Errorf("reading prompt file: %w", err)
		}
		req.Prompt = string(data)
	case opts.prompt != "":
		req.Prompt = opts.prompt
	}

	if opts.schemaPath != "" {
		data, err := os.ReadFile(opts.schemaPath)
		if err != nil {
			return nil, fmt.Errorf("reading schema: %w", err)
		}
		req.Schema = data
	}

	if strings.TrimSpace(req.Diff) == "" && len(req.Files) == 0 {
		return nil, fmt.Errorf("nothing to review: provide -diff, -git, or file arguments (see -h)")
	}
	return req, nil
}

// gitDiff returns the diff and the changed file paths for a revision range.
func gitDiff(revRange string) (diff string, changedFiles []string, err error) {
	diffOut, err := runGit("diff", revRange)
	if err != nil {
		return "", nil, err
	}
	if strings.TrimSpace(diffOut) == "" {
		return "", nil, fmt.Errorf("git diff %s produced no changes", revRange)
	}
	namesOut, err := runGit("diff", "--name-only", revRange)
	if err != nil {
		return "", nil, err
	}
	for _, line := range strings.Split(namesOut, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			changedFiles = append(changedFiles, line)
		}
	}
	return diffOut, changedFiles, nil
}

func runGit(args ...string) (string, error) {
	var stdout, stderr bytes.Buffer
	cmd := exec.Command("git", args...)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}

func containsPath(paths []string, p string) bool {
	for _, candidate := range paths {
		if candidate == p {
			return true
		}
	}
	return false
}

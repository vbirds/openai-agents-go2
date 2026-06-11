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
		req.Files = loadChangedFiles(changed, filePaths)
	case opts.svnTarget != "":
		diff, changed, err := svnDiff(opts.svnTarget)
		if err != nil {
			return nil, err
		}
		req.Diff = diff
		req.Files = loadChangedFiles(changed, filePaths)
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
		return nil, fmt.Errorf("nothing to review: provide -diff, -git, -svn, or file arguments (see -h)")
	}
	return req, nil
}

// loadChangedFiles reads the post-change (working copy) contents of files
// reported as changed by the VCS, skipping files the user listed explicitly
// (those are loaded separately) and files that no longer exist (deletions).
func loadChangedFiles(changed, explicit []string) []review.File {
	var files []review.File
	for _, p := range changed {
		if len(files) >= maxContextFiles {
			fmt.Fprintf(os.Stderr, "warning: more than %d changed files; remaining files passed via diff only\n", maxContextFiles)
			break
		}
		if containsPath(explicit, p) {
			continue
		}
		content, err := os.ReadFile(p) //nolint:gosec // paths come from the VCS in the user's own repo
		if err != nil {
			// Deleted or renamed-away files have no post-change content.
			continue
		}
		files = append(files, review.File{Path: p, Content: string(content)})
	}
	return files
}

// gitDiff returns the diff and the changed file paths for a revision range.
func gitDiff(revRange string) (diff string, changedFiles []string, err error) {
	diffOut, err := runVCS("git", "diff", revRange)
	if err != nil {
		return "", nil, err
	}
	if strings.TrimSpace(diffOut) == "" {
		return "", nil, fmt.Errorf("git diff %s produced no changes", revRange)
	}
	namesOut, err := runVCS("git", "diff", "--name-only", revRange)
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

// svnDiff returns the diff and changed file paths for a Subversion target:
// "wc" for uncommitted working-copy changes, "N:M" (revision numbers or
// keywords like BASE:HEAD) for a revision range, or a single revision number
// for the change committed in that revision.
func svnDiff(target string) (diff string, changedFiles []string, err error) {
	revArgs, err := svnRevisionArgs(target)
	if err != nil {
		return "", nil, err
	}

	diffOut, err := runVCS("svn", append([]string{"diff"}, revArgs...)...)
	if err != nil {
		return "", nil, err
	}
	if strings.TrimSpace(diffOut) == "" {
		return "", nil, fmt.Errorf("svn diff %s produced no changes", strings.Join(revArgs, " "))
	}

	summarizeOut, err := runVCS("svn", append([]string{"diff", "--summarize"}, revArgs...)...)
	if err != nil {
		return "", nil, err
	}
	return diffOut, parseSvnSummarize(summarizeOut), nil
}

// svnRevisionArgs translates the -svn flag value into svn diff arguments.
func svnRevisionArgs(target string) ([]string, error) {
	switch {
	case target == "wc":
		// Uncommitted working-copy changes: plain `svn diff`.
		return nil, nil
	case strings.Contains(target, ":"):
		// Revision range, e.g. 100:105 or BASE:HEAD.
		return []string{"-r", target}, nil
	case isAllDigits(target):
		// A single committed revision, e.g. 123 -> `svn diff -c 123`.
		return []string{"-c", target}, nil
	default:
		return nil, fmt.Errorf("invalid -svn value %q: use 'wc' for working-copy changes, 'N:M' for a revision range, or a revision number", target)
	}
}

// parseSvnSummarize extracts changed file paths from `svn diff --summarize`
// output. Lines have a fixed-width 8-column status field followed by the
// path, e.g. "M       path/to/file" or " M      props-only-change". Deleted
// files are skipped since they have no post-change content to load.
func parseSvnSummarize(out string) []string {
	var paths []string
	for _, line := range strings.Split(out, "\n") {
		if len(line) <= 8 {
			continue
		}
		if line[0] == 'D' {
			continue
		}
		if p := strings.TrimSpace(line[8:]); p != "" {
			paths = append(paths, p)
		}
	}
	return paths
}

func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func runVCS(name string, args ...string) (string, error) {
	var stdout, stderr bytes.Buffer
	cmd := exec.Command(name, args...) //nolint:gosec // name is a fixed VCS binary; args derive from user flags
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("%s %s: %v: %s", name, strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
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

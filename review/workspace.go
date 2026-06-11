package review

import (
	"bytes"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/MitulShah1/openai-agents-go/tools"
)

// Limits on exploration tool output, chosen to keep individual tool results
// well under a single turn's context budget while staying useful.
const (
	maxReadLines     = 800             // lines returned by one read_file call
	maxReadBytes     = 64 * 1024       // bytes returned by one read_file call
	maxGrepMatches   = 100             // matches returned by one grep call
	maxGrepFileSize  = 1 * 1024 * 1024 // files larger than this are not searched
	maxListEntries   = 300             // entries returned by one list_dir call
	maxGrepLineBytes = 500             // grep match lines are clipped to this
)

// skipDirs are well-known directories excluded from grep and list_dir; they
// are either VCS metadata or third-party code that drowns out signal.
var skipDirs = map[string]bool{
	".git":         true,
	".svn":         true,
	".hg":          true,
	"node_modules": true,
	".idea":        true,
	".vscode":      true,
}

// workspace provides sandboxed, read-only access to a project directory for
// the exploration tools. All paths are resolved against the root and access
// outside it (including via symlinks) is rejected.
type workspace struct {
	root string // absolute path with symlinks resolved
}

func newWorkspace(root string) (*workspace, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("%w: workspace root: %v", ErrInvalidRequest, err)
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return nil, fmt.Errorf("%w: workspace root %q: %v", ErrInvalidRequest, root, err)
	}
	info, err := os.Stat(resolved)
	if err != nil || !info.IsDir() {
		return nil, fmt.Errorf("%w: workspace root %q is not a directory", ErrInvalidRequest, root)
	}
	return &workspace{root: resolved}, nil
}

// resolve maps a model-supplied path to an absolute path inside the root,
// rejecting escapes via "..", absolute paths outside the root, and symlinks
// pointing outside the root.
func (w *workspace) resolve(p string) (string, error) {
	if strings.TrimSpace(p) == "" || p == "." {
		return w.root, nil
	}
	candidate := p
	if !filepath.IsAbs(candidate) {
		candidate = filepath.Join(w.root, candidate)
	}
	candidate = filepath.Clean(candidate)

	resolved, err := filepath.EvalSymlinks(candidate)
	if err != nil {
		if os.IsNotExist(err) {
			return "", fmt.Errorf("path %q does not exist", p)
		}
		return "", fmt.Errorf("cannot access %q: %v", p, err)
	}
	if resolved != w.root && !strings.HasPrefix(resolved, w.root+string(filepath.Separator)) {
		return "", fmt.Errorf("path %q is outside the workspace", p)
	}
	return resolved, nil
}

// rel converts an absolute path back to workspace-relative form for display.
func (w *workspace) rel(abs string) string {
	if r, err := filepath.Rel(w.root, abs); err == nil {
		return filepath.ToSlash(r)
	}
	return abs
}

// readFile returns the (line-numbered) content of a file, optionally
// restricted to a 1-based inclusive line range.
func (w *workspace) readFile(path string, startLine, endLine int) (string, error) {
	abs, err := w.resolve(path)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(abs)
	if err != nil {
		return "", fmt.Errorf("cannot stat %q: %v", path, err)
	}
	if info.IsDir() {
		return "", fmt.Errorf("%q is a directory; use list_dir instead", path)
	}
	data, err := os.ReadFile(abs) //nolint:gosec // path is sandbox-checked by resolve
	if err != nil {
		return "", fmt.Errorf("cannot read %q: %v", path, err)
	}
	if isBinary(data) {
		return "", fmt.Errorf("%q appears to be a binary file", path)
	}

	lines := strings.Split(string(data), "\n")
	if n := len(lines); n > 1 && lines[n-1] == "" {
		lines = lines[:n-1]
	}
	total := len(lines)

	if startLine <= 0 {
		startLine = 1
	}
	if endLine <= 0 || endLine > total {
		endLine = total
	}
	if startLine > total {
		return "", fmt.Errorf("start_line %d is beyond the end of %q (%d lines)", startLine, path, total)
	}
	if endLine < startLine {
		return "", fmt.Errorf("end_line %d is before start_line %d", endLine, startLine)
	}

	clipped := false
	if endLine-startLine+1 > maxReadLines {
		endLine = startLine + maxReadLines - 1
		clipped = true
	}

	width := len(fmt.Sprint(endLine))
	var b strings.Builder
	for i := startLine; i <= endLine; i++ {
		line := lines[i-1]
		fmt.Fprintf(&b, "%*d| %s\n", width, i, line)
		if b.Len() > maxReadBytes {
			endLine = i
			clipped = true
			break
		}
	}
	header := fmt.Sprintf("%s (lines %d-%d of %d)\n", w.rel(abs), startLine, endLine, total)
	out := header + b.String()
	if clipped {
		out += fmt.Sprintf("[output clipped at line %d; call read_file again with start_line=%d to continue]\n", endLine, endLine+1)
	}
	return out, nil
}

// grep searches file contents under a path with a Go (RE2) regular
// expression and returns matches as "path:line: text" lines.
func (w *workspace) grep(pattern, path string) (string, error) {
	re, err := regexp.Compile(pattern)
	if err != nil {
		return "", fmt.Errorf("invalid regular expression: %v", err)
	}
	abs, err := w.resolve(path)
	if err != nil {
		return "", err
	}

	var b strings.Builder
	matches, filesScanned := 0, 0
	clipped := false

	walkErr := filepath.WalkDir(abs, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil //nolint:nilerr // unreadable entries are skipped, not fatal
		}
		if d.IsDir() {
			if skipDirs[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		info, err := d.Info()
		if err != nil || info.Size() > maxGrepFileSize {
			return nil
		}
		data, err := os.ReadFile(p) //nolint:gosec // p is under the sandbox-checked root
		if err != nil || isBinary(data) {
			return nil
		}
		filesScanned++
		for i, line := range strings.Split(string(data), "\n") {
			if !re.MatchString(line) {
				continue
			}
			if matches >= maxGrepMatches {
				clipped = true
				return filepath.SkipAll
			}
			text := strings.TrimRight(line, "\r")
			if len(text) > maxGrepLineBytes {
				text = text[:maxGrepLineBytes] + "..."
			}
			fmt.Fprintf(&b, "%s:%d: %s\n", w.rel(p), i+1, text)
			matches++
		}
		return nil
	})
	if walkErr != nil {
		return "", fmt.Errorf("search failed: %v", walkErr)
	}

	if matches == 0 {
		return fmt.Sprintf("no matches for %q (searched %d files)", pattern, filesScanned), nil
	}
	summary := fmt.Sprintf("%d matches:\n", matches)
	if clipped {
		summary = fmt.Sprintf("first %d matches (more exist; narrow the pattern or path):\n", maxGrepMatches)
	}
	return summary + b.String(), nil
}

// listDir lists a directory's entries, directories first.
func (w *workspace) listDir(path string) (string, error) {
	abs, err := w.resolve(path)
	if err != nil {
		return "", err
	}
	entries, err := os.ReadDir(abs)
	if err != nil {
		return "", fmt.Errorf("cannot list %q: %v", path, err)
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].IsDir() != entries[j].IsDir() {
			return entries[i].IsDir()
		}
		return entries[i].Name() < entries[j].Name()
	})

	var b strings.Builder
	fmt.Fprintf(&b, "%s/ (%d entries)\n", w.rel(abs), len(entries))
	for i, e := range entries {
		if i >= maxListEntries {
			fmt.Fprintf(&b, "[%d more entries not shown]\n", len(entries)-maxListEntries)
			break
		}
		if e.IsDir() {
			fmt.Fprintf(&b, "  %s/\n", e.Name())
			continue
		}
		size := int64(0)
		if info, err := e.Info(); err == nil {
			size = info.Size()
		}
		fmt.Fprintf(&b, "  %s (%d bytes)\n", e.Name(), size)
	}
	return b.String(), nil
}

func isBinary(data []byte) bool {
	probe := data
	if len(probe) > 8192 {
		probe = probe[:8192]
	}
	return bytes.IndexByte(probe, 0) >= 0
}

// explorationTools exposes the workspace to the agent as read-only tools.
func explorationTools(w *workspace) []tools.Tool {
	readFile := tools.New(
		"read_file",
		"Read a file from the repository with line numbers. Use start_line/end_line to read a specific region; large reads are clipped.",
		map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path": map[string]any{
					"type":        "string",
					"description": "File path relative to the repository root, exactly as it appears in the diff or grep results.",
				},
				"start_line": map[string]any{
					"type":        "integer",
					"description": "First line to read (1-based). Omit to read from the beginning.",
				},
				"end_line": map[string]any{
					"type":        "integer",
					"description": "Last line to read (inclusive). Omit to read to the end.",
				},
			},
			"required": []any{"path"},
		},
		func(args map[string]any, _ tools.ContextVariables) (any, error) {
			path, _ := args["path"].(string)
			return w.readFile(path, intArg(args, "start_line"), intArg(args, "end_line"))
		},
	)

	grep := tools.New(
		"grep",
		"Search file contents in the repository with a regular expression (RE2 syntax). Returns 'path:line: text' matches. Use it to find callers, definitions, and usages of changed symbols.",
		map[string]any{
			"type": "object",
			"properties": map[string]any{
				"pattern": map[string]any{
					"type":        "string",
					"description": "Regular expression to search for, e.g. '\\bUserByName\\(' to find call sites.",
				},
				"path": map[string]any{
					"type":        "string",
					"description": "Directory or file to search, relative to the repository root. Omit to search the whole repository.",
				},
			},
			"required": []any{"pattern"},
		},
		func(args map[string]any, _ tools.ContextVariables) (any, error) {
			pattern, _ := args["pattern"].(string)
			path, _ := args["path"].(string)
			return w.grep(pattern, path)
		},
	)

	listDir := tools.New(
		"list_dir",
		"List the entries of a repository directory (directories first).",
		map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path": map[string]any{
					"type":        "string",
					"description": "Directory path relative to the repository root. Omit for the root.",
				},
			},
			"required": []any{},
		},
		func(args map[string]any, _ tools.ContextVariables) (any, error) {
			path, _ := args["path"].(string)
			return w.listDir(path)
		},
	)

	return []tools.Tool{readFile, grep, listDir}
}

// intArg extracts an integer tool argument; JSON numbers arrive as float64.
func intArg(args map[string]any, key string) int {
	switch v := args[key].(type) {
	case float64:
		return int(v)
	case int:
		return v
	default:
		return 0
	}
}

package review

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// newTestWorkspace builds a small project tree:
//
//	root/
//	  main.go        (3 lines)
//	  sub/util.go    (contains "TargetFunc")
//	  sub/data.bin   (binary)
//	  .git/config    (must be invisible to grep/list)
//	  secret-link -> /etc (symlink escape, non-Windows)
func newTestWorkspace(t *testing.T) (*workspace, string) {
	t.Helper()
	root := t.TempDir()

	mustWrite(t, filepath.Join(root, "main.go"), "package main\n\nfunc main() { TargetFunc() }\n")
	mustWrite(t, filepath.Join(root, "sub", "util.go"), "package sub\n\nfunc TargetFunc() {}\n")
	mustWrite(t, filepath.Join(root, "sub", "data.bin"), "bin\x00ary")
	mustWrite(t, filepath.Join(root, ".git", "config"), "[core]\nTargetFunc\n")

	if runtime.GOOS != "windows" {
		if err := os.Symlink("/etc", filepath.Join(root, "secret-link")); err != nil {
			t.Fatal(err)
		}
	}

	ws, err := newWorkspace(root)
	if err != nil {
		t.Fatalf("newWorkspace: %v", err)
	}
	return ws, root
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestWorkspaceSandbox(t *testing.T) {
	ws, root := newTestWorkspace(t)

	escapes := []string{
		"../outside.txt",
		"sub/../../outside.txt",
		"/etc/passwd",
	}
	if runtime.GOOS != "windows" {
		escapes = append(escapes, "secret-link/passwd")
	}
	for _, p := range escapes {
		if _, err := ws.resolve(p); err == nil {
			t.Errorf("resolve(%q) must be rejected", p)
		}
	}

	for _, p := range []string{"main.go", "sub/util.go", ".", ""} {
		if _, err := ws.resolve(p); err != nil {
			t.Errorf("resolve(%q): %v", p, err)
		}
	}

	// Absolute path inside the root is fine.
	if _, err := ws.resolve(filepath.Join(root, "main.go")); err != nil {
		t.Errorf("absolute in-root path rejected: %v", err)
	}
}

func TestWorkspaceReadFile(t *testing.T) {
	ws, _ := newTestWorkspace(t)

	out, err := ws.readFile("main.go", 0, 0)
	if err != nil {
		t.Fatalf("readFile: %v", err)
	}
	if !strings.Contains(out, "1| package main") || !strings.Contains(out, "3| func main") {
		t.Errorf("missing numbered lines:\n%s", out)
	}
	if !strings.Contains(out, "lines 1-3 of 3") {
		t.Errorf("missing range header:\n%s", out)
	}

	out, err = ws.readFile("main.go", 3, 3)
	if err != nil {
		t.Fatalf("readFile range: %v", err)
	}
	if strings.Contains(out, "package main") || !strings.Contains(out, "func main") {
		t.Errorf("range not respected:\n%s", out)
	}

	if _, err := ws.readFile("sub/data.bin", 0, 0); err == nil {
		t.Error("binary file must be rejected")
	}
	if _, err := ws.readFile("sub", 0, 0); err == nil {
		t.Error("directory must be rejected")
	}
	if _, err := ws.readFile("main.go", 99, 0); err == nil {
		t.Error("start_line beyond EOF must error")
	}
	if _, err := ws.readFile("nope.go", 0, 0); err == nil {
		t.Error("missing file must error")
	}
}

func TestWorkspaceGrep(t *testing.T) {
	ws, _ := newTestWorkspace(t)

	out, err := ws.grep(`TargetFunc\(`, "")
	if err != nil {
		t.Fatalf("grep: %v", err)
	}
	if !strings.Contains(out, "main.go:3:") || !strings.Contains(out, "sub/util.go:3:") {
		t.Errorf("expected matches in both files:\n%s", out)
	}
	if strings.Contains(out, ".git") {
		t.Errorf(".git must be excluded from search:\n%s", out)
	}

	out, err = ws.grep("TargetFunc", "sub")
	if err != nil {
		t.Fatalf("grep scoped: %v", err)
	}
	if strings.Contains(out, "main.go") {
		t.Errorf("scoped grep leaked outside sub/:\n%s", out)
	}

	out, err = ws.grep("NoSuchSymbol", "")
	if err != nil || !strings.Contains(out, "no matches") {
		t.Errorf("expected no-matches message, got %q, %v", out, err)
	}

	if _, err := ws.grep("(unclosed", ""); err == nil {
		t.Error("invalid regex must error")
	}
	if _, err := ws.grep("x", "../"); err == nil {
		t.Error("grep outside workspace must error")
	}
}

func TestWorkspaceListDir(t *testing.T) {
	ws, _ := newTestWorkspace(t)

	out, err := ws.listDir("")
	if err != nil {
		t.Fatalf("listDir: %v", err)
	}
	if !strings.Contains(out, "sub/") || !strings.Contains(out, "main.go") {
		t.Errorf("missing entries:\n%s", out)
	}

	if _, err := ws.listDir("main.go"); err == nil {
		t.Error("listing a file must error")
	}
	if _, err := ws.listDir("../"); err == nil {
		t.Error("listing outside workspace must error")
	}
}

func TestExplorationToolsExecute(t *testing.T) {
	ws, _ := newTestWorkspace(t)
	toolset := explorationTools(ws)
	if len(toolset) != 3 {
		t.Fatalf("expected 3 tools, got %d", len(toolset))
	}

	byName := map[string]int{}
	for i, tl := range toolset {
		byName[tl.Name] = i
	}

	out, err := toolset[byName["read_file"]].Execute(`{"path":"main.go","start_line":1,"end_line":1}`, nil)
	if err != nil || !strings.Contains(out.(string), "package main") {
		t.Errorf("read_file via Execute: %v, %v", out, err)
	}
	out, err = toolset[byName["grep"]].Execute(`{"pattern":"TargetFunc"}`, nil)
	if err != nil || !strings.Contains(out.(string), "util.go") {
		t.Errorf("grep via Execute: %v, %v", out, err)
	}
	out, err = toolset[byName["list_dir"]].Execute(`{}`, nil)
	if err != nil || !strings.Contains(out.(string), "sub/") {
		t.Errorf("list_dir via Execute: %v, %v", out, err)
	}
}

func TestNewWorkspaceRejectsBadRoots(t *testing.T) {
	if _, err := newWorkspace(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Error("missing root must error")
	}
	f := filepath.Join(t.TempDir(), "file.txt")
	mustWrite(t, f, "x")
	if _, err := newWorkspace(f); err == nil {
		t.Error("file root must error")
	}
}

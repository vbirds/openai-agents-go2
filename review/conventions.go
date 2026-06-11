package review

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// conventionFileNames are the well-known project instruction files probed
// by DiscoverConventions, in priority order. The first ~24KB of matched
// content is used.
var conventionFileNames = []string{
	"AGENTS.md",
	"AGENT.md",
	"CLAUDE.md",
	".codereview.md",
	"CONTRIBUTING.md",
}

const (
	// maxConventionFileBytes caps how much of a single instruction file is
	// read.
	maxConventionFileBytes = 16 * 1024
	// maxConventionTotalBytes caps the combined conventions content.
	maxConventionTotalBytes = 24 * 1024
)

// DiscoverConventions reads well-known project instruction files
// (AGENTS.md, CLAUDE.md, ...) from a project root and returns their
// combined content plus the names of the files found. The result is meant
// for Request.Conventions, teaching the reviewer project-specific rules
// the same way agent CLIs read AGENTS.md.
func DiscoverConventions(root string) (content string, found []string) {
	var b strings.Builder
	for _, name := range conventionFileNames {
		if b.Len() >= maxConventionTotalBytes {
			break
		}
		path := filepath.Join(root, name)
		info, err := os.Stat(path)
		if err != nil || info.IsDir() {
			continue
		}
		data, err := os.ReadFile(path) //nolint:gosec // fixed names under the caller's root
		if err != nil {
			continue
		}
		text := strings.TrimSpace(string(data))
		if text == "" {
			continue
		}
		if len(text) > maxConventionFileBytes {
			text = text[:maxConventionFileBytes] + "\n... [truncated]"
		}
		if budget := maxConventionTotalBytes - b.Len(); len(text) > budget {
			text = text[:budget] + "\n... [truncated]"
		}
		fmt.Fprintf(&b, "### %s\n\n%s\n\n", name, text)
		found = append(found, name)
	}
	return strings.TrimSpace(b.String()), found
}

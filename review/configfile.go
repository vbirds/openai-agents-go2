package review

import (
	"bytes"
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// ConfigFileName is the repository-level configuration file discovered by
// the CLI at the workspace root.
const ConfigFileName = ".codereview.yaml"

// FileConfig is the on-disk configuration format (.codereview.yaml). It
// lets a repository define and tune the specialist roster without code:
//
//	specialists:
//	  - name: api-compat
//	    description: Detects breaking changes to exported APIs.
//	    triggers:
//	      paths: ["api/**", "**/*.proto"]
//	    instructions: |
//	      You check exclusively for breaking API changes: ...
//	    categories: [bug, other]
//	  - name: security        # tune a built-in: set fields override
//	    model: gpt-4o
//	disable: [performance]
type FileConfig struct {
	// Specialists defines new specialists or overrides fields of existing
	// ones (matched by name; only set fields override).
	Specialists []Specialist `yaml:"specialists"`

	// Disable removes specialists from the roster by name.
	Disable []string `yaml:"disable"`
}

// LoadFileConfig parses a configuration file.
func LoadFileConfig(path string) (*FileConfig, error) {
	data, err := os.ReadFile(path) //nolint:gosec // config path is chosen by the caller
	if err != nil {
		return nil, fmt.Errorf("review: reading config file: %w", err)
	}
	var fc FileConfig
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&fc); err != nil {
		return nil, fmt.Errorf("review: parsing config file %s: %w", path, err)
	}
	return &fc, nil
}

// apply merges the file configuration into a specialist roster: entries
// matching an existing name override its set fields, new names are
// validated and appended, and disabled names are removed.
func (fc *FileConfig) apply(roster []Specialist) ([]Specialist, error) {
	byName := make(map[string]int, len(roster))
	for i, s := range roster {
		byName[s.Name] = i
	}

	for _, override := range fc.Specialists {
		idx, exists := byName[override.Name]
		if !exists {
			s := override
			if err := s.Validate(); err != nil {
				return nil, err
			}
			byName[s.Name] = len(roster)
			roster = append(roster, s)
			continue
		}
		base := &roster[idx]
		if override.Description != "" {
			base.Description = override.Description
		}
		if override.Instructions != "" {
			base.Instructions = override.Instructions
		}
		if len(override.Categories) > 0 {
			base.Categories = override.Categories
		}
		if override.Triggers.rulesDefined() {
			base.Triggers = override.Triggers
		}
		if override.Model != "" {
			base.Model = override.Model
		}
		if override.MaxTurns > 0 {
			base.MaxTurns = override.MaxTurns
		}
	}

	if len(fc.Disable) == 0 {
		return roster, nil
	}
	disabled := make(map[string]bool, len(fc.Disable))
	for _, n := range fc.Disable {
		disabled[n] = true
	}
	kept := roster[:0]
	for _, s := range roster {
		if !disabled[s.Name] {
			kept = append(kept, s)
		}
	}
	return kept, nil
}

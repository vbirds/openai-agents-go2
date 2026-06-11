package review

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), ConfigFileName)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadFileConfigAndApply(t *testing.T) {
	path := writeConfig(t, `
specialists:
  - name: api-compat
    description: Detects breaking changes to exported APIs.
    instructions: |
      You check exclusively for breaking API changes.
    categories: [bug, other]
    triggers:
      paths: ["api/**"]
  - name: security
    model: gpt-4o-mini
    max_turns: 6
disable: [performance]
`)

	fc, err := LoadFileConfig(path)
	if err != nil {
		t.Fatalf("LoadFileConfig: %v", err)
	}
	roster, err := fc.apply(BuiltinSpecialists())
	if err != nil {
		t.Fatalf("apply: %v", err)
	}

	byName := map[string]Specialist{}
	for _, s := range roster {
		byName[s.Name] = s
	}

	if _, ok := byName["performance"]; ok {
		t.Error("disabled specialist still present")
	}
	apiCompat, ok := byName["api-compat"]
	if !ok {
		t.Fatal("new specialist not added")
	}
	if len(apiCompat.Triggers.Paths) != 1 || apiCompat.Triggers.Paths[0] != "api/**" {
		t.Errorf("api-compat triggers = %+v", apiCompat.Triggers)
	}

	security := byName["security"]
	if security.Model != "gpt-4o-mini" || security.MaxTurns != 6 {
		t.Errorf("security override not applied: model=%q max_turns=%d", security.Model, security.MaxTurns)
	}
	// Untouched fields of the built-in must survive the override.
	if security.Instructions == "" || !security.Triggers.rulesDefined() {
		t.Error("override clobbered unset fields of the built-in specialist")
	}
}

func TestLoadFileConfigRejectsInvalid(t *testing.T) {
	// Unknown top-level keys are typos; fail loudly.
	if _, err := LoadFileConfig(writeConfig(t, "specialistz: []\n")); err == nil {
		t.Error("unknown field must be rejected")
	}
	// New specialists must be complete.
	fc, err := LoadFileConfig(writeConfig(t, "specialists:\n  - name: incomplete\n"))
	if err != nil {
		t.Fatalf("LoadFileConfig: %v", err)
	}
	if _, err := fc.apply(BuiltinSpecialists()); err == nil {
		t.Error("incomplete new specialist must be rejected at apply time")
	}
	if _, err := LoadFileConfig(filepath.Join(t.TempDir(), "missing.yaml")); err == nil {
		t.Error("missing file must error")
	}
}

func TestWithConfigFileOption(t *testing.T) {
	path := writeConfig(t, `
disable: [performance, concurrency]
`)
	cfg := config{specialists: BuiltinSpecialists()}
	if err := WithConfigFile(path)(&cfg); err != nil {
		t.Fatalf("WithConfigFile: %v", err)
	}
	if len(cfg.specialists) != 2 {
		t.Errorf("roster = %d specialists, want 2", len(cfg.specialists))
	}
	for _, s := range cfg.specialists {
		if s.Name == "performance" || s.Name == "concurrency" {
			t.Errorf("disabled specialist %q still in roster", s.Name)
		}
	}
}

func TestSpecialistRosterOptions(t *testing.T) {
	custom := Specialist{Name: "custom", Instructions: "x", Triggers: Trigger{Always: true}}

	cfg := config{specialists: BuiltinSpecialists()}
	if err := AddSpecialist(custom)(&cfg); err != nil {
		t.Fatalf("AddSpecialist: %v", err)
	}
	if len(cfg.specialists) != len(BuiltinSpecialists())+1 {
		t.Errorf("roster size = %d", len(cfg.specialists))
	}

	// AddSpecialist with an existing name replaces it.
	replacement := Specialist{Name: "custom", Instructions: "y", Triggers: Trigger{Always: true}}
	if err := AddSpecialist(replacement)(&cfg); err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, s := range cfg.specialists {
		if s.Name == "custom" {
			count++
			if s.Instructions != "y" {
				t.Error("replacement not applied")
			}
		}
	}
	if count != 1 {
		t.Errorf("duplicate specialists after replace: %d", count)
	}

	if err := DisableSpecialists("custom", "security")(&cfg); err != nil {
		t.Fatal(err)
	}
	for _, s := range cfg.specialists {
		if s.Name == "custom" || s.Name == "security" {
			t.Errorf("disabled specialist %q present", s.Name)
		}
	}

	if err := WithSpecialists(Specialist{Name: "bad"})(&cfg); err == nil ||
		!strings.Contains(err.Error(), "instructions") {
		t.Errorf("WithSpecialists must validate, got %v", err)
	}
}

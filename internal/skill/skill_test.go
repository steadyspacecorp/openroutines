package skill

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestListAgentIncludesPluginsAndDropsDuplicateNames(t *testing.T) {
	root := t.TempDir()
	write := func(rel, name string) {
		t.Helper()
		path := filepath.Join(root, rel, "SKILL.md")
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		raw := "---\nname: " + name + "\ndescription: test\n---\nwork\n"
		if err := os.WriteFile(path, []byte(raw), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("skills/owned", "owned")
	write(".openroutines/plugins/demo/skills/plugin-owned", "plugin-owned")
	skills, errs := ListAgent(root)
	if len(errs) != 0 || len(skills) != 2 {
		t.Fatalf("grouped discovery: skills=%v errs=%v", skills, errs)
	}
	write(".openroutines/plugins/demo/skills/owned", "owned")
	skills, errs = ListAgent(root)
	if len(errs) != 1 || !strings.Contains(errs[0].Error(), "duplicate skill") {
		t.Fatalf("duplicate should be reported: skills=%v errs=%v", skills, errs)
	}
	for _, s := range skills {
		if s.Name == "owned" {
			t.Fatal("ambiguous skill must fail closed")
		}
	}
}

package runner

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/steadyspacecorp/openroutines/internal/config"
	"github.com/steadyspacecorp/openroutines/internal/routine"
)

func TestStageUsesSuppliedKnowledgeSnapshotWithoutMaterializingLocalBranch(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "openroutines.yml"), []byte("name: Test\ninstructions: Test agent\nowner:\n  email: test@example.com\ntimezone: UTC\ndefaults:\n  model: test/model\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	snapshot := t.TempDir()
	if err := os.WriteFile(filepath.Join(snapshot, "events.md"), []byte("remote fact\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	r := &routine.Routine{Name: "knowledge-summary", Frontmatter: routine.Frontmatter{Teamwork: routine.TeamworkOff}, Body: "summarize"}
	agent, err := config.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := Stage(dir, agent, r, Attempt{RunID: "run_test", Number: 1, SnapshotDir: snapshot, ReadOnly: true}, &sync.Mutex{})
	if err != nil {
		t.Fatal(err)
	}
	defer prepared.workspace.Cleanup()
	raw, err := os.ReadFile(filepath.Join(prepared.workspace.KnowledgeDir, "events.md"))
	if err != nil || string(raw) != "remote fact\n" {
		t.Fatalf("staged snapshot = %q, %v", raw, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "knowledge")); !os.IsNotExist(err) {
		t.Fatalf("read-only staging materialized local knowledge: %v", err)
	}
}

func TestAttemptEnvIncludesEffectiveRoutineURL(t *testing.T) {
	agent := &config.Agent{Timezone: "America/New_York"}
	attempt := Attempt{RunID: "run_t", Number: 1}
	r := &routine.Routine{Frontmatter: routine.Frontmatter{}}
	if got := strings.Join(attemptEnv(agent, r, attempt, &runSecrets{}), "\n"); !strings.Contains(got, "OPENROUTINES_URL=https://openroutines.dev") {
		t.Fatalf("default framework env missing URL:\n%s", got)
	}
	r.Frontmatter.URL = "https://example.com/agent"
	if got := strings.Join(attemptEnv(agent, r, attempt, &runSecrets{}), "\n"); !strings.Contains(got, "OPENROUTINES_URL=https://example.com/agent") {
		t.Fatalf("declared framework env missing URL:\n%s", got)
	}
}

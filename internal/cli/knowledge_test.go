package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func knowledgeAgent(t *testing.T) string {
	t.Helper()
	base := t.TempDir()
	origin := filepath.Join(base, "origin.git")
	runGit := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@t", "-c", "protocol.file.allow=always"}, args...)...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	runGit(base, "init", "-q", "--bare", origin)
	dir := filepath.Join(base, "agent")
	runGit(base, "init", "-q", "-b", "main", dir)
	if err := os.WriteFile(filepath.Join(dir, "openroutines.yml"), []byte(statusAgentYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(dir, "add", ".")
	runGit(dir, "commit", "-qm", "main")
	runGit(dir, "remote", "add", "origin", origin)
	runGit(dir, "push", "-q", "-u", "origin", "main")
	runGit(dir, "checkout", "-q", "--orphan", "knowledge")
	runGit(dir, "rm", "-q", "-rf", ".")
	if err := os.WriteFile(filepath.Join(dir, "events.md"), []byte("remote fact\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "ledgers"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "ledgers", "daily.md"), []byte("state\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(dir, "add", ".")
	runGit(dir, "commit", "-qm", "knowledge")
	runGit(dir, "push", "-q", "origin", "knowledge")
	runGit(dir, "checkout", "-q", "main")
	return dir
}

func TestKnowledgeStatsAndListReadOriginWithoutMaterializing(t *testing.T) {
	dir := knowledgeAgent(t)
	stats := capture(t, dir, func() {
		if code := cmdKnowledge([]string{"stats"}); code != 0 {
			t.Fatalf("stats exit = %d", code)
		}
	})
	if !strings.Contains(stats, "current tree") || !strings.Contains(stats, "largest file") {
		t.Fatalf("stats output:\n%s", stats)
	}
	list := capture(t, dir, func() {
		if code := cmdKnowledge([]string{"list"}); code != 0 {
			t.Fatalf("list exit = %d", code)
		}
	})
	if !strings.Contains(list, "events.md") || !strings.Contains(list, "ledgers/daily.md") {
		t.Fatalf("list output:\n%s", list)
	}
	if _, err := os.Stat(filepath.Join(dir, "knowledge")); !os.IsNotExist(err) {
		t.Fatalf("inspection materialized local knowledge: %v", err)
	}
}

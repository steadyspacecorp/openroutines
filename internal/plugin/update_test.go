package plugin

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steadyspacecorp/openroutines/internal/routine"
)

func writeTree(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for rel, content := range files {
		path := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// updateFixture creates an upstream plugin repository and installs it into a
// fresh agent, exactly as plugin add does: fetched from a clone, with real
// provenance.
func updateFixture(t *testing.T) (agent, repo string) {
	t.Helper()
	repo = t.TempDir()
	writeTree(t, repo, map[string]string{
		FileName:           "---\nname: demo\ndescription: A demo plugin.\n---\nBody.\n",
		"routines/demo.md": "---\nschedule: \"0 9 * * *\"\n---\nStep one.\nStep two.\nStep three.\n",
	})
	gitCommitAll(t, repo, "v1")
	agent = t.TempDir()
	root, prov, cleanup, err := Fetch(repo, "", "")
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	inst, err := PrepareInstall(agent, root, prov)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := inst.Apply(); err != nil {
		t.Fatal(err)
	}
	return agent, repo
}

func TestPrepareUpdateReportsCurrent(t *testing.T) {
	agent, _ := updateFixture(t)
	upd, err := PrepareUpdate(agent, "demo")
	if err != nil {
		t.Fatal(err)
	}
	defer upd.Close()
	if !upd.Current() {
		t.Fatalf("freshly installed plugin should be current, got %s -> %s", upd.Old.Revision, upd.New.Revision)
	}
}

// A clean update three-way merges: local edits survive, upstream edits land,
// routines new since the recorded revision arrive deactivated, and provenance
// advances to the new revision.
func TestUpdateMergesLocalEditsAndDeactivatesNewRoutines(t *testing.T) {
	agent, repo := updateFixture(t)
	installedRoutine := filepath.Join(agent, ".openroutines", "plugins", "demo", "routines", "demo.md")
	raw, err := os.ReadFile(installedRoutine)
	if err != nil {
		t.Fatal(err)
	}
	local := strings.Replace(string(raw), "Step one.", "Step one, tuned locally.", 1)
	if err := os.WriteFile(installedRoutine, []byte(local), 0o644); err != nil {
		t.Fatal(err)
	}
	writeTree(t, repo, map[string]string{
		"routines/demo.md":  "---\nschedule: \"0 9 * * *\"\n---\nStep one.\nStep two.\nStep three, improved upstream.\n",
		"routines/extra.md": "---\nschedule: \"0 10 * * *\"\n---\nExtra.\n",
	})
	next := gitCommitAll(t, repo, "v2")

	upd, err := PrepareUpdate(agent, "demo")
	if err != nil {
		t.Fatal(err)
	}
	defer upd.Close()
	if upd.Current() {
		t.Fatal("upstream moved; update should not be current")
	}
	for _, want := range []string{"M " + filepath.Join("routines", "demo.md"), "A " + filepath.Join("routines", "extra.md")} {
		if !strings.Contains(strings.Join(upd.Changes, "\n"), want) {
			t.Fatalf("changes missing %q: %v", want, upd.Changes)
		}
	}

	conflicts, err := upd.Apply()
	if err != nil || len(conflicts) != 0 {
		t.Fatalf("clean update: conflicts=%v err=%v", conflicts, err)
	}
	merged, err := os.ReadFile(installedRoutine)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Step one, tuned locally.", "Step three, improved upstream.", "active: false"} {
		if !strings.Contains(string(merged), want) {
			t.Fatalf("merged routine missing %q:\n%s", want, merged)
		}
	}
	extra, err := os.ReadFile(filepath.Join(agent, ".openroutines", "plugins", "demo", "routines", "extra.md"))
	if err != nil || !strings.Contains(string(extra), "active: false") {
		t.Fatalf("new upstream routine must arrive deactivated: %v\n%s", err, extra)
	}
	source, err := ReadSource(filepath.Join(agent, ".openroutines", "plugins", "demo"))
	if err != nil || source.Revision != next {
		t.Fatalf("provenance should advance to %s: %+v err=%v", next, source, err)
	}
	if _, err := os.Stat(filepath.Join(agent, ".openroutines", "plugins", "demo.update-backup")); !os.IsNotExist(err) {
		t.Fatalf("backup should be gone after a clean swap: %v", err)
	}
}

// A conflicted update leaves the markers in place for the person to resolve
// and keeps the recorded revision, so rerunning the update converges.
func TestUpdateConflictKeepsRecordedRevision(t *testing.T) {
	agent, repo := updateFixture(t)
	installedRoutine := filepath.Join(agent, ".openroutines", "plugins", "demo", "routines", "demo.md")
	raw, err := os.ReadFile(installedRoutine)
	if err != nil {
		t.Fatal(err)
	}
	local := strings.Replace(string(raw), "Step one.", "Step one, ours.", 1)
	if err := os.WriteFile(installedRoutine, []byte(local), 0o644); err != nil {
		t.Fatal(err)
	}
	writeTree(t, repo, map[string]string{
		"routines/demo.md": "---\nschedule: \"0 9 * * *\"\n---\nStep one, theirs.\nStep two.\nStep three.\n",
	})
	gitCommitAll(t, repo, "v2")

	upd, err := PrepareUpdate(agent, "demo")
	if err != nil {
		t.Fatal(err)
	}
	defer upd.Close()
	conflicts, err := upd.Apply()
	if err != nil {
		t.Fatal(err)
	}
	if len(conflicts) != 1 || conflicts[0] != filepath.Join("routines", "demo.md") {
		t.Fatalf("want routine conflict, got %v", conflicts)
	}
	merged, _ := os.ReadFile(installedRoutine)
	if !strings.Contains(string(merged), "<<<<<<< local") {
		t.Fatalf("conflict markers missing:\n%s", merged)
	}
	source, err := ReadSource(filepath.Join(agent, ".openroutines", "plugins", "demo"))
	if err != nil || source.Revision != upd.Old.Revision {
		t.Fatalf("conflicted update must keep the recorded revision %s: %+v err=%v", upd.Old.Revision, source, err)
	}
}

// A plugin update may add a routine shadowed by an agent-owned routine; the
// vendored update lands normally and the agent-owned implementation wins.
func TestUpdateAllowsAgentOwnedRoutineOverride(t *testing.T) {
	agent, repo := updateFixture(t)
	writeTree(t, repo, map[string]string{
		"routines/extra.md": "---\nschedule: \"0 10 * * *\"\n---\nExtra.\n",
	})
	gitCommitAll(t, repo, "v2")
	writeTree(t, agent, map[string]string{
		"routines/extra.md": "---\nschedule: \"0 9 * * *\"\n---\nMine.\n",
	})

	upd, err := PrepareUpdate(agent, "demo")
	if err != nil {
		t.Fatalf("prepare update with override: %v", err)
	}
	defer upd.Close()
	conflicts, err := upd.Apply()
	if err != nil || len(conflicts) != 0 {
		t.Fatalf("apply update with override: conflicts=%v err=%v", conflicts, err)
	}
	r, err := routine.Find(agent, "extra")
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(agent, "routines", "extra.md")
	if r.Path != want {
		t.Fatalf("winning routine = %s, want %s", r.Path, want)
	}
}

func TestMergeTreesPreservesLocalEditsAndReportsConflicts(t *testing.T) {
	base := t.TempDir()
	ours := t.TempDir()
	theirs := t.TempDir()
	dest := t.TempDir()
	writeTree(t, base, map[string]string{"routines/demo.md": "schedule: old\nunchanged: one\nunchanged: two\nprompt: old\n"})
	writeTree(t, ours, map[string]string{"routines/demo.md": "schedule: local\nunchanged: one\nunchanged: two\nprompt: old\n"})
	writeTree(t, theirs, map[string]string{
		"routines/demo.md":    "schedule: old\nunchanged: one\nunchanged: two\nprompt: upstream\n",
		"skills/new/SKILL.md": "new\n",
	})

	conflicts, err := mergeTrees(base, ours, theirs, dest)
	if err != nil || len(conflicts) != 0 {
		t.Fatalf("clean merge: conflicts=%v err=%v", conflicts, err)
	}
	raw, _ := os.ReadFile(filepath.Join(dest, "routines", "demo.md"))
	if !strings.Contains(string(raw), "schedule: local") || !strings.Contains(string(raw), "prompt: upstream") {
		t.Fatalf("merge did not preserve both sides:\n%s", raw)
	}
	if _, err := os.Stat(filepath.Join(dest, "skills", "new", "SKILL.md")); err != nil {
		t.Fatalf("upstream addition missing: %v", err)
	}

	conflictDest := t.TempDir()
	writeTree(t, ours, map[string]string{"routines/demo.md": "schedule: ours\nunchanged: one\nunchanged: two\nprompt: old\n"})
	writeTree(t, theirs, map[string]string{"routines/demo.md": "schedule: theirs\nunchanged: one\nunchanged: two\nprompt: old\n"})
	conflicts, err = mergeTrees(base, ours, theirs, conflictDest)
	if err != nil {
		t.Fatal(err)
	}
	if len(conflicts) != 1 || conflicts[0] != filepath.Join("routines", "demo.md") {
		t.Fatalf("want routine conflict, got %v", conflicts)
	}
	raw, _ = os.ReadFile(filepath.Join(conflictDest, "routines", "demo.md"))
	if !strings.Contains(string(raw), "<<<<<<< local") {
		t.Fatalf("conflict markers missing:\n%s", raw)
	}
}

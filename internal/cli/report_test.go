package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steadyspacecorp/openroutines/internal/memory"
)

func memoryAgent(t *testing.T) string {
	t.Helper()
	dir := statusAgent(t, nil)
	memoryGit(t, dir, "init", "-q")
	memoryGit(t, dir, "config", "user.name", "Test")
	memoryGit(t, dir, "config", "user.email", "test@example.invalid")
	memoryGit(t, dir, "add", "openroutines.yml")
	memoryGit(t, dir, "commit", "-qm", "agent")
	if err := memory.At(dir).Ensure(); err != nil {
		t.Fatal(err)
	}
	return dir
}

const checkInFM = "---\nschedule: \"0 6 * * *\"\nteamwork: off\nconsumes: memory\n---\nCheck in.\n"

func writeCheckInRoutine(t *testing.T, dir, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "routines", "check-in.md"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func writeCheckInLedger(t *testing.T, dir string) {
	t.Helper()
	wt := memory.At(dir).Worktree()
	checkIn := "# Check-in 2026-08-06\n\nWhat I did: reviewed 3 PRs.\n"
	if err := os.WriteFile(filepath.Join(wt, "ledgers", "check-in.md"), []byte(checkIn), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestReportShowsLatestCheckIn(t *testing.T) {
	dir := memoryAgent(t)
	writeCheckInRoutine(t, dir, checkInFM)
	writeCheckInLedger(t, dir)
	var code int
	out := capture(t, dir, func() { code = cmdReport(nil) })
	if code != 0 {
		t.Fatalf("report returned %d", code)
	}
	if !strings.Contains(out, "Memory is local only") {
		t.Fatalf("report did not report sync state:\n%s", out)
	}
	if !strings.Contains(out, "reviewed 3 PRs") {
		t.Fatalf("report did not show the stored check-in:\n%s", out)
	}
	if strings.Contains(out, "note:") {
		t.Fatalf("healthy routine drew a health note:\n%s", out)
	}
}

func TestReportWithoutStoredCheckIn(t *testing.T) {
	dir := memoryAgent(t)
	writeCheckInRoutine(t, dir, checkInFM)
	var code int
	out := capture(t, dir, func() { code = cmdReport(nil) })
	if code != 0 {
		t.Fatalf("report returned %d", code)
	}
	if !strings.Contains(out, "No check-in recorded yet") {
		t.Fatalf("missing ledger was not explained:\n%s", out)
	}
}

func TestReportRejectsArguments(t *testing.T) {
	dir := memoryAgent(t)
	var code int
	capture(t, dir, func() { code = cmdReport([]string{"extra"}) })
	if code == 0 {
		t.Fatal("report accepted a positional argument")
	}
}

func TestReportNamesMissingRoutine(t *testing.T) {
	dir := memoryAgent(t)
	var code int
	out := capture(t, dir, func() { code = cmdReport(nil) })
	if code != 0 {
		t.Fatalf("report returned %d", code)
	}
	if !strings.Contains(out, "no check-in routine") {
		t.Fatalf("missing routine was not named:\n%s", out)
	}
	if strings.Contains(out, "routines run") {
		t.Fatalf("suggested running a routine that does not exist:\n%s", out)
	}
}

// A stale report shown without comment reads as current: an inactive
// check-in routine draws a note above the stored report.
func TestReportFlagsInactiveRoutine(t *testing.T) {
	dir := memoryAgent(t)
	writeCheckInRoutine(t, dir, strings.Replace(checkInFM, "teamwork: off\n", "teamwork: off\nactive: false\n", 1))
	writeCheckInLedger(t, dir)
	var code int
	out := capture(t, dir, func() { code = cmdReport(nil) })
	if code != 0 {
		t.Fatalf("report returned %d", code)
	}
	if !strings.Contains(out, "note:") || !strings.Contains(out, "inactive") {
		t.Fatalf("inactive routine was not flagged:\n%s", out)
	}
	if !strings.Contains(out, "reviewed 3 PRs") {
		t.Fatalf("stored report was withheld:\n%s", out)
	}
}

func TestMemoryCommandRetired(t *testing.T) {
	if commands["report"] == nil {
		t.Fatal("report command is not registered")
	}
	for _, retired := range []string{"memory", "teamwork"} {
		if commands[retired] != nil {
			t.Fatalf("the %s command was replaced by report and should be gone", retired)
		}
	}
}

func memoryGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
}

package knowledge

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steadyspacecorp/openroutines/internal/creds"
	"github.com/steadyspacecorp/openroutines/internal/scrub"
)

func TestSupervisorEntriesRedactSecrets(t *testing.T) {
	const masterKey = "aabbccddeeff00112233445566778899aabbccddeeff00112233445566778899"    // gitleaks:allow -- synthetic fixture
	const deployKeyLine = "b3BlbnNzaC1rZXktdjEAAAAABG5vbmUAAAAEbm9uZQAAAAAAAAABAAAAMwAAAAt" // gitleaks:allow -- synthetic fixture
	t.Setenv("OPENROUTINES_MASTER_KEY", masterKey)
	dir := deliveryFixture(t)

	if _, err := creds.LoadKey(dir); err != nil {
		t.Fatal(err)
	}
	scrub.Register(map[string]string{"DEPLOY_KEY": deployKeyLine})

	if err := NewStore(dir).AppendEvent("2026-07-29 supervisor: push failed with key " + deployKeyLine + " and master key " + masterKey); err != nil {
		t.Fatal(err)
	}
	if err := NewStore(dir).AppendHumanTask("task-20260729-1", "investigate: run failed with master key "+masterKey+" (source: supervisor; added 2026-07-29)"); err != nil {
		t.Fatal(err)
	}
	if err := NewStore(dir).AppendRunRecord(`{"run_id":"run_x","hint":"push failed with key ` + deployKeyLine + ` and master key ` + masterKey + `"}`); err != nil {
		t.Fatal(err)
	}
	for _, file := range []string{"events.md", "tasks.md", "runs.jsonl"} {
		raw, err := os.ReadFile(filepath.Join(NewStore(dir).Worktree(), file))
		if err != nil {
			t.Fatal(err)
		}
		text := string(raw)
		for _, secret := range []string{masterKey, deployKeyLine} {
			if strings.Contains(text, secret) {
				t.Errorf("%s carries an unredacted secret: %s", file, text)
			}
		}
		if !strings.Contains(text, "[REDACTED:MASTER_KEY]") {
			t.Errorf("%s missing redaction marker: %s", file, text)
		}
	}
}

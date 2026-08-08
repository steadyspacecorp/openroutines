package cli

import (
	"io"
	"os"
	"strings"
	"testing"

	"github.com/steadyspacecorp/openroutines/internal/creds"
)

func TestRoutinesTestCommandIsRemoved(t *testing.T) {
	if got := cmdRoutines([]string{"test", "digest"}); got != 2 {
		t.Fatalf("exit code = %d, want 2 for an unknown command", got)
	}
}

// A manual `routines run` in the production container spawns the same
// sandboxed model process a supervised run does, so a key layout boot would
// refuse has to be refused here too -- not accepted because the supervisor
// happens not to be the one asking.
func TestManualRunRefusesAKeyFileTheSandboxWouldGrant(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("OPENROUTINES_IN_CONTAINER", "1")
	t.Setenv(creds.EnvMasterKeyFile, "/usr/local/etc/master.key")
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	stderr := os.Stderr
	os.Stderr = w
	code := cmdRoutines([]string{"run", "digest"})
	os.Stderr = stderr
	w.Close()
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	if code == 0 {
		t.Fatal("a master key inside the granted read-only OS was accepted")
	}
	if !strings.Contains(string(out), creds.EnvMasterKeyFile) {
		t.Fatalf("the refusal should be the key preflight, not a later failure: %s", out)
	}
}

func TestParseRoutineRunArgs(t *testing.T) {
	for _, tc := range []struct {
		name     string
		args     []string
		wantName string
		wantNo   bool
		wantErr  bool
	}{
		{name: "ordinary run", args: []string{"digest"}, wantName: "digest"},
		{name: "flag after name", args: []string{"digest", "--skip-knowledge"}, wantName: "digest", wantNo: true},
		{name: "flag before name", args: []string{"--skip-knowledge", "digest"}, wantName: "digest", wantNo: true},
		{name: "missing name", args: []string{"--skip-knowledge"}, wantErr: true},
		{name: "duplicate flag", args: []string{"digest", "--skip-knowledge", "--skip-knowledge"}, wantErr: true},
		{name: "unknown flag", args: []string{"digest", "--dry-run"}, wantErr: true},
		{name: "extra argument", args: []string{"digest", "other"}, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gotName, gotNo, _, err := parseRoutineRunArgs(tc.args)
			if (err != nil) != tc.wantErr {
				t.Fatalf("error = %v, want error %v", err, tc.wantErr)
			}
			if gotName != tc.wantName || gotNo != tc.wantNo {
				t.Fatalf("got (%q, %v), want (%q, %v)", gotName, gotNo, tc.wantName, tc.wantNo)
			}
		})
	}
}

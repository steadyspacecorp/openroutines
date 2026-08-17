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

func TestManualRunRefusesAKeyFileTheSandboxWouldGrant(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("OPENROUTINES_IN_CONTAINER", "1")
	t.Setenv(creds.EnvMasterKey, "")
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
		{name: "flag after name", args: []string{"digest", "--write-knowledge"}, wantName: "digest", wantNo: true},
		{name: "flag before name", args: []string{"--write-knowledge", "digest"}, wantName: "digest", wantNo: true},
		{name: "missing name", args: []string{"--write-knowledge"}, wantErr: true},
		{name: "duplicate flag", args: []string{"digest", "--write-knowledge", "--write-knowledge"}, wantErr: true},
		{name: "unknown flag", args: []string{"digest", "--dry-run"}, wantErr: true},
		{name: "extra argument", args: []string{"digest", "other"}, wantErr: true},
		{name: "rehearse default", args: []string{"digest", "--rehearse"}, wantName: "digest"},
		{name: "rehearse scenario", args: []string{"digest", "quiet", "--rehearse"}, wantName: "digest"},
		{name: "scenario without rehearse", args: []string{"digest", "quiet"}, wantErr: true},
		{name: "rehearse with write", args: []string{"digest", "quiet", "--rehearse", "--write-knowledge"}, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gotName, _, gotNo, _, _, err := parseRoutineRunArgs(tc.args)
			if (err != nil) != tc.wantErr {
				t.Fatalf("error = %v, want error %v", err, tc.wantErr)
			}
			if gotName != tc.wantName || gotNo != tc.wantNo {
				t.Fatalf("got (%q, %v), want (%q, %v)", gotName, gotNo, tc.wantName, tc.wantNo)
			}
		})
	}
}

func TestResolveRehearsal(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	os.MkdirAll("rehearsals/announcements", 0o755)
	os.WriteFile("rehearsals/check-in.md", []byte("fixtures"), 0o644)
	os.WriteFile("rehearsals/announcements/default.md", []byte("fixtures"), 0o644)
	os.WriteFile("rehearsals/announcements/cold-start.md", []byte("fixtures"), 0o644)

	for _, tc := range []struct {
		routine, scenario, want, wantErr string
	}{
		{routine: "check-in", want: "rehearsals/check-in.md"},
		{routine: "announcements", want: "rehearsals/announcements/default.md"},
		{routine: "announcements", scenario: "cold-start", want: "rehearsals/announcements/cold-start.md"},
		{routine: "announcements", scenario: "quiet", wantErr: "have: cold-start, default"},
		{routine: "digest", want: ""},
		{routine: "digest", scenario: "quiet", wantErr: "rehearsals/digest/quiet.md"},
	} {
		got, err := resolveRehearsal(tc.routine, tc.scenario)
		if tc.wantErr != "" {
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("%s/%s: error = %v, want %q", tc.routine, tc.scenario, err, tc.wantErr)
			}
			continue
		}
		if err != nil || got != tc.want {
			t.Fatalf("%s/%s: got %q, %v; want %q", tc.routine, tc.scenario, got, err, tc.want)
		}
	}
}

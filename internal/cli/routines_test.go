package cli

import "testing"

func TestRoutinesTestCommandIsRemoved(t *testing.T) {
	if got := cmdRoutines([]string{"test", "digest"}); got != 2 {
		t.Fatalf("exit code = %d, want 2 for an unknown command", got)
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

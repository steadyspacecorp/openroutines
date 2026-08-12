package sandbox

import (
	"slices"
	"testing"
)

func TestOuterUserNamespacePrecedesBubblewrap(t *testing.T) {
	workspace := t.TempDir()
	cmd, err := (bubblewrap{proc: privateProc, outerUserNamespace: true}).Command(
		Attempt{Workspace: workspace}, "true",
	)
	if err != nil {
		t.Fatal(err)
	}

	wantPrefix := []string{
		"unshare", "--user", "--map-root-user", "--", "bwrap",
		"--unshare-pid", "--unshare-ipc", "--unshare-uts",
	}
	if len(cmd.Args) < len(wantPrefix) || !slices.Equal(cmd.Args[:len(wantPrefix)], wantPrefix) {
		t.Fatalf("command prefix = %q, want %q", cmd.Args, wantPrefix)
	}
	if slices.Contains(cmd.Args, "--unshare-user") {
		t.Fatalf("bubblewrap redundantly creates a nested user namespace: %q", cmd.Args)
	}
	if got := cmd.Args[len(cmd.Args)-2:]; !slices.Equal(got, []string{"--", "true"}) {
		t.Fatalf("command suffix = %q, want command separator and command", got)
	}
}

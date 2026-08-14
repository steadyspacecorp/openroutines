package sandbox

import (
	"slices"
	"testing"
)

func TestBubblewrapMountsWorkspaceWritable(t *testing.T) {
	workspace := t.TempDir()
	cmd, err := (bubblewrap{proc: privateProc}).Command(workspace, "true")
	if err != nil {
		t.Fatal(err)
	}

	if !containsSequence(cmd.Args, "--bind", workspace, workspace) {
		t.Fatalf("workspace is not a writable bind: %q", cmd.Args)
	}
	if containsSequence(cmd.Args, "--ro-bind", workspace, workspace) {
		t.Fatalf("workspace is still mounted read-only: %q", cmd.Args)
	}
}

func TestOuterUserNamespacePrecedesBubblewrap(t *testing.T) {
	workspace := t.TempDir()
	cmd, err := (bubblewrap{proc: privateProc, outerUserNamespace: true}).Command(workspace, "true")
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

func containsSequence(haystack []string, needle ...string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if slices.Equal(haystack[i:i+len(needle)], needle) {
			return true
		}
	}
	return false
}

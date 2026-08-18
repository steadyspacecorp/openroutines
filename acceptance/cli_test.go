//go:build acceptance

package acceptance

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rogpeppe/go-internal/testscript"
)

func TestCLI(t *testing.T) {
	testscript.Run(t, testscript.Params{
		Dir:  "testdata/cli",
		Cmds: map[string]func(*testscript.TestScript, bool, []string){"replace-line": replaceLine},
		Setup: func(env *testscript.Env) error {
			home := filepath.Join(env.WorkDir, "home")
			if err := os.Mkdir(home, 0o755); err != nil {
				return err
			}
			if err := os.WriteFile(filepath.Join(home, ".gitconfig"), []byte("[user]\n\tname = Acceptance\n\temail = acceptance@example.invalid\n"), 0o644); err != nil {
				return err
			}
			env.Setenv("HOME", home)
			env.Setenv("REPO", repoRoot)
			return nil
		},
		RequireExplicitExec: true,
		RequireUniqueNames:  true,
	})
}

func replaceLine(ts *testscript.TestScript, neg bool, args []string) {
	if neg || len(args) != 3 {
		ts.Fatalf("usage: replace-line file prefix replacement")
	}
	lines := strings.Split(ts.ReadFile(args[0]), "\n")
	replaced := 0
	for i, line := range lines {
		if strings.HasPrefix(line, args[1]) {
			lines[i] = args[2]
			replaced++
		}
	}
	if replaced != 1 {
		ts.Fatalf("replace-line: %s has %d lines beginning with %q", args[0], replaced, args[1])
	}
	ts.Check(os.WriteFile(ts.MkAbs(args[0]), []byte(strings.Join(lines, "\n")), 0o644))
}

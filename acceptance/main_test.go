//go:build acceptance

package acceptance

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

var (
	repoRoot        string
	testRoot        string
	openroutinesBin string
	isolationImage  string
)

func TestMain(m *testing.M) {
	var err error
	repoRoot, err = filepath.Abs("..")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	testRoot, err = os.MkdirTemp("", "openroutines-acceptance-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	binDir := filepath.Join(testRoot, "bin")
	if err := os.Mkdir(binDir, 0o755); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	openroutinesBin = filepath.Join(binDir, "openroutines")
	cmd := exec.Command("go", "build", "-o", openroutinesBin, "./cmd/openroutines")
	cmd.Dir = repoRoot
	if out, err := cmd.CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "building openroutines: %v\n%s", err, out)
		os.Exit(1)
	}
	if err := os.Setenv("PATH", binDir+string(filepath.ListSeparator)+os.Getenv("PATH")); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	code := m.Run()
	if isolationImage != "" {
		_ = exec.Command("docker", "image", "rm", "-f", isolationImage).Run()
	}
	if err := os.RemoveAll(testRoot); err != nil && code == 0 {
		fmt.Fprintln(os.Stderr, err)
		code = 1
	}
	os.Exit(code)
}

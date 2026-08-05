//go:build linux

package sandbox

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
	"testing"
)

func TestRestrictProcessGroupEscape(t *testing.T) {
	if os.Getenv("OPENROUTINES_PROCESS_GROUP_PROBE") == "1" {
		if err := RestrictProcessGroupEscape(); err != nil {
			t.Fatal(err)
		}
		if _, err := syscall.Setsid(); !errors.Is(err, syscall.EPERM) {
			t.Fatalf("setsid after confinement = %v, want EPERM", err)
		}
		if err := syscall.Setpgid(0, 0); !errors.Is(err, syscall.EPERM) {
			t.Fatalf("setpgid after confinement = %v, want EPERM", err)
		}
		return
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestRestrictProcessGroupEscape$")
	cmd.Env = append(os.Environ(), "OPENROUTINES_PROCESS_GROUP_PROBE=1")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("confined child: %v: %s", err, out)
	}
}

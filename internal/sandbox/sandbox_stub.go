//go:build !linux

package sandbox

import "fmt"

// The non-Linux stub: the filesystem sandbox is Linux-only.
func Apply(ro, rw []string) (string, []string, error) {
	_ = ro
	_ = rw
	return "", nil, ErrUnsupported
}

// A no-op off Linux: dumpable is a Linux procfs concept,
// and production supervision always runs in a Linux container.
func ProtectProcess() error { return nil }

// Reports that production UID isolation is Linux-only.
func DropIdentity(_ uint32) error {
	return fmt.Errorf("attempt uid isolation is unavailable on this platform")
}

// Unnecessary outside the Linux production container.
func ReapIdentity(_ uint32) error { return nil }

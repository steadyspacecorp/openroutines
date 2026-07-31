//go:build !linux

package sandbox

import "fmt"

// Apply is the non-Linux stub: the filesystem sandbox is Linux-only.
func Apply(ro, rw []string) (string, error) {
	_ = ro
	_ = rw
	return "", ErrUnsupported
}

// ProtectProcess is a no-op off Linux: dumpable is a Linux procfs concept,
// and production supervision always runs in a Linux container.
func ProtectProcess() error { return nil }

// DropIdentity reports that production UID isolation is Linux-only.
func DropIdentity(_ uint32) error {
	return fmt.Errorf("attempt uid isolation is unavailable on this platform")
}

// ReapIdentity is unnecessary outside the Linux production container.
func ReapIdentity(_ uint32) error { return nil }

//go:build !linux

package sandbox

// Apply is the non-Linux stub: the filesystem sandbox is Linux-only.
func Apply(ro, rw []string) (string, error) {
	_ = ro
	_ = rw
	return "", ErrUnsupported
}

// ProtectProcess is a no-op off Linux: dumpable is a Linux procfs concept,
// and production supervision always runs in a Linux container.
func ProtectProcess() error { return nil }

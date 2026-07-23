//go:build !linux

package sandbox

func Apply(ro, rw []string) (string, error) {
	_ = ro
	_ = rw
	return "", ErrUnsupported
}

// ProtectProcess is a no-op off Linux: dumpable is a Linux procfs concept,
// and production supervision always runs in a Linux container.
func ProtectProcess() error { return nil }

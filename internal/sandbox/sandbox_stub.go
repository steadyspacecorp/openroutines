//go:build !linux

package sandbox

func Apply(ro, rw []string) (string, error) {
	_ = ro
	_ = rw
	return "", ErrUnsupported
}

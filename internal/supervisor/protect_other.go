//go:build !linux

package supervisor

// protectSelf is a no-op off Linux, where the supervisor does not run in
// production: local runs execute the model process in the run container.
func protectSelf() error { return nil }

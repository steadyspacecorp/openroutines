//go:build !linux

package supervisor

func protectSelf() error { return nil }

// Package mode interprets the process's OpenRoutines deployment environment.
package mode

import "os"

const (
	// EnvContainer marks a process running inside the deployed agent container.
	EnvContainer = "OPENROUTINES_IN_CONTAINER"
	// EnvNative opts local development into the host opencode runtime.
	EnvNative = "OPENROUTINES_NATIVE"
)

// Mode is the process's current deployment mode.
type Mode struct {
	Container bool
	Native    bool
}

// Current reads the deployment mode from the process environment.
func Current() Mode {
	return Mode{
		Container: os.Getenv(EnvContainer) == "1",
		Native:    os.Getenv(EnvNative) == "1",
	}
}

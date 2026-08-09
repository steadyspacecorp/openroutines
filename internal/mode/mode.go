// Package mode interprets the process's OpenRoutines deployment environment.
package mode

import "os"

// Deployment modes.
const (
	// EnvContainer marks a process running inside the deployed agent container.
	EnvContainer = "OPENROUTINES_IN_CONTAINER"
	// EnvNative opts local development into the host opencode runtime.
	EnvNative = "OPENROUTINES_NATIVE"
)

// Deployment identifies where model processes execute.
type Deployment uint8

const (
	// LocalContainer runs opencode in a per-attempt container on the local host.
	LocalContainer Deployment = iota
	// LocalNative runs the developer's local opencode without confinement.
	LocalNative
	// DeployedContainer runs opencode inside the deployed agent container.
	DeployedContainer
)

// Current reads the deployment mode from the process environment.
func Current() Deployment {
	switch {
	case os.Getenv(EnvContainer) == "1":
		return DeployedContainer
	case os.Getenv(EnvNative) == "1":
		return LocalNative
	default:
		return LocalContainer
	}
}

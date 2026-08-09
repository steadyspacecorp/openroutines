package mode

import "os"

const (
	// Marks a process running inside the deployed agent container.
	EnvContainer = "OPENROUTINES_IN_CONTAINER"
	// Opts local development into the host opencode runtime.
	EnvNative = "OPENROUTINES_NATIVE"
)

// Identifies where model processes execute.
type Deployment uint8

const (
	// Runs opencode in a per-attempt container on the local host.
	LocalContainer Deployment = iota
	// Runs the developer's local opencode without confinement.
	LocalNative
	// Runs opencode inside the deployed agent container.
	DeployedContainer
)

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

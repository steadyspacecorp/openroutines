package mode

import "os"

const (
	EnvContainer = "OPENROUTINES_IN_CONTAINER"

	EnvNative = "OPENROUTINES_NATIVE"
)

type Deployment uint8

const (
	LocalContainer Deployment = iota

	LocalNative

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

package runner

import (
	"os"
)

// Isolation is what stands between a model process and everything it was
// not given. There is exactly one answer per deployment, decided by where
// the binary is running rather than by configuration -- see docs/design.md,
// "Runs are sandboxed -- and local runs use the production container".
type Isolation int

const (
	// Containerized runs the attempt in a disposable container built from the
	// image's runtime stage, with only its workspace mounted. The default,
	// and what a developer's `routines run` uses.
	Containerized Isolation = iota
	// Sandboxed runs the attempt in a sandbox of its own, as a child of the
	// supervisor inside the agent image. Production.
	Sandboxed
	// Unconfined runs opencode as an ordinary child of this process. A
	// contributor opt-in, never a deployment.
	Unconfined
)

// Confinement reports how model processes are isolated here. The production
// image sets OPENROUTINES_IN_CONTAINER=1 and outranks the contributor
// opt-out: a deployed agent cannot be talked out of its sandbox.
func Confinement() Isolation {
	switch {
	case os.Getenv("OPENROUTINES_IN_CONTAINER") == "1":
		return Sandboxed
	case os.Getenv("OPENROUTINES_NATIVE") == "1":
		return Unconfined
	default:
		return Containerized
	}
}

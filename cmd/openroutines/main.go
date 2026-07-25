// Command openroutines is the CLI: agent scaffolding, routine
// management, and the supervisor (see internal/cli).
package main

import (
	"os"

	"github.com/steadyspacecorp/openroutines/internal/cli"
)

func main() {
	os.Exit(cli.Run(os.Args[1:]))
}

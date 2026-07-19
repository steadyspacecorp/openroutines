package main

import (
	"os"

	"github.com/steadyspacecorp/openroutines/internal/cli"
)

func main() {
	os.Exit(cli.Run(os.Args[1:]))
}

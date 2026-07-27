// Package cli implements the openroutines command surface.
package cli

import (
	"fmt"
	"os"

	"github.com/steadyspacecorp/openroutines/internal/version"
)

const usage = `OpenRoutines -- run simple, secure, and durable autonomous AI agents

Usage:
  openroutines scaffold <path>      create a new agent repository
  openroutines configure            fill in agent.yaml, generate the master key
  openroutines check                validate the agent; made for CI
  openroutines status               show what the agent has and still needs
  openroutines usage                token use and reported cost per routine (--json)
  openroutines routines <command>   new, list, run, test, edit, activate, deactivate, remove
  openroutines skills <command>     new, list, remove
  openroutines plugin add <source>  install an inactive plugin bundle (--yes for non-interactive use)
  openroutines credentials <cmd>    set, list, remove
  openroutines supervise            run the scheduler (container entrypoint)
  openroutines update               bump the pinned framework version
  openroutines version              print the version

Run any command from inside an agent repository (except scaffold).
`

// Run dispatches a CLI invocation and returns the process exit code.
func Run(args []string) int {
	if len(args) == 0 {
		fmt.Print(usage)
		return 0
	}
	cmd, rest := args[0], args[1:]
	switch cmd {
	case "scaffold":
		return cmdScaffold(rest)
	case "configure":
		return cmdConfigure(rest)
	case "check":
		return cmdCheck(rest)
	case "routines", "routine":
		return cmdRoutines(rest)
	case "supervise":
		return cmdSupervise(rest)
	case "status":
		return cmdStatus(rest)
	case "usage":
		return cmdUsage(rest)
	case "skills", "skill":
		return cmdSkills(rest)
	case "plugin", "plugins":
		return cmdPlugin(rest)
	case "credentials", "credential":
		return cmdCredentials(rest)
	case "update":
		return cmdUpdate(rest)
	case "sandbox-exec": // internal: Landlock re-exec shim (see runner)
		return cmdSandboxExec(rest)
	case "sandbox-probe": // internal: boot-time availability check
		return cmdSandboxProbe(rest)
	case "version", "--version", "-v":
		fmt.Println(version.Version)
		return 0
	case "help", "--help", "-h":
		fmt.Print(usage)
		return 0
	default:
		fmt.Fprintf(os.Stderr, "openroutines: unknown command %q\n\n", cmd)
		fmt.Print(usage)
		return 2
	}
}

func fail(err error) int {
	fmt.Fprintf(os.Stderr, "openroutines: %v\n", err)
	return 1
}

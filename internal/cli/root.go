// Package cli implements the openroutines command surface.
package cli

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/steadyspacecorp/openroutines/internal/config"
	"github.com/steadyspacecorp/openroutines/internal/logging"
	"github.com/steadyspacecorp/openroutines/internal/version"
)

const usage = `OpenRoutines -- run simple, secure, and durable autonomous AI agents

Usage:
  openroutines scaffold <path>      create a new agent repository
  openroutines configure            fill in openroutines.yml, generate the master key
  openroutines check                validate the agent; made for CI
  openroutines status               show what the agent has and still needs
  openroutines usage                token use and reported cost per routine (--json)
  openroutines sync                 pull the agent's latest knowledge from origin (--push)
  openroutines routines <command>   new, list, run, edit, activate, deactivate, remove
  openroutines skills <command>     new, list, remove
  openroutines plugin <command>     add, list, update grouped plugin bundles
  openroutines credentials <cmd>    set, list, remove
  openroutines supervise            run the scheduler (container entrypoint)
  openroutines update               bump the pinned framework version
  openroutines version              print the version

Run any command from inside an agent repository (except scaffold).
`

// scaffold and the sandbox-* commands are exempt from the agent-repo check
// below: scaffold creates the repository, and sandbox commands are internal
// re-exec shims that run in the sandboxed workspace, not the agent checkout.
var commands = map[string]func([]string) int{
	"scaffold":            cmdScaffold,
	"configure":           cmdConfigure,
	"check":               cmdCheck,
	"routines":            cmdRoutines,
	"routine":             cmdRoutines,
	"supervise":           cmdSupervise,
	"status":              cmdStatus,
	"usage":               cmdUsage,
	"sync":                cmdSync,
	"skills":              cmdSkills,
	"skill":               cmdSkills,
	"plugin":              cmdPlugin,
	"plugins":             cmdPlugin,
	"credentials":         cmdCredentials,
	"credential":          cmdCredentials,
	"update":              cmdUpdate,
	"sandbox-exec":        cmdSandboxExec,
	"sandbox-probe":       cmdSandboxProbe,
	"sandbox-spawn-probe": cmdSandboxSpawnProbe,
	"sandbox-hold":        cmdSandboxHold,
	"sandbox-reclaim":     cmdSandboxReclaim,
}

var repoOptional = map[string]bool{
	"scaffold":            true,
	"sandbox-exec":        true,
	"sandbox-probe":       true,
	"sandbox-spawn-probe": true,
	"sandbox-hold":        true,
	"sandbox-reclaim":     true,
}

// Run dispatches a CLI invocation and returns the process exit code.
func Run(args []string) int {
	if len(args) == 0 {
		fmt.Print(usage)
		return 0
	}
	cmd, rest := args[0], args[1:]
	switch cmd {
	case "version", "--version", "-v":
		fmt.Println(version.Version)
		return 0
	case "help", "--help", "-h":
		fmt.Print(usage)
		return 0
	}

	handler, ok := commands[cmd]
	if !ok {
		fmt.Fprintf(os.Stderr, "openroutines: unknown command %q\n\n", cmd)
		fmt.Print(usage)
		return 2
	}

	// Asserting the agent-repo check here, before command-specific logic runs,
	// turns a wrong-directory mistake into an obvious message instead of a
	// confusing failure deep in whatever that command reads first (#64).
	if !repoOptional[cmd] {
		if _, err := os.Stat(config.Path(".")); err != nil {
			return fail(fmt.Errorf("not an agent repository (no %s found)", config.FileName))
		}
		setupLogging(".")
		// check already names the mismatch itself; avoid printing it twice.
		if cmd != "check" {
			warnOnPinMismatch(".")
		}
	}

	return handler(rest)
}

// Flags a binary that doesn't match the agent's pinned framework version --
// otherwise a schema mismatch surfaces as a confusing field-level error
// buried in a command's own output. Source builds are exempt: development
// runs against pinned agents on purpose.
func warnOnPinMismatch(dir string) {
	pin, err := os.ReadFile(filepath.Join(dir, ".openroutines", "version"))
	if err != nil {
		return
	}
	if v := strings.TrimSpace(string(pin)); v != version.Version && !strings.Contains(version.Version, "-dev") {
		slog.Warn("this binary does not match the agent's pinned framework version -- update the binary (curl -fsSL https://get.openroutines.dev/install.sh | bash) or the agent (openroutines update)", "binary", version.Version, "pin", v)
	}
}

// Assigns the process logger's level and timezone from the agent's
// configuration before any command runs. Best effort: a broken config or
// timezone keeps the load-time defaults so `check` can still run and name
// the problem itself.
func setupLogging(dir string) {
	agent, err := config.Load(dir)
	if err != nil {
		return
	}
	logging.Zone, _ = time.LoadLocation(agent.Timezone)
	logging.ConfigureLevel()
	if v, ok := logging.IgnoredLevel(); ok {
		slog.Warn("ignoring an unrecognized log level", "env", logging.EnvLevel, "value", v, "using", logging.Level.Level())
	}
}

func fail(err error) int {
	fmt.Fprintf(os.Stderr, "openroutines: %v\n", err)
	return 1
}

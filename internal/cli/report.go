package cli

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/steadyspacecorp/openroutines/internal/config"
	"github.com/steadyspacecorp/openroutines/internal/memory"
)

const reportUsage = "usage: openroutines report"

// checkInRoutine is the reporting routine report runs: the template's
// default delivery consumer (design decision "Every agent checks in").
const checkInRoutine = "check-in"

// cmdReport syncs memory, then -- after the operator confirms -- runs the
// check-in routine with its memory writes discarded, so its report echoes
// to the terminal without consuming the change feed. This is the interactive
// delivery path for the default check-in: supervised run output never enters
// the log stream, so on demand is how a person hears it.
func cmdReport(args []string) int {
	positional, _, help, err := parseFlags(args, nil)
	if err != nil {
		return fail(err)
	}
	if help {
		fmt.Println(reportUsage)
		return 0
	}
	if len(positional) != 0 {
		return fail(errors.New(reportUsage))
	}

	mem := memory.At(".")
	if err := mem.Ensure(); err != nil {
		return fail(err)
	}
	message, err := syncMemoryForRead(mem)
	if err != nil {
		return fail(err)
	}
	fmt.Printf("%s\n\n", message)

	// The check-in is a real run and spends a model invocation, so it never
	// starts without a person saying so. It writes nothing: --no-memory
	// discards the run's staged memory, so the pending change feed stays for
	// the scheduled check-in to consume.
	fmt.Printf("Run the %s routine now? It spends a model invocation; memory is untouched. [y/N] ", checkInRoutine)
	if !confirmed(os.Stdin) {
		fmt.Println("Not running it.")
		return 0
	}

	// The run happens at warn unless the operator already asked for a level:
	// the runner's and opencode's info chatter would bury the one thing this
	// command exists to show. The env override is the existing mechanism, and
	// setting it here reaches both streams -- the process handler via
	// setupLogging, and opencode via the --log-level the runner derives from
	// the same override.
	if os.Getenv(config.EnvLogLevel) == "" {
		if err := os.Setenv(config.EnvLogLevel, "warn"); err != nil {
			return fail(err)
		}
		setupLogging(".")
	}
	return routinesRun([]string{checkInRoutine, "--no-memory"})
}

// confirmed reads one line and accepts only an explicit yes: a closed or
// non-interactive stdin declines, so a script never triggers a run.
func confirmed(r io.Reader) bool {
	line, err := bufio.NewReader(r).ReadString('\n')
	if err != nil && line == "" {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return true
	}
	return false
}

func syncMemoryForRead(mem *memory.Memory) (string, error) {
	rep := mem.Sync()
	switch {
	case rep.NoOrigin:
		return "Memory is local only (no origin)", nil
	case rep.RemoteMissing:
		return fmt.Sprintf("Memory is local only (origin has no %s branch yet)", memory.Branch), nil
	case rep.Unreachable:
		return "", fmt.Errorf("origin unreachable: %s", rep.Detail)
	case rep.Rewritten:
		reportStranded(mem)
		return "", errors.New(rep.Detail)
	case rep.Conflict:
		reportStranded(mem)
		return "", fmt.Errorf("memory does not reconcile cleanly: %s\n\nresolve inside memory/, commit, then rerun openroutines report", rep.Detail)
	case rep.Detail != "":
		return "", errors.New(rep.Detail)
	default:
		return fmt.Sprintf("Memory is current with origin/%s", memory.Branch), nil
	}
}

package cli

import (
	"context"
	"fmt"
	"log/slog"
	"os/signal"
	"syscall"

	"github.com/steadyspacecorp/openroutines/internal/supervisor"
)

const superviseUsage = "usage: openroutines supervise"

// cmdSupervise runs the scheduler until SIGTERM/SIGINT: the container
// entrypoint.
func cmdSupervise(args []string) int {
	positional, _, help, err := parseFlags(args, nil)
	if err != nil {
		return fail(err)
	}
	if help {
		fmt.Println(superviseUsage)
		return 0
	}
	if len(positional) != 0 {
		return fail(fmt.Errorf("%s", superviseUsage))
	}

	s, err := supervisor.New(".")
	if err != nil {
		return fail(err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	if err := s.Run(ctx); err != nil {
		// The process logger -- scrubbing included -- has existed since
		// package load and was configured by the dispatch, so Run's
		// failure is logged rather than handed to fail().
		slog.Error("supervisor stopped", "error", err, "instance", s.InstanceID, "dir", s.Dir)
		return 1
	}
	return 0
}

package cli

import (
	"context"
	"os/signal"
	"syscall"

	"github.com/steadyspacecorp/openroutines/internal/supervisor"
)

// cmdSupervise runs the scheduler until SIGTERM/SIGINT: the container
// entrypoint.
func cmdSupervise(_ []string) int {
	s, err := supervisor.New(".")
	if err != nil {
		return fail(err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	if err := s.Run(ctx); err != nil {
		return fail(err)
	}
	return 0
}

package runner

import (
	"errors"
	"fmt"
	"log/slog"

	"github.com/steadyspacecorp/openroutines/internal/config"
	"github.com/steadyspacecorp/openroutines/internal/lock"
	"github.com/steadyspacecorp/openroutines/internal/sandbox"
)

// Reserves the manual attempt identity: the one uid past the supervisor's
// slot pool, pre-created by the template Dockerfile. Production refuses to
// spawn a model process without a reserved attempt uid, and only the
// supervisor holds the slot pool -- so `routines run` inside the container
// takes this fixed identity instead, locked so two manual runs cannot share
// it.
func reserveManualIdentity(dir string) (uint32, func(), error) {
	const uid = uint32(sandbox.AttemptUIDBase + config.MaxConcurrency)
	// Group membership is how the staged trees are shared with the identity.
	// The container's init may not have delivered the image's membership to
	// this process, so join the pool here; failure names the contract
	// violation instead of surfacing as a bare chgrp error mid-staging.
	if err := sandbox.EnsureAttemptGroups(config.MaxConcurrency + 1); err != nil {
		return 0, nil, fmt.Errorf("%w: %w", ErrFatal, err)
	}
	release, err := lock.Take(dir, "manual-attempt")
	if errors.Is(err, lock.ErrLocked) {
		return 0, nil, errors.New("another manual run holds the manual attempt identity -- try again when it finishes")
	}
	if err != nil {
		return 0, nil, err
	}
	// Prove the identity is empty before handing it out, not only after: a
	// previous manual run that died can leave an escaped descendant that
	// would share -- and be able to inspect -- this run's identity.
	if err := sandbox.ReapIdentity(uid); err != nil {
		release()
		return 0, nil, fmt.Errorf("manual attempt identity is not clean -- refusing to reuse it: %w", err)
	}
	return uid, func() {
		if err := sandbox.ReapIdentity(uid); err != nil {
			slog.Warn("manual attempt identity not proven empty at release", "uid", uid, "error", err)
		}
		release()
	}, nil
}

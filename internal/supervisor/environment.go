package supervisor

import (
	"errors"
	"fmt"
	"log/slog"

	"github.com/steadyspacecorp/openroutines/internal/creds"
	"github.com/steadyspacecorp/openroutines/internal/mode"
	"github.com/steadyspacecorp/openroutines/internal/repository"
	"github.com/steadyspacecorp/openroutines/internal/sandbox"
)

var supervisorKeyFiles = []struct {
	name, env string
	path      func(string) string
}{
	{"master key", creds.EnvMasterKeyFile, creds.MasterKeyFilePath},
	{"deploy key", repository.EnvDeployKeyFile, repository.DeployKeyFilePath},
}

// ValidateKeyFileLocations rejects a production key file that routines can
// read. It runs before every entry point that can spawn a routine.
func ValidateKeyFileLocations(dir string) error {
	if mode.Current() != mode.DeployedContainer {
		return nil
	}
	if sandbox.Disabled() {
		return nil
	}
	for _, key := range supervisorKeyFiles {
		if path := key.path(dir); path != "" && sandbox.Exposes(path) {
			return fmt.Errorf("the %s file at %s is in a directory routines can read; move it to /run/secrets or another directory routines cannot access, set %s to its new path, and see https://openroutines.dev/docs/deploying/", key.name, path, key.env)
		}
	}
	return nil
}

func validateCredentialStore(dir string) error {
	_, _, err := creds.Load(dir)
	if err == nil {
		return nil
	}
	if mode.Current() == mode.DeployedContainer {
		return fmt.Errorf("credentials are not usable in this deployed container: %w", err)
	}
	slog.Warn("credentials are not usable; routines may lack provider authentication", "error", err)
	return nil
}

// Settles at boot rather than mid-run what will stand between a model process
// and everything it was not given. Only production spawns model processes
// directly; elsewhere the run container is the boundary, or a contributor has
// opted out of both.
func verifyIsolation() error {
	switch mode.Current() {
	case mode.DeployedContainer:
		// A lower rung is a difference in degree; no rung at all is a difference
		// in kind, because an unconfined model process shares a uid and a
		// filesystem with every peer run and with the supervisor holding the keys.
		_, err := sandbox.Verify()
		switch {
		case errors.Is(err, sandbox.ErrDisabled):
			slog.Warn("run sandbox disabled -- model processes run unconfined", "disabled_by", sandbox.EnvDisable)
		case err != nil:
			slog.Error("no run sandbox could be built here", "detail", err)
			return fmt.Errorf("runs cannot be isolated on this host -- see https://openroutines.dev/docs/deploying/, or set %s=1 to run without a sandbox", sandbox.EnvDisable)
		}
	case mode.LocalNative:
		slog.Warn("OPENROUTINES_NATIVE=1 -- model processes run unconfined (dev mode)")
	case mode.LocalContainer:
		slog.Info("model processes run in the per-run container")
	}
	return nil
}

package supervisor

import (
	"errors"
	"fmt"
	"log/slog"
	"os"

	"github.com/steadyspacecorp/openroutines/internal/creds"
	"github.com/steadyspacecorp/openroutines/internal/knowledge"
	"github.com/steadyspacecorp/openroutines/internal/mode"
	"github.com/steadyspacecorp/openroutines/internal/sandbox"
)

// The secrets the supervisor holds and no run may have. Each arrives one of two
// ways -- a value in the environment, or a path to a mounted file -- so each
// answers the same two questions.
var supervisorKeys = []struct{ what, valueEnv, fileEnv string }{
	{"master key", creds.EnvMasterKey, creds.EnvMasterKeyFile},
	{"deploy key", knowledge.EnvDeployKey, knowledge.EnvDeployKeyFile},
}

// VerifyKeyDelivery inspects how the supervisor's own secrets arrived, in
// production only. It runs at boot and again before a manual `routines run`, so
// a layout boot would refuse cannot be accepted just because the supervisor is
// not the one asking. It composes here because neither creds nor knowledge can
// answer alone: the question is about a key *and* what the sandbox grants.
//
// A key file inside a granted path is fatal -- runs hold the supervisor's uid,
// so the file's mode stops nobody. A key value in the environment is a warning:
// weaker delivery, but out of reach of a run, whose environment is constructed
// rather than inherited. It fires on a leftover variable too.
func VerifyKeyDelivery() error {
	if mode.Current() != mode.DeployedContainer {
		return nil
	}
	// With the sandbox disabled there is no grant list to sit outside of, and
	// refusing the smaller version of a tradeoff the operator already took would
	// help nobody. The isolation check names that consequence.
	confined := !sandbox.Disabled()
	for _, key := range supervisorKeys {
		if path := os.Getenv(key.fileEnv); confined && path != "" && sandbox.Exposes(path) {
			return fmt.Errorf("%s=%s puts the %s inside a path every run's sandbox grants, where a run could read it -- runs share this process's uid, so the file's mode does not stop them. Mount it outside the granted OS paths instead (for example /run/secrets); see docs/operating.md", key.fileEnv, path, key.what)
		}
		if os.Getenv(key.valueEnv) != "" {
			slog.Warn("the "+key.what+" value is in this process's environment -- readable wherever that environment is; mount the key as a file, point the file variable at the path, and unset the value variable",
				"value_env", key.valueEnv, "file_env", key.fileEnv)
		}
	}
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
		rung, err := sandbox.Verify()
		switch {
		case errors.Is(err, sandbox.ErrDisabled):
			slog.Warn("run sandbox disabled -- model processes run unconfined", "disabled_by", sandbox.EnvDisable)
		case err != nil:
			// Which mechanism failed is for whoever is debugging the host, not
			// for the operator being told their agent will not start.
			slog.Debug("no run sandbox could be built here", "detail", err)
			return fmt.Errorf("runs cannot be isolated on this host -- see docs/operating.md, or set %s=1 to run without a sandbox", sandbox.EnvDisable)
		default:
			// Which rung won is an implementation detail, not an operator's choice.
			slog.Debug("run sandbox active for model processes", "rung", rung.Name())
		}
	case mode.LocalNative:
		slog.Warn("OPENROUTINES_NATIVE=1 -- model processes run unconfined (dev mode)")
	case mode.LocalContainer:
		slog.Info("model processes run in the per-run container")
	}
	return nil
}

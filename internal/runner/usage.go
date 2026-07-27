package runner

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

// Usage is one attempt's token consumption, summed from the assistant
// messages of the attempt's opencode session. CostReported is opencode's
// own estimate -- informational; tokens with the model and effort are the
// durable record, and dollars derive at read time.
type Usage struct {
	Input        int64   `json:"input"`
	Output       int64   `json:"output"`
	Reasoning    int64   `json:"reasoning"`
	CacheRead    int64   `json:"cache_read"`
	CacheWrite   int64   `json:"cache_write"`
	CostReported float64 `json:"-"`
}

// attemptHomeName is the disposable per-attempt home inside the run
// workspace. Production created it for sandbox hygiene (alpha.22); local
// container runs point HOME here too, which is what keeps the attempt's
// session data readable after the container exits.
const attemptHomeName = ".home"

// opencodeExec runs one opencode subcommand in the attempt's context --
// the caller decides where opencode exists (on PATH in the production
// container, via the runtime image for local runs, nowhere in native dev
// mode) and returns its stdout.
type opencodeExec func(args ...string) ([]byte, error)

// captureUsage reads what the attempt consumed. Preferred surface: ask
// opencode itself (session list + export -- messages live in its database
// from 1.18 on). Fallback: the pre-1.18 message JSONs on disk. The store
// is attempt-scoped either way (the home is fresh per attempt), and
// absence is nil, never zero -- bookkeeping must never fail a run.
func captureUsage(workspace string, oc opencodeExec) *Usage {
	if oc != nil {
		if u := captureViaExport(oc); u != nil {
			return u
		}
	}
	return captureFromLegacyFiles(workspace)
}

// captureViaExport asks opencode for the attempt's session: the fresh home
// holds at most one, `session list --format json` names it, and `export`
// prints {info, messages} with per-message token counts.
func captureViaExport(oc opencodeExec) *Usage {
	raw, err := oc("session", "list", "--format", "json", "-n", "1")
	if err != nil {
		return nil
	}
	var sessions []struct {
		ID string `json:"id"`
	}
	if json.Unmarshal(raw, &sessions) != nil || len(sessions) == 0 || sessions[0].ID == "" {
		return nil
	}
	raw, err = oc("export", sessions[0].ID)
	if err != nil {
		return nil
	}
	var export struct {
		Messages []struct {
			Info assistantInfo `json:"info"`
		} `json:"messages"`
	}
	if json.Unmarshal(raw, &export) != nil {
		return nil
	}
	var u Usage
	seen := false
	for _, m := range export.Messages {
		if m.Info.addTo(&u) {
			seen = true
		}
	}
	if !seen {
		return nil
	}
	return &u
}

// captureFromLegacyFiles sums the message JSONs opencode wrote before 1.18
// moved persistence into its database. Also the seam the fake opencode in
// tests writes to.
func captureFromLegacyFiles(workspace string) *Usage {
	pattern := filepath.Join(workspace, attemptHomeName, ".local", "share", "opencode", "storage", "message", "*", "*.json")
	files, _ := filepath.Glob(pattern)
	var u Usage
	seen := false
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		var msg assistantInfo
		if json.Unmarshal(raw, &msg) != nil {
			continue
		}
		if msg.addTo(&u) {
			seen = true
		}
	}
	if !seen {
		return nil
	}
	return &u
}

// assistantInfo is the slice of an opencode message record the capture
// reads -- identical in the export payload and the legacy files.
type assistantInfo struct {
	Role   string `json:"role"`
	Tokens *struct {
		Input     int64 `json:"input"`
		Output    int64 `json:"output"`
		Reasoning int64 `json:"reasoning"`
		Cache     struct {
			Read  int64 `json:"read"`
			Write int64 `json:"write"`
		} `json:"cache"`
	} `json:"tokens"`
	Cost float64 `json:"cost"`
}

// addTo folds one assistant message into the sum, reporting whether it
// counted.
func (m assistantInfo) addTo(u *Usage) bool {
	if m.Role != "assistant" || m.Tokens == nil {
		return false
	}
	u.Input += m.Tokens.Input
	u.Output += m.Tokens.Output
	u.Reasoning += m.Tokens.Reasoning
	u.CacheRead += m.Tokens.Cache.Read
	u.CacheWrite += m.Tokens.Cache.Write
	u.CostReported += m.Cost
	return true
}

// captureTimeout bounds each capture exec: a hung docker or opencode must
// not stall the supervisor's tick.
const captureTimeout = 30 * time.Second

// hostOpencodeExec runs opencode from PATH against the attempt home -- the
// production container, where the binary sits next to the supervisor.
func hostOpencodeExec(workspace string) opencodeExec {
	home := filepath.Join(workspace, attemptHomeName)
	return func(args ...string) ([]byte, error) {
		ctx, cancel := context.WithTimeout(context.Background(), captureTimeout)
		defer cancel()
		cmd := exec.CommandContext(ctx, "opencode", args...)
		cmd.Dir = workspace
		cmd.Env = []string{
			"PATH=" + os.Getenv("PATH"),
			"HOME=" + home,
			"XDG_DATA_HOME=" + filepath.Join(home, ".local", "share"),
		}
		return cmd.Output()
	}
}

// containerOpencodeExec re-enters the runtime image with the workspace
// mounted -- local runs, where opencode exists only inside the image. No
// network involved; the image is already local.
func containerOpencodeExec(workspace, image string) opencodeExec {
	return func(args ...string) ([]byte, error) {
		ctx, cancel := context.WithTimeout(context.Background(), captureTimeout)
		defer cancel()
		dargs := []string{
			"run", "--rm",
			"-v", workspace + ":/work",
			"-w", "/work",
			"-e", "HOME=/work/" + attemptHomeName,
			"-e", "XDG_DATA_HOME=/work/" + attemptHomeName + "/.local/share",
			image, "opencode",
		}
		cmd := exec.CommandContext(ctx, "docker", append(dargs, args...)...)
		return cmd.Output()
	}
}

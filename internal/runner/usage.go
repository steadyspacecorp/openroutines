package runner

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// Usage is one attempt's token consumption, summed from the assistant
// messages opencode persists in the attempt's disposable home. CostReported
// is opencode's own estimate -- informational; tokens with the model and
// effort are the durable record, and dollars derive at read time.
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
// container runs point HOME here too, which is what makes opencode's
// session storage readable after the container exits.
const attemptHomeName = ".home"

// captureUsage reads the opencode message store the attempt left behind and
// sums assistant-message tokens. Everything in the store belongs to this
// attempt (the home is fresh per attempt), messages are written
// incrementally (a timeout still leaves what was spent), and absence is
// nil, never zero -- native dev runs use the developer's real home, and
// bookkeeping must never fail a run.
func captureUsage(workspace string) *Usage {
	pattern := filepath.Join(workspace, attemptHomeName, ".local", "share", "opencode", "storage", "message", "*", "*.json")
	files, _ := filepath.Glob(pattern)
	var u Usage
	seen := false
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		var msg struct {
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
		if json.Unmarshal(raw, &msg) != nil || msg.Role != "assistant" || msg.Tokens == nil {
			continue
		}
		seen = true
		u.Input += msg.Tokens.Input
		u.Output += msg.Tokens.Output
		u.Reasoning += msg.Tokens.Reasoning
		u.CacheRead += msg.Tokens.Cache.Read
		u.CacheWrite += msg.Tokens.Cache.Write
		u.CostReported += msg.Cost
	}
	if !seen {
		return nil
	}
	return &u
}

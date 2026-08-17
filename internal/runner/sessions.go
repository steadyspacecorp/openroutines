package runner

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/steadyspacecorp/openroutines/internal/run"
	"github.com/steadyspacecorp/openroutines/internal/scrub"
)

func FormatUsage(u *Usage) string {
	if u == nil {
		return "usage unavailable"
	}
	parts := []string{fmt.Sprintf("%s input", formatUsageTokens(u.Input)), fmt.Sprintf("%s output", formatUsageTokens(u.Output))}
	if u.Reasoning > 0 {
		parts = append(parts, fmt.Sprintf("%s reasoning", formatUsageTokens(u.Reasoning)))
	}
	if u.CostReported > 0 {
		parts = append(parts, fmt.Sprintf("~$%.2f reported", u.CostReported))
	}
	return strings.Join(parts, " · ")
}

func formatUsageTokens(n int64) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1_000_000)
	case n >= 1_000:
		return fmt.Sprintf("%.1fk", float64(n)/1_000)
	default:
		return fmt.Sprintf("%d", n)
	}
}

type Usage = run.Tokens

type Capture struct {
	Usage   *Usage
	Failure string
}

type sessionExport struct {
	id  string
	raw []byte
}

type opencodeExec func(args ...string) ([]byte, error)

func fetchSessions(oc opencodeExec, log *slog.Logger) ([]sessionExport, error) {
	raw, err := completeJSON(oc, log, "session", "list", "--format", "json")
	if err != nil {
		return nil, err
	}
	var sessions []struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(raw, &sessions); err != nil {
		return nil, err
	}
	var exports []sessionExport
	var firstErr error
	for _, s := range sessions {
		if s.ID == "" {
			continue
		}
		raw, err := completeJSON(oc, log, "export", s.ID)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		exports = append(exports, sessionExport{id: s.ID, raw: raw})
	}
	return exports, firstErr
}

func completeJSON(oc opencodeExec, log *slog.Logger, args ...string) ([]byte, error) {
	raw, err := oc(args...)
	if err != nil {
		return nil, err
	}
	if json.Valid(raw) {
		return raw, nil
	}
	log.Warn("opencode returned truncated JSON -- retrying", "args", strings.Join(args, " "), "bytes", len(raw))
	raw, err = oc(args...)
	if err != nil {
		return nil, err
	}
	if json.Valid(raw) {
		return raw, nil
	}
	return nil, fmt.Errorf("opencode %s returned truncated JSON twice (%d bytes)", strings.Join(args, " "), len(raw))
}

func captureSessions(exports []sessionExport, log *slog.Logger) Capture {
	sessions, err := sessionMessages(exports)
	if err != nil {
		log.Warn("session capture unavailable -- no usage recorded and the session-outcome check did not run", "error", err)
		return Capture{}
	}
	if len(sessions) == 0 {
		log.Debug("attempt left no sessions")
	}
	return summarize(sessions)
}

func sessionMessages(exports []sessionExport) ([][]assistantInfo, error) {
	var groups [][]assistantInfo
	for _, s := range exports {
		var export struct {
			Messages []struct {
				Info assistantInfo `json:"info"`
			} `json:"messages"`
		}
		if err := json.Unmarshal(s.raw, &export); err != nil {
			return nil, err
		}
		msgs := make([]assistantInfo, len(export.Messages))
		for i, m := range export.Messages {
			msgs[i] = m.Info
		}
		groups = append(groups, msgs)
	}
	return groups, nil
}

const finishStop = "stop"

func summarize(sessions [][]assistantInfo) Capture {
	var s Capture
	var u Usage
	tokens := false
	for _, msgs := range sessions {
		for _, m := range msgs {
			if m.Role == "assistant" && m.addTo(&u) {
				tokens = true
			}
		}
	}
	if tokens {
		s.Usage = &u
	}
	if len(sessions) > 0 {
		s.Failure = judge(sessions[0])
	}
	// The claim quotes a model-writable record and outlives the mint
	// registration, so it is lifted redacted while the registration is live.
	s.Failure = scrub.Redacted(s.Failure)
	return s
}

func judge(msgs []assistantInfo) string {
	finished := false
	lastFinish := ""
	for _, m := range msgs {
		if m.Role != "assistant" {
			continue
		}
		if m.Error != nil {
			return "the model session ended on an error: " + m.Error.describe()
		}
		if m.Finish != "" {
			lastFinish = m.Finish
			finished = finished || m.Finish == finishStop
		}
	}
	if lastFinish != "" && !finished {
		return fmt.Sprintf("the model session never finished a turn (last step finished on %q) -- the agent loop stopped on a step it did not come back from", lastFinish)
	}
	return ""
}

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
	Cost   float64     `json:"cost"`
	Finish string      `json:"finish"`
	Error  *namedError `json:"error"`
}

type namedError struct {
	Name string `json:"name"`
	Data struct {
		Message string `json:"message"`
	} `json:"data"`
}

func (e *namedError) describe() string {
	if e.Data.Message != "" {
		return e.Data.Message
	}
	return e.Name
}

func (m assistantInfo) addTo(u *Usage) bool {
	if m.Tokens == nil {
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

// Operator storage is wired where the container is defined, not in the repo;
// exports land there whatever the attempt's outcome.
const EnvSessionDir = "OPENROUTINES_SESSION_DIR"

const sessionTimestampLayout = "20060102T150405Z"

func exportSessions(exports []sessionExport, routineName, runID string, log *slog.Logger) {
	root := os.Getenv(EnvSessionDir)
	if root == "" || len(exports) == 0 {
		return
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		log.Warn("session dir not writable -- sessions not preserved", "dir", root, "error", err)
		return
	}
	timestamp := time.Now().UTC().Format(sessionTimestampLayout)
	var firstErr error
	for _, s := range exports {
		name := strings.Join([]string{timestamp, routineName, runID, filepath.Base(s.id)}, "_") + ".json"
		if err := os.WriteFile(filepath.Join(root, name), s.raw, 0o600); err != nil {
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	if firstErr != nil {
		log.Warn("sessions exported incompletely", "dir", root, "error", firstErr)
	}
}

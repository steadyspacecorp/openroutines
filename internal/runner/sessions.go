// The attempt's sessions are opencode's own record of the run, fetched once
// after the model process exits. captureSessions distills token usage and
// whether the run really finished; exportSessions preserves the same payloads
// verbatim into operator storage.

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

// One attempt's token consumption, summed from the assistant
// messages of the attempt's opencode session. CostReported is opencode's
// own estimate -- informational; tokens with the model and effort are the
// durable record, and dollars derive at read time.
type Usage = run.Tokens

// What the run record keeps from the attempt's sessions. Usage is
// nil when the runtime didn't report -- never zero. Failure is empty unless
// the record positively says the session ended badly.
type Capture struct {
	Usage   *Usage
	Failure string
}

// One session's export payload, complete JSON as opencode
// rendered it. Fetched once and shared: capture reads its messages, export
// writes it verbatim.
type sessionExport struct {
	id  string
	raw []byte
}

// Runs one opencode subcommand in the attempt's context and
// returns its stdout -- satisfied by the opencode runtime's exec method.
type opencodeExec func(args ...string) ([]byte, error)

// Asks opencode for the attempt's sessions, in list order --
// most-recently-updated first (verified against 1.18.3), so the first export
// belongs to the session that acted last. Top-level sessions only: a
// subagent's child session is invisible to the CLI. A non-nil error means a
// partial read; the exports that arrived are still returned. No sessions is
// (nil, nil).
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

// Runs one opencode subcommand expected to print a JSON
// document, refusing a truncated one -- the backstop behind runToFile for
// any surface the file cannot cover. The loss is a race, so one retry
// usually returns the whole document.
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

// Distills the fetched sessions. Bookkeeping must never fail a run, so an
// unreadable export fails open.
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

// Reads each export's message records, one group per
// session in fetch order.
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

// The finish reason opencode records for an assistant message
// that ended its turn because the model was done -- as opposed to one that
// ended a step to call tools and never came back.
const finishStop = "stop"

// Folds the sessions into the Capture. Usage sums every session;
// the outcome is judged from the first-listed session alone -- a clean
// ending in an older session must not vouch for the one that held the run.
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

// Reads whether one session ended the way a finished run ends, on
// positive evidence only: an errored message, or finish reasons with no
// finished turn. A record that says nothing leaves the process's own verdict
// standing -- failing closed here would fail every run on the agent.
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

// The slice of an opencode message record the capture
// reads, measured against the real 1.18.3 runtime. Errored messages carry no
// `finish` at all, which is why the error check is not folded into the
// finish one.
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

// Opencode's error shape on a message: a tagged name with the
// provider's own message under data, when there is one.
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

// Folds one assistant message into the sum, reporting whether it
// counted.
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

// Designates operator storage for session history: when set, sessions land
// directly in this directory whatever the outcome. An env var, not
// configuration -- storage is wired up where the container is defined, not
// in the repo.
const EnvSessionDir = "OPENROUTINES_SESSION_DIR"

const sessionTimestampLayout = "20060102T150405Z"

// Saves the attempt's session history into operator storage. The writes run
// in the supervisor's process, so the model process never touches the volume.
// Best-effort throughout -- broken storage must never fail the run.
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

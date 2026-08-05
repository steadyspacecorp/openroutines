// The attempt's sessions are opencode's own record of the run, read twice
// after the model process exits. captureSessions distills them into what
// the run record keeps -- a Capture: token usage, and whether the run
// really finished. exportSessions preserves them verbatim into operator
// storage when one is designated. Capture reshapes; export copies.

package runner

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"

	"github.com/steadyspacecorp/openroutines/internal/scrub"
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

// Capture is what the run record keeps from the attempt's sessions -- not
// a session, but what their messages add up to: token consumption, and
// whether they ended the way a finished run ends. Usage is nil when the
// runtime didn't report -- never zero. Failure is empty unless the record
// positively says the session ended badly.
type Capture struct {
	Usage   *Usage
	Failure string
}

// captureSessions distills the attempt's sessions. The store is
// attempt-scoped (the home is fresh per attempt), and an unreadable one
// says nothing -- bookkeeping must never fail a run. A capture that fails
// open is silent by design, but the two things it costs -- usage
// reporting, and the "did the session end cleanly" check -- are worth a
// line: log carries what the return value can't.
func captureSessions(oc opencodeExec, log *slog.Logger) Capture {
	sessions, err := sessionMessages(oc)
	if err != nil {
		log.Warn("session capture unavailable -- no usage recorded and the session-outcome check did not run", "error", err)
	} else if len(sessions) == 0 {
		log.Debug("attempt left no sessions")
	}
	return summarize(sessions)
}

// sessionMessages asks opencode for the attempt's sessions: `session
// list --format json` names them, and `export` prints {info, messages}
// for each, returned one message group per session in list order. The
// list is ordered most-recently-updated first (verified against the
// pinned 1.18.3), so the first group belongs to the session that acted
// last. The fresh home normally holds exactly one session -- the run's
// own -- but reading every listed one keeps the sum honest if it ever
// holds more. The list surfaces top-level sessions only (`svc.list({roots:
// true})`), so a subagent's child session is invisible here -- its
// consumption is not capturable through the CLI, and no child can dilute
// the session-outcome check either. A non-nil error means the surface
// itself couldn't be read -- exec failure or unparseable JSON. No sessions
// yet is not an error: (nil, nil).
func sessionMessages(oc opencodeExec) ([][]assistantInfo, error) {
	raw, err := oc("session", "list", "--format", "json")
	if err != nil {
		return nil, err
	}
	var sessions []struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(raw, &sessions); err != nil {
		return nil, err
	}
	var groups [][]assistantInfo
	for _, s := range sessions {
		if s.ID == "" {
			continue
		}
		raw, err := oc("export", s.ID)
		if err != nil {
			return nil, err
		}
		var export struct {
			Messages []struct {
				Info assistantInfo `json:"info"`
			} `json:"messages"`
		}
		if err := json.Unmarshal(raw, &export); err != nil {
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

// finishStop is the finish reason opencode records for an assistant message
// that ended its turn because the model was done -- as opposed to one that
// ended a step to call tools and never came back.
const finishStop = "stop"

// summarize folds the sessions into the Capture. Usage sums every
// session's assistant messages -- a partial sum is a silently wrong usage
// record -- but the outcome is judged from the most significant session
// alone, the first-listed one that acted last: a clean ending in some
// older session must not vouch for the session that actually held the run.
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
	// registration that would otherwise redact it downstream (events, the
	// run record, the manual echo) -- so it is lifted redacted, while the
	// registration is still live.
	s.Failure = scrub.Redacted(s.Failure)
	return s
}

// judge reads whether one session ended the way a finished run ends. The
// failure claim rests on positive evidence only -- an errored message, or
// a runtime that reported finish reasons and never reported a finished
// turn. A record that says nothing about how it ended (an older opencode,
// a field that moves) leaves the process's own verdict standing, because a
// capture that failed open costs one confusing run record while one that
// failed closed would fail every run on the agent.
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

// assistantInfo is the slice of an opencode message record the capture
// reads from the export payload. Field
// names are measured against the real runtime, not just its schema: across
// 227 assistant messages written by opencode 1.18.3, `finish` was present
// on 224 (values `stop` and `tool-calls`) and `error` on 3, always shaped
// `{name, data:{message}}`. The three errored messages carried no `finish`
// at all, which is why the error check is not folded into the finish one.
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

// namedError is opencode's error shape on a message: a tagged name with the
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

// addTo folds one assistant message into the sum, reporting whether it
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

// EnvSessionDir designates operator storage for session history. When set,
// the attempt's sessions are exported when the attempt ends, whatever the
// outcome: `opencode session list` names them, `opencode export` renders
// each, and the output lands at `<dir>/<run_id>.<attempt_id>/<session_id>.json`
// (design decision "Run history: opencode's log passed through, sessions
// exported"). Unset means nothing is written. An env var rather than
// configuration for the same reason as the log-level override: storage --
// typically a mounted volume -- is wired up where the container is defined,
// not in the repo.
const EnvSessionDir = "OPENROUTINES_SESSION_DIR"

// sessionIDPattern is the shape of an id worth using as a filename. The ids
// come back from a session store the model process could write, so an id
// that could climb out of the attempt's directory names no file.
var sessionIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// exportSessions saves the attempt's session history into operator storage
// and returns the directory it landed in -- "" when no session dir is
// designated, the attempt left no sessions, or storage is broken. The
// exports run in the supervisor's process after the attempt ends: the model
// process never touches the volume, so no sandbox grant or container mount
// ever exposes it. Best-effort throughout -- broken operator storage must
// never fail the run. An export that fails partway still names its
// directory (the record points at what survived; the log carries the
// warning), but one that lands no file at all names nothing, because an
// empty directory is not a record.
func exportSessions(meta Meta, oc opencodeExec, log *slog.Logger) string {
	root := os.Getenv(EnvSessionDir)
	if root == "" {
		return ""
	}
	raw, err := oc("session", "list", "--format", "json")
	if err != nil {
		log.Warn("listing the attempt's sessions failed -- sessions not preserved", "error", err)
		return ""
	}
	var sessions []struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(raw, &sessions); err != nil {
		log.Warn("unreadable session list -- sessions not preserved", "error", err)
		return ""
	}
	if len(sessions) == 0 {
		log.Debug("attempt left no sessions to export")
		return ""
	}
	dir := filepath.Join(root, meta.RunID+"."+meta.AttemptID)
	if abs, err := filepath.Abs(dir); err == nil {
		dir = abs
	}
	// A retried attempt reuses its identity -- giveBack() hands the attempt
	// number back when a run is canceled or its lease is lost, after this
	// export already ran -- so the directory can hold a previous attempt's
	// files. One directory names one attempt's sessions, not two merged.
	_ = os.RemoveAll(dir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		log.Warn("session dir not writable -- sessions not preserved", "dir", dir, "error", err)
		return ""
	}
	wrote := false
	var firstErr error
	for _, s := range sessions {
		if err := exportSession(oc, s.ID, dir); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		wrote = true
	}
	if firstErr != nil {
		log.Warn("sessions exported incompletely", "dir", dir, "error", firstErr)
	}
	if !wrote {
		_ = os.RemoveAll(dir)
		return ""
	}
	return dir
}

// exportSession writes one session's export into the attempt's directory.
// Owner-only: verbatim, unscrubbed sessions are as sensitive as the
// credentials the routine could see.
func exportSession(oc opencodeExec, id, dir string) error {
	if !sessionIDPattern.MatchString(id) {
		return fmt.Errorf("session id %q is not a safe filename", id)
	}
	raw, err := oc("export", id)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, id+".json"), raw, 0o600)
}

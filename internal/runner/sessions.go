// The attempt's sessions are opencode's own record of the run. After the
// model process exits, that record is read twice: captureSession folds it
// into what the attempt records (token usage, whether the session ended
// cleanly), and exportSessions saves its replayable form into operator
// storage when one is designated.

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

// Session is what opencode's own record says about one attempt: what it
// consumed, and whether it ended the way a finished run ends. Usage is nil
// when the runtime didn't report -- never zero. Failure is empty unless the
// record positively says the session ended badly.
type Session struct {
	Usage   *Usage
	Failure string
}

// captureSession reads the attempt's session. Preferred surface: ask
// opencode itself (session list + export -- messages live in its database
// from 1.18 on). Fallback: the pre-1.18 message JSONs on disk. The store
// is attempt-scoped either way (the home is fresh per attempt), and an
// unreadable one says nothing -- bookkeeping must never fail a run. A
// capture that fails open is silent by design, but the two things it
// costs -- usage reporting, and the "did the session end cleanly" check --
// are worth a line each: log carries what the return value can't.
func captureSession(workspace string, oc opencodeExec, log *slog.Logger) Session {
	if oc != nil {
		msgs, err := messagesViaExport(oc)
		switch {
		case err != nil:
			log.Warn("session capture unavailable -- no usage recorded and the session-outcome check did not run", "error", err)
		case len(msgs) > 0:
			return summarize(msgs)
		}
		log.Debug("session capture fell back to on-disk message files")
	}
	msgs := messagesFromLegacyFiles(workspace)
	if len(msgs) == 0 {
		log.Debug("attempt session reported no assistant messages")
	}
	return summarize(msgs)
}

// messagesViaExport asks opencode for the attempt's session: the fresh home
// holds at most one, `session list --format json` names it, and `export`
// prints {info, messages}. A non-nil error means the surface itself
// couldn't be read -- exec failure or unparseable JSON -- and the fallback
// is worth trying. No session yet, or a session with no assistant
// messages, is not an error: (nil, nil).
func messagesViaExport(oc opencodeExec) ([]assistantInfo, error) {
	raw, err := oc("session", "list", "--format", "json", "-n", "1")
	if err != nil {
		return nil, err
	}
	var sessions []struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(raw, &sessions); err != nil {
		return nil, err
	}
	if len(sessions) == 0 || sessions[0].ID == "" {
		return nil, nil
	}
	raw, err = oc("export", sessions[0].ID)
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
	msgs := make([]assistantInfo, 0, len(export.Messages))
	for _, m := range export.Messages {
		msgs = append(msgs, m.Info)
	}
	return msgs, nil
}

// messagesFromLegacyFiles reads the message JSONs opencode wrote before 1.18
// moved persistence into its database. Also the seam the fake opencode in
// tests writes to.
func messagesFromLegacyFiles(workspace string) []assistantInfo {
	pattern := filepath.Join(workspace, attemptHomeName, ".local", "share", "opencode", "storage", "message", "*", "*.json")
	files, _ := filepath.Glob(pattern)
	var msgs []assistantInfo
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		var msg assistantInfo
		if json.Unmarshal(raw, &msg) != nil {
			continue
		}
		msgs = append(msgs, msg)
	}
	return msgs
}

// finishStop is the finish reason opencode records for an assistant message
// that ended its turn because the model was done -- as opposed to one that
// ended a step to call tools and never came back.
const finishStop = "stop"

// summarize folds the session's assistant messages into what the attempt
// records: token sums, and whether the session ended the way a finished run
// ends. The failure claim rests on positive evidence only -- an errored
// message, or a runtime that reported finish reasons and never reported a
// finished turn. A record that says nothing about how it ended (an older
// opencode, a field that moves) leaves the process's own verdict standing,
// because a capture that failed open costs one confusing run record while
// one that failed closed would fail every run on the agent.
func summarize(msgs []assistantInfo) Session {
	var s Session
	var u Usage
	tokens, finished := false, false
	lastFinish := ""
	for _, m := range msgs {
		if m.Role != "assistant" {
			continue
		}
		if m.addTo(&u) {
			tokens = true
		}
		if m.Error != nil && s.Failure == "" {
			s.Failure = "the model session ended on an error: " + m.Error.describe()
		}
		if m.Finish != "" {
			lastFinish = m.Finish
			finished = finished || (m.Finish == finishStop && m.Error == nil)
		}
	}
	if tokens {
		s.Usage = &u
	}
	if s.Failure == "" && lastFinish != "" && !finished {
		s.Failure = fmt.Sprintf("the model session never finished a turn (last step finished on %q) -- the agent loop stopped on a step it did not come back from", lastFinish)
	}
	// The claim quotes a model-writable record and outlives the mint
	// registration that would otherwise redact it downstream (events, the
	// run record, the manual echo) -- so it is lifted redacted, while the
	// registration is still live.
	s.Failure = scrub.Redacted(s.Failure)
	return s
}

// assistantInfo is the slice of an opencode message record the capture
// reads -- identical in the export payload and the legacy files. Field
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
	if root == "" || oc == nil {
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

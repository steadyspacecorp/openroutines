package supervisor

import (
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/steadyspacecorp/openroutines/internal/knowledge"
	"github.com/steadyspacecorp/openroutines/internal/routine"
)

func (s *Supervisor) syncOnce() {
	rep := s.store.Sync()
	switch {
	case rep.LocalOnly:
		return
	case rep.Rewritten:
		s.blockers.syncBlocked = true
		s.blockOnce("sync", "knowledge branch history rewritten on origin -- sync stopped, running on local state", errors.New(rep.Detail), &s.blockers.syncWarned)
		s.strandBlocked()
	case rep.Conflict:
		s.blockers.syncBlocked = true
		s.blockOnce("sync", "knowledge sync conflict -- sync stopped, running on local state", errors.New(rep.Detail), &s.blockers.syncWarned)
		s.strandBlocked()
	case rep.Unreachable:
		// Recorded locally, published when origin returns -- an outage whose
		// only trace is a log line in a replaced container is no trace.
		s.blockOnce("origin", "origin unreachable -- knowledge is not durable and no new runs start until it returns", errors.New(rep.Detail), &s.blockers.originWarned)
	case rep.Detail != "":
		// Sync could not even read the local worktree; an open blocker must
		// not be resolved on the strength of it.
		slog.Warn("knowledge sync did not run", "detail", rep.Detail)
	default:
		s.blockers.syncBlocked = false
		s.recover("sync", "knowledge sync with origin recovered", &s.blockers.syncWarned)
		s.recover("origin", "origin reachable again -- knowledge sync resumed", &s.blockers.originWarned)
		if rep.Adopted {
			slog.Info("knowledge: adopted remote commits")
		}
		if rep.RemoteMissing {
			slog.Debug("knowledge: origin has no knowledge branch yet -- the next push creates it")
		}
	}
}

// Broken files heal by editing, so load failures are events rather than
// human-owned tasks; unattributed failures stay in the log.
func (s *Supervisor) reportLoadFailures(errs []error, now time.Time) {
	failing := map[string]string{}
	for _, e := range errs {
		var re *routine.Error
		if !errors.As(e, &re) {
			slog.Warn("routine load error", "error", e)
			continue
		}
		failing[re.Name] = strings.TrimPrefix(e.Error(), s.Dir+string(filepath.Separator))
	}

	var news []string
	for _, name := range slices.Sorted(maps.Keys(failing)) {
		if s.blockers.loadFailed[name] != failing[name] {
			slog.Warn("routine load error", "routine", name, "error", failing[name])
			news = append(news, fmt.Sprintf("routine %s does not load (%s) -- it will not run until the file is fixed", name, failing[name]))
		}
	}
	for _, name := range slices.Sorted(maps.Keys(s.blockers.loadFailed)) {
		if _, still := failing[name]; !still {
			news = append(news, fmt.Sprintf("routine %s loads again", name))
		}
	}
	s.blockers.loadFailed = failing
	if len(news) == 0 {
		return
	}

	date := now.UTC().Format("2006-01-02")
	for _, line := range news {
		if err := s.store.AppendEvent(fmt.Sprintf("%s supervisor: %s", date, line)); err != nil {
			slog.Error("recording routine load status failed", "error", err)
			return
		}
		slog.Warn("routine load status changed", "status", line)
	}
	if _, err := s.store.Commit("Record routine load status"); err != nil {
		slog.Error("routine load status commit failed", "error", err)
		return
	}
	s.pushBestEffort()
}

// A blocker is a human-owned task -- only a person can clear it. The
// date-scoped id prevents a restart from re-recording the same onset.
func (s *Supervisor) blockOnce(kind, reason string, err error, warned *bool) {
	if *warned {
		return
	}
	*warned = true
	// BLOCKED and RECOVERED are literal, greppable markers: only these say
	// that dispatch itself is held.
	slog.Error("BLOCKED", "kind", kind, "reason", reason, "error", err)
	msg := reason
	if err != nil {
		msg = reason + ": " + err.Error()
	}
	date := time.Now().UTC().Format("2006-01-02")
	taskID := "task-" + kind + "-" + time.Now().UTC().Format("20060102")
	if aerr := s.store.AppendHumanTask(taskID, fmt.Sprintf("%s (source: supervisor; added %s)", msg, date)); aerr != nil {
		slog.Warn("could not record the supervisor blocker in knowledge -- this log line is the only copy",
			"kind", kind, "task_id", taskID, "error", aerr)
		*warned = false
		return
	}
	if _, cerr := s.store.Commit("Record supervisor blocker"); cerr != nil {
		slog.Warn("could not record the supervisor blocker in knowledge -- this log line is the only copy",
			"kind", kind, "task_id", taskID, "error", cerr)
		*warned = false
		return
	}
	s.pushBestEffort()
}

func (s *Supervisor) recover(kind, msg string, warned *bool) {
	*warned = false
	changed, err := s.store.ResolveHumanTasks("task-"+kind+"-",
		"done "+time.Now().UTC().Format("2006-01-02")+" -- "+msg)
	if err != nil {
		slog.Warn("could not resolve the supervisor blocker task -- it will read as open until repaired",
			"kind", kind, "error", err)
		return
	}
	if !changed {
		return
	}
	slog.Error("RECOVERED", "kind", kind, "reason", msg)
	_, _ = s.store.Commit("Resolve supervisor blocker")
	s.pushBestEffort()
}

// Publishes what the knowledge worktree carries. While sync is
// blocked the record goes to the supervisor-owned blocked ref instead; once
// the branch carries the same state, the stranded copy is dropped.
func (s *Supervisor) pushBestEffort() {
	if s.blockers.syncBlocked {
		s.strandBlocked()
		return
	}
	if err := s.store.Push(); err != nil {
		slog.Warn("knowledge push failed (will retry)", "error", err)
		return
	}
	if s.blockers.blockedTip != "" {
		s.blockers.blockedTip = ""
		s.store.ClearBlocked()
	}
}

// Publishes knowledge to the blocked ref on every blocked tick,
// so a failed attempt is retried rather than dying with the log line that
// announced it. Keyed on the tip: a tick that changed nothing pushes nothing.
func (s *Supervisor) strandBlocked() {
	tip, err := s.store.Head()
	if err != nil {
		slog.Error("could not read the knowledge tip -- blocked knowledge not stranded to origin", "error", err)
		return
	}
	if tip == s.blockers.blockedTip {
		return
	}
	if err := s.store.PublishBlocked(); err != nil {
		slog.Error("publishing blocked knowledge to origin failed (will retry)", "error", err)
		return
	}
	s.blockers.blockedTip = tip
	slog.Error("knowledge: stranded until sync is repaired", "ref", knowledge.BlockedRef)
}

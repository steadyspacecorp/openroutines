package supervisor

import (
	"errors"
	"fmt"
	"time"

	"github.com/steadyspacecorp/openroutines/internal/config"
	"github.com/steadyspacecorp/openroutines/internal/creds"
	"github.com/steadyspacecorp/openroutines/internal/routine"
	"github.com/steadyspacecorp/openroutines/internal/trigger"
)

// evaluateTrigger performs one change-detection poll for a routine with no
// cron firing due. It reports whether the routine should fire
// and whether durable trigger state changed (the caller folds that into the
// intent commit -- the newly observed value is persisted before the run acts).
//
// One interval governs everything: polls happen at most once per interval,
// and since a poll is the only fire opportunity, the same knob bounds fire
// rate and reply latency alike. The last-poll clock is in-memory only --
// persisting it would dirty the memory worktree on every poll; a restart
// costs one early poll.
func (s *Supervisor) evaluateTrigger(r *routine.Routine, now time.Time) (fired, dirty bool) {
	spec := *r.FM.Trigger
	interval, _ := spec.IntervalDuration() // validated by the caller

	if last, ok := s.lastPolled[r.Name]; ok && now.Before(last.Add(interval)) {
		return false, false
	}
	prior, err := trigger.Load(s.stateDir(), r.Name)
	if err != nil {
		s.Log.Printf("%s: %v", r.Name, err)
		return false, false
	}

	res, ok := s.poll(r, spec, prior, now)
	if !ok {
		return false, false
	}
	if prior == nil {
		// First observation establishes the baseline and never fires,
		// mirroring how a new consumer starts at the current commit.
		if err := res.Next.Save(s.stateDir()); err != nil {
			s.Log.Printf("%s: %v", r.Name, err)
			return false, false
		}
		s.Log.Printf("%s: trigger baseline established", r.Name)
		return false, true
	}
	if !res.Changed {
		return false, false
	}
	if err := res.Next.Save(s.stateDir()); err != nil {
		s.Log.Printf("%s: %v", r.Name, err)
		return false, false
	}
	return true, true
}

// refreshTriggerBaseline re-observes the endpoint without firing, called when
// a scheduled run is minted for a routine that also declares a trigger. Best
// effort: a failed refresh costs at most one redundant run later.
func (s *Supervisor) refreshTriggerBaseline(r *routine.Routine, now time.Time) (dirty bool) {
	prior, err := trigger.Load(s.stateDir(), r.Name)
	if err != nil {
		return false
	}
	res, ok := s.poll(r, *r.FM.Trigger, prior, now)
	if !ok || (prior != nil && !res.Changed) {
		return false
	}
	if err := res.Next.Save(s.stateDir()); err != nil {
		s.Log.Printf("%s: %v", r.Name, err)
		return false
	}
	return true
}

// poll performs the HTTP observation, resolving the declared credential and
// deduplicating error logs (log on transition, not every tick).
func (s *Supervisor) poll(r *routine.Routine, spec trigger.Spec, prior *trigger.State, now time.Time) (trigger.Result, bool) {
	s.lastPolled[r.Name] = now
	credential := ""
	if spec.Credential != "" {
		value, err := s.credentialValue(spec.Credential)
		if err != nil {
			if !s.pollFailed[r.Name] {
				s.pollFailed[r.Name] = true
				s.Log.Printf("%s: trigger credential %q: %v", r.Name, spec.Credential, err)
			}
			return trigger.Result{}, false
		}
		credential = value
	}
	res, err := trigger.Poll(trigger.Client, spec, credential, r.Name, prior)
	if err != nil {
		if !s.pollFailed[r.Name] {
			s.pollFailed[r.Name] = true
			s.Log.Printf("%s: trigger poll: %v", r.Name, err)
		}
		return trigger.Result{}, false
	}
	if s.pollFailed[r.Name] {
		s.Log.Printf("%s: trigger poll recovered", r.Name)
		delete(s.pollFailed, r.Name)
	}
	return res, true
}

// credentialValue decrypts one credential for a trigger poll. The supervisor
// holds the master key already; this is the one place it uses a routine
// credential itself, and the value goes only into an Authorization header.
// Typed credentials are refused: their stored value is a root secret that
// derivation exists to keep out of requests, and a poll sends its credential
// verbatim as a bearer token.
func (s *Supervisor) credentialValue(name string) (string, error) {
	agent, err := config.Load(s.Dir)
	if err != nil {
		return "", err
	}
	if spec := agent.Credentials[name]; spec.Type != "" {
		return "", fmt.Errorf("credential is typed (%s) and cannot authenticate a trigger poll", spec.Type)
	}
	key, err := creds.LoadKey(s.Dir)
	if err != nil {
		return "", err
	}
	store, err := creds.Read(s.Dir, key)
	if err != nil {
		return "", err
	}
	value, ok := store[name]
	if !ok {
		return "", errors.New("not present in the credentials store")
	}
	return value, nil
}

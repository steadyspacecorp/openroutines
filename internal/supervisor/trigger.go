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
// cron firing due, and reports whether the routine should fire. A newly
// observed value is saved to durable trigger state, which the tick's intent
// commit carries -- persisted before the run it fired acts.
//
// One interval governs everything: polls happen at most once per interval,
// and since a poll is the only fire opportunity, the same knob bounds fire
// rate and reply latency alike. The last-poll clock is in-memory only --
// persisting it would dirty the memory worktree on every poll; a restart
// costs one early poll.
func (s *Supervisor) evaluateTrigger(r *routine.Routine, now time.Time) bool {
	spec := *r.FM.Trigger
	interval, _ := spec.IntervalDuration() // validated by the caller

	if last, ok := s.lastPolled[r.Name]; ok && now.Before(last.Add(interval)) {
		return false
	}
	prior, err := trigger.Load(s.stateDir(), r.Name)
	if err != nil {
		s.Log.Printf("%s: %v", r.Name, err)
		return false
	}

	res, ok := s.poll(r, spec, prior, now)
	if !ok {
		return false
	}
	if prior == nil {
		// First observation establishes the baseline and never fires,
		// mirroring how a new consumer starts at the current commit.
		if err := res.Next.Save(s.stateDir()); err != nil {
			s.Log.Printf("%s: %v", r.Name, err)
			return false
		}
		s.Log.Printf("%s: trigger baseline established", r.Name)
		return false
	}
	if !res.Changed {
		return false
	}
	if err := res.Next.Save(s.stateDir()); err != nil {
		s.Log.Printf("%s: %v", r.Name, err)
		return false
	}
	return true
}

// refreshTriggerBaseline re-observes the endpoint without firing, called when
// a scheduled run is minted for a routine that also declares a trigger. Best
// effort: a failed refresh costs at most one redundant run later.
func (s *Supervisor) refreshTriggerBaseline(r *routine.Routine, now time.Time) {
	prior, err := trigger.Load(s.stateDir(), r.Name)
	if err != nil {
		return
	}
	res, ok := s.poll(r, *r.FM.Trigger, prior, now)
	if !ok || (prior != nil && !res.Changed) {
		return
	}
	if err := res.Next.Save(s.stateDir()); err != nil {
		s.Log.Printf("%s: %v", r.Name, err)
	}
}

// poll performs the HTTP observation, resolving the declared credential and
// deduplicating error logs (log on transition, not every tick).
func (s *Supervisor) poll(r *routine.Routine, spec trigger.Spec, prior *trigger.State, now time.Time) (trigger.Result, bool) {
	s.lastPolled[r.Name] = now
	credential := ""
	cleanup := func() {}
	if spec.Credential != "" {
		value, release, err := s.triggerCredential(spec.Credential)
		if err != nil {
			if !s.pollFailed[r.Name] {
				s.pollFailed[r.Name] = true
				s.Log.Printf("%s: trigger credential %q: %v", r.Name, spec.Credential, err)
			}
			return trigger.Result{}, false
		}
		credential = value
		cleanup = release
	}
	defer cleanup()
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

// triggerCredential materializes one credential for a trigger poll. Raw
// credentials retain their verbatim bearer behavior. Typed credentials are
// derived by the trusted supervisor, and only the type's explicit bearer
// surface leaves this function -- never its stored root secret. The caller
// must release derived material immediately after the poll.
func (s *Supervisor) triggerCredential(name string) (string, func(), error) {
	agent, err := config.Load(s.Dir)
	if err != nil {
		return "", nil, err
	}
	key, err := creds.LoadKey(s.Dir)
	if err != nil {
		return "", nil, err
	}
	store, err := creds.Read(s.Dir, key)
	if err != nil {
		return "", nil, err
	}
	value, ok := store[name]
	if !ok {
		return "", nil, errors.New("not present in the credentials store")
	}
	spec, typed := agent.Credentials[name]
	if !typed {
		return value, func() {}, nil
	}
	derived, err := creds.Derive(name, spec, value)
	if err != nil {
		return "", nil, err
	}
	if derived.Bearer == "" {
		derived.Cleanup()
		return "", nil, fmt.Errorf("credential type %s does not produce bearer material", spec.Type)
	}
	return derived.Bearer, derived.Cleanup, nil
}

package supervisor

import (
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/steadyspacecorp/openroutines/internal/config"
	"github.com/steadyspacecorp/openroutines/internal/creds"
	"github.com/steadyspacecorp/openroutines/internal/routine"
	"github.com/steadyspacecorp/openroutines/internal/trigger"
)

func (s *Supervisor) evaluateTrigger(r *routine.Routine, now time.Time) bool {
	spec := *r.Frontmatter.Trigger
	interval, _ := spec.IntervalDuration()

	log := r.Log()
	if last, ok := s.triggers.lastPolled[r.Name]; ok && now.Before(last.Add(interval)) {
		log.Debug("skipped", "reason", "poll interval not elapsed")
		return false
	}
	prior, err := trigger.Load(s.stateDir(), r.Name)
	if err != nil {
		log.Error("loading trigger state failed", "error", err)
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
			log.Error("saving trigger state failed", "error", err)
			return false
		}
		log.Info("trigger baseline established")
		return false
	}
	if !res.Changed {
		return false
	}
	if err := res.Next.Save(s.stateDir()); err != nil {
		log.Error("saving trigger state failed", "error", err)
		return false
	}
	return true
}

func (s *Supervisor) refreshTriggerBaseline(r *routine.Routine, now time.Time) {
	prior, err := trigger.Load(s.stateDir(), r.Name)
	if err != nil {
		r.Log().Warn("could not refresh the trigger baseline", "error", err)
		return
	}
	res, ok := s.poll(r, *r.Frontmatter.Trigger, prior, now)
	if !ok || (prior != nil && !res.Changed) {
		return
	}
	if err := res.Next.Save(s.stateDir()); err != nil {
		r.Log().Error("saving trigger state failed", "error", err)
	}
}

func (s *Supervisor) poll(r *routine.Routine, spec trigger.Spec, prior *trigger.State, now time.Time) (trigger.Result, bool) {
	s.triggers.lastPolled[r.Name] = now
	log := r.Log()
	credential := ""
	cleanup := func() {}
	if spec.Credential != "" {
		// The rule `check` errors on, enforced again at the point that
		// materializes the value: a poll uses a credential only when the
		// routine's own credentials list grants it.
		if !slices.Contains(r.Frontmatter.Credentials, spec.Credential) {
			if !s.triggers.pollFailed[r.Name] {
				s.triggers.pollFailed[r.Name] = true
				log.Warn("trigger credential is not listed in the routine's credentials", "credential", spec.Credential)
			}
			return trigger.Result{}, false
		}
		derived, err := s.triggerCredential(spec.Credential)
		if err != nil {
			if !s.triggers.pollFailed[r.Name] {
				s.triggers.pollFailed[r.Name] = true
				log.Warn("trigger credential failed", "credential", spec.Credential, "error", err)
			}
			return trigger.Result{}, false
		}
		credential = derived.Bearer
		cleanup = derived.Cleanup
	}
	defer cleanup()
	res, err := trigger.Poll(trigger.Client, spec, credential, r.Name, prior)
	if err != nil {
		if !s.triggers.pollFailed[r.Name] {
			s.triggers.pollFailed[r.Name] = true
			log.Warn("trigger poll failed", "error", err)
		}
		return trigger.Result{}, false
	}
	if s.triggers.pollFailed[r.Name] {
		log.Warn("trigger poll recovered")
		delete(s.triggers.pollFailed, r.Name)
	}
	return res, true
}

// Materializes one credential for a trigger poll. Raw credentials retain
// their verbatim bearer behavior. Typed credentials are derived by the
// trusted supervisor, and only the type's explicit bearer surface leaves
// this function -- never its stored root secret. The caller must run
// Cleanup immediately after the poll.
func (s *Supervisor) triggerCredential(name string) (*creds.Derived, error) {
	agent, err := config.Load(s.Dir)
	if err != nil {
		return nil, err
	}
	_, store, err := creds.Load(s.Dir)
	if err != nil {
		return nil, err
	}
	value, ok := store[name]
	if !ok {
		return nil, errors.New("not present in the credentials store")
	}
	spec, typed := agent.Credentials[name]
	if !typed {
		return &creds.Derived{Bearer: value, Cleanup: func() {}}, nil
	}
	derived, err := creds.Derive(name, spec, value)
	if err != nil {
		return nil, err
	}
	if derived.Bearer == "" {
		derived.Cleanup()
		return nil, fmt.Errorf("credential type %s does not produce bearer material", spec.Type)
	}
	return derived, nil
}

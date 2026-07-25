package cli

import (
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/robfig/cron/v3"

	"github.com/steadyspacecorp/openroutines/internal/config"
	"github.com/steadyspacecorp/openroutines/internal/creds"
	"github.com/steadyspacecorp/openroutines/internal/memory"
	"github.com/steadyspacecorp/openroutines/internal/routine"
	"github.com/steadyspacecorp/openroutines/internal/runner"
	"github.com/steadyspacecorp/openroutines/internal/skill"
)

// effortPattern loosely constrains reasoning-effort values -- providers
// define the real vocabulary; this just catches obvious mistakes.
var effortPattern = regexp.MustCompile(`^[a-z0-9-]+$`)

// cmdCheck validates the agent repository: agent.yaml, every routine's
// frontmatter, skill references, credential names, and deploy prerequisites.
// Exit code 1 on any failure -- made for CI.
func cmdCheck(args []string) int {
	dir := "."
	failures := 0
	warnings := 0
	failf := func(format string, a ...any) {
		failures++
		fmt.Printf("  ✗ %s\n", fmt.Sprintf(format, a...))
	}
	warnf := func(format string, a ...any) {
		warnings++
		fmt.Printf("  ! %s\n", fmt.Sprintf(format, a...))
	}
	okf := func(format string, a ...any) {
		fmt.Printf("  ✓ %s\n", fmt.Sprintf(format, a...))
	}

	// agent.yaml
	fmt.Println("agent.yaml")
	agent, err := config.Load(dir)
	if err != nil {
		failf("%v", err)
	} else if problems := agent.Problems(); len(problems) > 0 {
		for _, p := range problems {
			failf("%s", p)
		}
	} else {
		okf("valid (%s, %s)", agent.Name, agent.Timezone)
	}

	// Routines
	fmt.Println("routines/")
	routines, parseErrs := routine.LoadDir(filepath.Join(dir, "routines"))
	for _, e := range parseErrs {
		failf("%v", e)
	}
	for _, r := range routines {
		var errs []string
		if r.FM.Schedule == "" && r.FM.Trigger == nil {
			errs = append(errs, "needs a schedule or a trigger")
		}
		if r.FM.Schedule != "" {
			if _, err := cron.ParseStandard(r.FM.Schedule); err != nil {
				errs = append(errs, fmt.Sprintf("schedule %q: %v", r.FM.Schedule, err))
			}
		}
		if t := r.FM.Trigger; t != nil {
			if err := t.Validate(); err != nil {
				errs = append(errs, err.Error())
			} else if strings.HasPrefix(t.Poll, "http://") {
				warnf("%s: trigger polls over plain http -- the bearer credential and response travel unencrypted", r.Name)
			}
			if t.Credential != "" && !slices.Contains(r.FM.Credentials, t.Credential) {
				errs = append(errs, fmt.Sprintf("trigger credential %q must also be listed in credentials", t.Credential))
			}
			// A typed credential's stored value is a root secret (the run
			// receives derived material instead); a poll would send that
			// root secret as a bearer token.
			if t.Credential != "" && agent != nil && agent.Credentials[t.Credential].Type != "" {
				errs = append(errs, fmt.Sprintf("trigger credential %q is typed (%s) -- a poll would send the stored root secret as a bearer token; use a raw credential", t.Credential, agent.Credentials[t.Credential].Type))
			}
			if d, err := t.IntervalDuration(); err == nil && d < time.Minute {
				warnf("%s: trigger interval %q is below the 1m tick -- polls can't happen more often than the tick", r.Name, t.Interval)
			}
			if r.FM.Schedule == "" {
				warnf("%s: trigger with no schedule heartbeat -- a missed wake-up has no backstop", r.Name)
			}
		}
		if r.FM.Timeout != "" {
			if _, err := time.ParseDuration(r.FM.Timeout); err != nil {
				errs = append(errs, fmt.Sprintf("timeout %q is not a valid duration", r.FM.Timeout))
			}
		}
		if r.FM.Model != "" && !strings.Contains(r.FM.Model, "/") {
			errs = append(errs, fmt.Sprintf("model %q must be provider/model", r.FM.Model))
		}
		if r.FM.Effort != "" && !effortPattern.MatchString(r.FM.Effort) {
			errs = append(errs, fmt.Sprintf("effort %q should be a simple token like low, medium, high, or xhigh", r.FM.Effort))
		}
		if r.FM.Consumes != "" && r.FM.Consumes != "memory" {
			errs = append(errs, fmt.Sprintf("consumes %q: the only supported feed is \"memory\"", r.FM.Consumes))
		}
		for _, c := range r.FM.Credentials {
			if !creds.NamePattern.MatchString(c) {
				errs = append(errs, fmt.Sprintf("credential name %q must be lowercase snake_case", c))
			} else if strings.HasPrefix(c, creds.ReservedPrefix) {
				errs = append(errs, fmt.Sprintf("credential name %q collides with the reserved %s_* prefix", c, strings.ToUpper(creds.ReservedPrefix)))
			} else if creds.ReservedEnvName(c) {
				errs = append(errs, fmt.Sprintf("credential name %q would shadow the %s environment variable in the run", c, strings.ToUpper(c)))
			}
		}
		for _, s := range r.FM.Skills {
			// Grammar before path use: a declared name becomes a filesystem
			// path in the run pipeline.
			if !skill.NamePattern.MatchString(s) {
				errs = append(errs, fmt.Sprintf("skill name %q is not a valid Agent Skills name", s))
				continue
			}
			if _, err := os.Stat(filepath.Join(dir, "skills", s, "SKILL.md")); err != nil {
				errs = append(errs, fmt.Sprintf("skill %q not found in skills/", s))
			}
		}
		if !routine.NamePattern.MatchString(r.Name) {
			errs = append(errs, fmt.Sprintf("routine filename %q: names must be lowercase alphanumerics with hyphens/underscores (the filename is the routine's identity and becomes paths)", r.Name))
		}
		if r.Body == "" {
			errs = append(errs, "empty prompt body")
		}
		// Offline wiring validation: render the generated agent definition
		// (both modes) exactly as a run would -- no provider key, no Docker.
		if agent != nil {
			for _, dry := range []bool{false, true} {
				if _, rerr := runner.RenderDefinition(agent, r, dry); rerr != nil {
					errs = append(errs, fmt.Sprintf("generated definition: %v", rerr))
					break
				}
			}
		}
		if len(errs) == 0 {
			state := "active"
			if !r.FM.IsActive() {
				state = "inactive"
			}
			okf("%s (%s, %s)", r.Name, scheduleSummary(r), state)
		} else {
			for _, e := range errs {
				failf("%s: %s", r.Name, e)
			}
		}
	}
	if len(routines) == 0 && len(parseErrs) == 0 {
		warnf("no routines defined")
	}

	// Provider-auth readiness: every active routine's effective model needs
	// its provider key. Runs construct a clean environment, so there is no
	// ambient fallback in the container -- without the key, the first run
	// fails inside opencode with an opaque error, and production burns
	// retry attempts before anyone learns why.
	providerNeeds := map[string][]string{}
	if agent != nil {
		for _, r := range routines {
			if !r.FM.IsActive() {
				continue
			}
			model, merr := runner.EffectiveModel(agent, r)
			if merr != nil {
				continue // already reported against agent.yaml/frontmatter
			}
			keyName := creds.ProviderKeyName(strings.SplitN(model, "/", 2)[0])
			providerNeeds[keyName] = append(providerNeeds[keyName], r.Name)
		}
	}

	// Credentials store
	fmt.Println("credentials")
	if key, err := creds.LoadKey(dir); err != nil {
		if len(providerNeeds) > 0 {
			failf("%v -- active routines cannot authenticate to their model provider without it", err)
		} else {
			warnf("%v", err)
		}
	} else if store, err := creds.Read(dir, key); err != nil {
		failf("%v", err)
	} else {
		okf("credentials decrypt (%d stored)", len(store))
		for keyName, users := range providerNeeds {
			if _, ok := store[keyName]; !ok {
				failf("%s not set -- %s cannot authenticate (openroutines credentials set %s)", keyName, strings.Join(users, ", "), keyName)
			}
		}
		// Every declared credential must exist in the store.
		for _, r := range routines {
			for _, c := range r.FM.Credentials {
				if _, ok := store[c]; !ok {
					failf("%s declares credential %q, not present in %s", r.Name, c, creds.FileName)
				}
			}
		}
		if agent != nil {
			// A typed entry names a stored credential; one without a stored
			// value is dormant misconfiguration.
			for name, spec := range agent.Credentials {
				if _, ok := store[name]; !ok {
					warnf("credential entry %q (type %s) has no stored value in %s", name, spec.Type, creds.FileName)
				}
			}
			// github_app tokens expire after one hour; a routine that can run
			// close to that may lose authentication late in the attempt.
			for _, r := range routines {
				for _, c := range r.FM.Credentials {
					if agent.Credentials[c].Type == "github_app" && runner.EffectiveTimeout(agent, r) >= 50*time.Minute {
						warnf("%s: timeout %s approaches the one-hour github_app token life -- authentication may fail late in a run", r.Name, runner.EffectiveTimeout(agent, r))
					}
				}
			}
		}
		// A variable sharing a credential's name would be shadowed in the
		// run environment (the credential wins) -- rename one of them.
		if agent != nil {
			for name := range agent.Variables {
				if _, ok := store[name]; ok {
					failf("variable %q collides with a stored credential -- the credential wins in the run environment", name)
				}
			}
		}
	}

	// opencode.json is the harness's config file: the permission policy plus
	// custom provider endpoint definitions (an AI gateway, a proxy), both
	// interpreted by opencode alone -- and update never rewrites the file.
	// Model *choice* is framework config (the framework interprets it for
	// per-routine resolution and provider-key injection), so a harness-side
	// default model or anything else here is drift worth flagging (it has
	// arrived via coding-agent sessions before). Defined providers are
	// cross-checked against model prefixes: an unreferenced id usually means
	// a typo on one side.
	if raw, err := os.ReadFile(filepath.Join(dir, "opencode.json")); err == nil {
		var cfg map[string]any
		if json.Unmarshal(raw, &cfg) == nil {
			for _, key := range slices.Sorted(maps.Keys(cfg)) {
				if key != "$schema" && key != "permission" && key != "provider" {
					warnf("opencode.json contains %q -- model choice belongs in agent.yaml and frontmatter, not here", key)
				}
			}
			if providers, ok := cfg["provider"].(map[string]any); ok {
				prefixes := map[string]bool{}
				if agent != nil && agent.Defaults.Model != "" {
					prefixes[strings.SplitN(agent.Defaults.Model, "/", 2)[0]] = true
				}
				for _, r := range routines {
					if r.FM.Model != "" {
						prefixes[strings.SplitN(r.FM.Model, "/", 2)[0]] = true
					}
				}
				for _, id := range slices.Sorted(maps.Keys(providers)) {
					if !prefixes[id] {
						warnf("provider %q in opencode.json is not referenced by any model in agent.yaml defaults or routine frontmatter", id)
					}
				}
			}
		}
	}

	// Memory hygiene: task discipline is convention, not schema -- warn, never
	// rewrite. The supervisor does not interpret model-authored memory.
	if raw, err := os.ReadFile(filepath.Join(memory.WorktreePath(dir), "tasks.md")); err == nil {
		inFence := false
		for _, line := range strings.Split(string(raw), "\n") {
			t := strings.TrimSpace(line)
			if strings.HasPrefix(t, "```") || strings.HasPrefix(t, "~~~") {
				inFence = !inFence
				continue
			}
			if !inFence && (strings.HasPrefix(t, "- [ ]") || strings.HasPrefix(t, "- [x]")) && !strings.Contains(t, "`task-") {
				warnf("tasks.md entry without a stable `task-...` id: %.60s", t)
			}
		}
	}

	// Deploy prerequisites
	fmt.Println("deploy")
	if out, err := exec.Command("git", "-C", dir, "remote", "get-url", "origin").Output(); err != nil {
		warnf("no git origin -- required before deploy (memory needs a durable home)")
	} else {
		okf("origin %s", strings.TrimSpace(string(out)))
	}
	if pin, err := os.ReadFile(filepath.Join(dir, ".openroutines-version")); err == nil {
		v := strings.TrimSpace(string(pin))
		if strings.Contains(v, "-dev") {
			warnf("pinned %s -- a source-build version; the base image tag for it does not exist, so this agent cannot deploy until the pin points at a release", v)
		}
		if dockerfile, derr := os.ReadFile(filepath.Join(dir, "Dockerfile")); derr != nil {
			failf("Dockerfile: %v", derr)
		} else if !dockerfileUsesVersion(dockerfile, v) {
			failf("Dockerfile base image does not match .openroutines-version %s", v)
		} else {
			okf("Dockerfile base image matches %s", v)
		}
	}

	fmt.Println()
	if failures > 0 {
		fmt.Printf("check failed: %d problem(s), %d warning(s)\n", failures, warnings)
		return 1
	}
	fmt.Printf("check passed (%d warning(s))\n", warnings)
	return 0
}

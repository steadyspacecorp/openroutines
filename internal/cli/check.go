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
		if r.FM.Schedule == "" {
			errs = append(errs, "missing schedule")
		} else if _, err := cron.ParseStandard(r.FM.Schedule); err != nil {
			errs = append(errs, fmt.Sprintf("schedule %q: %v", r.FM.Schedule, err))
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
			}
		}
		for _, s := range r.FM.Skills {
			if _, err := os.Stat(filepath.Join(dir, "skills", s, "SKILL.md")); err != nil {
				errs = append(errs, fmt.Sprintf("skill %q not found in skills/", s))
			}
		}
		if r.Body == "" {
			errs = append(errs, "empty prompt body")
		}
		if len(errs) == 0 {
			state := "active"
			if !r.FM.IsActive() {
				state = "inactive"
			}
			okf("%s (%s, %s)", r.Name, r.FM.Schedule, state)
		} else {
			for _, e := range errs {
				failf("%s: %s", r.Name, e)
			}
		}
	}
	if len(routines) == 0 && len(parseErrs) == 0 {
		warnf("no routines defined")
	}

	// Credentials store
	fmt.Println("credentials")
	if key, err := creds.LoadKey(dir); err != nil {
		warnf("%v", err)
	} else if store, err := creds.Read(dir, key); err != nil {
		failf("%v", err)
	} else {
		okf("credentials decrypt (%d stored)", len(store))
		// Every declared credential must exist in the store.
		for _, r := range routines {
			for _, c := range r.FM.Credentials {
				if _, ok := store[c]; !ok {
					failf("%s declares credential %q, not present in %s", r.Name, c, creds.FileName)
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

	fmt.Println()
	if failures > 0 {
		fmt.Printf("check failed: %d problem(s), %d warning(s)\n", failures, warnings)
		return 1
	}
	fmt.Printf("check passed (%d warning(s))\n", warnings)
	return 0
}

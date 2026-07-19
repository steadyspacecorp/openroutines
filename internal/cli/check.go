package cli

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/robfig/cron/v3"

	"github.com/steadyspacecorp/openroutines/internal/config"
	"github.com/steadyspacecorp/openroutines/internal/creds"
	"github.com/steadyspacecorp/openroutines/internal/routine"
)

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
	seenIDs := map[string]string{}
	for _, r := range routines {
		var errs []string
		if r.FM.ID == "" {
			errs = append(errs, "missing id (openroutines routines new generates one)")
		} else if !routine.IDPattern.MatchString(r.FM.ID) {
			errs = append(errs, fmt.Sprintf("id %q is not a valid routine id", r.FM.ID))
		} else if other, dup := seenIDs[r.FM.ID]; dup {
			errs = append(errs, fmt.Sprintf("id %s duplicates %s -- a copied routine needs a new id", r.FM.ID, other))
		} else {
			seenIDs[r.FM.ID] = r.Name
		}
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
			okf("%s (%s, %s, %s)", r.Name, r.FM.ID, r.FM.Schedule, state)
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

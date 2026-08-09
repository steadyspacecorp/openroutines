package cli

import (
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
	"github.com/steadyspacecorp/openroutines/internal/knowledge"
	"github.com/steadyspacecorp/openroutines/internal/plugin"
	"github.com/steadyspacecorp/openroutines/internal/routine"
	"github.com/steadyspacecorp/openroutines/internal/runner"
	"github.com/steadyspacecorp/openroutines/internal/skill"
	"github.com/steadyspacecorp/openroutines/internal/version"
)

// effortPattern loosely constrains reasoning-effort values -- providers
// define the real vocabulary; this just catches obvious mistakes.
var effortPattern = regexp.MustCompile(`^[a-z0-9-]+$`)

const checkUsage = "usage: openroutines check"

// cmdCheck validates the agent repository: openroutines.yml, every routine's
// frontmatter, skill references, credential names, and deploy prerequisites.
// Exit code 1 on any failure -- made for CI.
func cmdCheck(args []string) int {
	positional, _, help, err := parseFlags(args, nil)
	if err != nil {
		return fail(err)
	}
	if help {
		fmt.Println(checkUsage)
		return 0
	}
	if len(positional) != 0 {
		return fail(fmt.Errorf("%s", checkUsage))
	}

	dir := "."
	report := &checkReport{}
	failf, warnf, okf := report.failf, report.warnf, report.okf

	// A binary that doesn't match the agent's pinned framework version reads
	// the repo through the wrong schema, so every divergence below surfaces
	// as a confusing field-level error. Name the real problem first, before
	// any of those can. Source builds are exempt: development runs against
	// pinned agents on purpose.
	if pin, err := os.ReadFile(filepath.Join(dir, ".openroutines", "version")); err == nil {
		if v := strings.TrimSpace(string(pin)); v != version.Version && !strings.Contains(version.Version, "-dev") {
			report.section("binary")
			failf("this binary is %s but the agent pins %s -- every finding below is suspect until they match: update the binary (curl -fsSL https://get.openroutines.dev/install.sh | bash) or the agent (openroutines update)", version.Version, v)
		}
	}

	configName := filepath.Base(config.Path(dir))
	report.section(configName)
	// opencode.json is loaded alongside: routine MCP grants are validated
	// against its server names, so a file opencode itself could not parse
	// must fail here, not surface as every grant looking undefined.
	oc, err := config.LoadOpenCode(dir)
	if err != nil {
		failf("%v", err)
	} else if oc.Missing {
		warnf("%s is missing -- runs fall back to opencode's defaults (session-title agent on, question tool allowed); scaffold ships a baseline, restore it", config.OpenCodeFileName)
	}
	agent, err := config.Load(dir)
	if err != nil {
		failf("%v", err)
	} else if problems := agent.Problems(); len(problems) > 0 {
		for _, p := range problems {
			failf("%s", p)
		}
	} else {
		okf("valid (%s, %s)", agent.Name, agent.Timezone)
		if configName != config.FileName {
			warnf("%s is a legacy configuration name -- rename it to %s (git mv %s %s); all spellings are read, and the rename is a one-line diff",
				configName, config.FileName, configName, config.FileName)
		}
	}

	// Skills -- checked first because plugin validation reads the agent's
	// skill names. A duplicate global name is dropped from the list rather
	// than returned, so the namespace errors have to be reported here or
	// nothing downstream will ever mention them.
	report.section("skills/")
	allSkills, skillErrs := skill.ListAgent(dir)
	for _, e := range skillErrs {
		failf("%v", e)
	}
	agentSkills := map[string]bool{}
	for _, s := range allSkills {
		agentSkills[s.Name] = true
	}
	if len(skillErrs) == 0 {
		if len(allSkills) == 0 {
			okf("no skills")
		} else {
			okf("%d skill(s), globally unique", len(allSkills))
		}
	}

	// Plugins
	report.section(".openroutines/plugins/")
	pluginEntries, pluginDirErr := os.ReadDir(filepath.Join(dir, ".openroutines", "plugins"))
	if pluginDirErr != nil && !os.IsNotExist(pluginDirErr) {
		failf("%v", pluginDirErr)
	}
	pluginCount := 0
	for _, entry := range pluginEntries {
		if !entry.IsDir() {
			failf(".openroutines/plugins/%s is not a plugin directory", entry.Name())
			continue
		}
		pluginCount++
		pluginDir := filepath.Join(dir, ".openroutines", "plugins", entry.Name())
		p, err := plugin.Load(pluginDir, agentSkills)
		if err != nil {
			failf("%s: %v", entry.Name(), err)
			continue
		}
		if p.Manifest.Name != entry.Name() {
			failf("%s: manifest name is %q", entry.Name(), p.Manifest.Name)
		}
		source, err := plugin.ReadSource(pluginDir)
		if err != nil {
			failf("%s: %v", entry.Name(), err)
		} else {
			okf("%s (%s @ %s)", entry.Name(), source.Repository, shortRevision(source.Revision))
		}
	}
	if pluginCount == 0 {
		okf("no plugins installed")
	}

	// Routines
	report.section("routines/")
	routines, parseErrs := routine.LoadAgent(dir)
	pluginRoutines, _ := routine.LoadPlugins(dir)
	pluginPaths := map[string][]string{}
	for _, r := range pluginRoutines {
		rel, err := filepath.Rel(dir, r.Path)
		if err == nil {
			pluginPaths[r.Name] = append(pluginPaths[r.Name], rel)
		}
	}
	for _, e := range parseErrs {
		failf("%v", e)
	}
	for _, r := range routines {
		if r.Path == filepath.Join(dir, "routines", r.Name+".md") {
			for _, shadowed := range pluginPaths[r.Name] {
				okf("%s overrides %s", r.Name, shadowed)
			}
		}
		checkRoutine(dir, agent, oc, r, report)
	}
	if len(routines) == 0 && len(parseErrs) == 0 {
		warnf("no routines defined")
	}

	// Rehearsal fixtures are bound to routines by name; an orphan is the
	// drift this convention exists to catch -- a routine renamed or removed
	// out from under the fixture that rehearses it.
	routineNames := map[string]bool{}
	for _, r := range routines {
		routineNames[r.Name] = true
	}
	if entries, err := os.ReadDir(filepath.Join(dir, "rehearsals")); err == nil {
		for _, entry := range entries {
			stem, isMD := strings.CutSuffix(entry.Name(), ".md")
			switch {
			case entry.IsDir() && !routineNames[entry.Name()]:
				warnf("rehearsals/%s/ matches no routine -- orphaned fixtures", entry.Name())
			case !entry.IsDir() && isMD && !routineNames[stem]:
				warnf("rehearsals/%s matches no routine -- orphaned fixture", entry.Name())
			}
		}
	}

	checkCredentials(dir, agent, routines, report)

	// opencode.json belongs to the harness and update never rewrites it, so
	// framework concerns that creep in stay until flagged; the schema
	// knowledge of what counts as drift lives with config.OpenCode.
	prefixes := map[string]bool{}
	if agent != nil && agent.Defaults.Model != "" {
		prefixes[strings.SplitN(agent.Defaults.Model, "/", 2)[0]] = true
	}
	for _, r := range routines {
		if r.FM.Model != "" {
			prefixes[strings.SplitN(r.FM.Model, "/", 2)[0]] = true
		}
	}
	for _, w := range oc.Drift(slices.Sorted(maps.Keys(prefixes))) {
		warnf("%s", w)
	}

	// Knowledge hygiene: task discipline is convention, not schema -- warn, never
	// rewrite. The supervisor does not interpret model-authored knowledge.
	if raw, err := os.ReadFile(filepath.Join(knowledge.At(dir).Worktree(), "tasks.md")); err == nil {
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
	report.section("deploy")
	if out, err := exec.Command("git", "-C", dir, "remote", "get-url", "origin").Output(); err != nil {
		warnf("no git origin -- required before deploy (knowledge needs a durable home)")
	} else {
		okf("origin %s", strings.TrimSpace(string(out)))
	}
	if pin, err := os.ReadFile(filepath.Join(dir, ".openroutines", "version")); err == nil {
		v := strings.TrimSpace(string(pin))
		if strings.Contains(v, "-dev") {
			warnf("pinned %s -- a source-build version; no release exists for it, so this agent cannot deploy until the pin points at a release", v)
		}
		if dockerfile, derr := os.ReadFile(filepath.Join(dir, "Dockerfile")); derr != nil {
			failf("Dockerfile: %v", derr)
		} else if !dockerfileUsesVersion(dockerfile, v) {
			failf("Dockerfile version pin does not match .openroutines/version %s", v)
		} else {
			okf("Dockerfile version pin matches %s", v)
		}
	}

	return report.print()
}

func checkCredentials(dir string, agent *config.Agent, routines []*routine.Routine, report *checkReport) {
	providerNeeds := map[string][]string{}
	if agent != nil {
		for _, r := range routines {
			if !r.FM.IsActive() {
				continue
			}
			model, err := runner.EffectiveModel(agent, r)
			if err != nil {
				continue
			}
			keyName := creds.ProviderKeyName(strings.SplitN(model, "/", 2)[0])
			providerNeeds[keyName] = append(providerNeeds[keyName], r.Name)
		}
	}
	report.section("credentials")
	key, err := creds.LoadKey(dir)
	if err != nil {
		if len(providerNeeds) > 0 {
			report.failf("%v -- active routines cannot authenticate to their model provider without it", err)
		} else {
			report.warnf("%v", err)
		}
		return
	}
	store, err := creds.Read(dir, key)
	if err != nil {
		report.failf("%v", err)
		return
	}
	report.okf("credentials decrypt (%d stored)", len(store))
	for _, keyName := range slices.Sorted(maps.Keys(providerNeeds)) {
		if _, ok := store[keyName]; !ok {
			report.failf("%s not set -- %s cannot authenticate (openroutines credentials set %s)", keyName, strings.Join(providerNeeds[keyName], ", "), keyName)
		}
	}
	for _, r := range routines {
		for _, credential := range r.FM.Credentials {
			if _, ok := store[credential]; !ok {
				report.failf("%s declares credential %q, not present in %s", r.Name, credential, creds.FileName)
			}
		}
	}
	if agent == nil {
		return
	}
	for _, name := range slices.Sorted(maps.Keys(agent.Credentials)) {
		spec := agent.Credentials[name]
		value, ok := store[name]
		if !ok {
			report.warnf("credential entry %q (type %s) has no stored value in %s", name, spec.Type, creds.FileName)
			continue
		}
		if err := creds.ValidateStored(spec, value); err != nil {
			report.failf("credential %q: %v -- re-store it: openroutines credentials set %s", name, err, name)
		}
	}
	for _, name := range slices.Sorted(maps.Keys(agent.Variables)) {
		if _, ok := store[name]; ok {
			report.failf("variable %q collides with a stored credential -- the credential wins in the run environment", name)
		}
	}
}

func checkRoutine(dir string, agent *config.Agent, oc *config.OpenCode, r *routine.Routine, report *checkReport) {
	var errs []string
	if r.FM.Schedule == "" && r.FM.Trigger == nil {
		errs = append(errs, "needs a schedule or a trigger")
	}
	if r.FM.Schedule != "" {
		if _, err := cron.ParseStandard(r.FM.Schedule); err != nil {
			errs = append(errs, fmt.Sprintf("schedule %q: %v", r.FM.Schedule, err))
		}
	}
	if trigger := r.FM.Trigger; trigger != nil {
		if err := trigger.Validate(); err != nil {
			errs = append(errs, err.Error())
		} else if strings.HasPrefix(trigger.Poll, "http://") {
			report.warnf("%s: trigger polls over plain http -- the bearer credential and response travel unencrypted", r.Name)
		}
		if trigger.Credential != "" && !slices.Contains(r.FM.Credentials, trigger.Credential) {
			errs = append(errs, fmt.Sprintf("trigger credential %q must also be listed in credentials", trigger.Credential))
		}
		if interval, err := trigger.IntervalDuration(); err == nil && interval < time.Minute {
			report.warnf("%s: trigger interval %q is below the 1m tick -- polls can't happen more often than the tick", r.Name, trigger.Interval)
		}
		if r.FM.Schedule == "" {
			report.warnf("%s: trigger with no schedule heartbeat -- a missed wake-up has no backstop", r.Name)
		}
	}
	if r.FM.Timeout != "" {
		if _, err := time.ParseDuration(r.FM.Timeout); err != nil {
			errs = append(errs, fmt.Sprintf("timeout %q is not a valid duration", r.FM.Timeout))
		}
	}
	if agent != nil {
		if declared, ceiling := runner.DeclaredTimeout(agent, r), agent.MaxRunTimeout(); declared > ceiling {
			report.warnf("%s: timeout %s exceeds the agent's %s ceiling, so attempts are capped at %s -- raise max_timeout in %s or split the work into shorter runs", r.Name, declared, ceiling, ceiling, config.FileName)
		}
	}
	if r.FM.Model != "" && !strings.Contains(r.FM.Model, "/") {
		errs = append(errs, fmt.Sprintf("model %q must be provider/model", r.FM.Model))
	}
	if r.FM.Effort != "" && !effortPattern.MatchString(r.FM.Effort) {
		errs = append(errs, fmt.Sprintf("effort %q should be a simple token like low, medium, high, or xhigh", r.FM.Effort))
	}
	for _, credential := range r.FM.Credentials {
		switch {
		case !creds.NamePattern.MatchString(credential):
			errs = append(errs, fmt.Sprintf("credential name %q must be lowercase snake_case", credential))
		case strings.HasPrefix(credential, creds.ReservedPrefix):
			errs = append(errs, fmt.Sprintf("credential name %q collides with the reserved %s_* prefix", credential, strings.ToUpper(creds.ReservedPrefix)))
		case creds.ReservedEnvName(credential):
			errs = append(errs, fmt.Sprintf("credential name %q would shadow the %s environment variable in the run", credential, strings.ToUpper(credential)))
		}
	}
	for _, skillName := range r.FM.Skills {
		if !skill.NamePattern.MatchString(skillName) {
			errs = append(errs, fmt.Sprintf("skill name %q is not a valid Agent Skills name", skillName))
			continue
		}
		if _, err := skill.Find(dir, skillName); err != nil {
			errs = append(errs, err.Error())
		}
	}
	for _, server := range r.FM.MCP {
		if !slices.Contains(oc.MCPServers(), server) {
			errs = append(errs, fmt.Sprintf("mcp server %q is not defined in opencode.json's mcp block", server))
		}
	}
	if !routine.NamePattern.MatchString(r.Name) {
		errs = append(errs, fmt.Sprintf("routine filename %q: names must be lowercase alphanumerics with hyphens/underscores (the filename is the routine's identity and becomes paths)", r.Name))
	}
	if r.Body == "" {
		errs = append(errs, "empty prompt body")
	}
	if agent != nil {
		if _, err := runner.RenderDefinition(agent, r, oc.MCPServers()); err != nil {
			errs = append(errs, fmt.Sprintf("generated definition: %v", err))
		}
	}
	if len(errs) > 0 {
		for _, err := range errs {
			report.failf("%s: %s", r.Name, err)
		}
		return
	}
	if r.FM.IsActive() {
		report.okf("%s (%s, active)", r.Name, scheduleSummary(r))
	} else {
		report.inactivef("%s (%s, inactive)", r.Name, scheduleSummary(r))
	}
}

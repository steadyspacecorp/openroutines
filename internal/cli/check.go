package cli

import (
	"fmt"
	"maps"
	"os"
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
	"github.com/steadyspacecorp/openroutines/internal/repository"
	"github.com/steadyspacecorp/openroutines/internal/routine"
	"github.com/steadyspacecorp/openroutines/internal/runner"
	"github.com/steadyspacecorp/openroutines/internal/skill"
	"github.com/steadyspacecorp/openroutines/internal/version"
)

var effortPattern = regexp.MustCompile(`^[a-z0-9-]+$`)

const checkUsage = "usage: openroutines check"

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
	checkBinaryVersion(dir, report)
	agent, opencode := checkConfiguration(dir, report)
	agentSkills := checkSkills(dir, report)
	checkPlugins(dir, agentSkills, report)
	routines := checkRoutines(dir, agent, opencode, report)
	checkRehearsals(dir, routines, report)
	checkCredentials(dir, agent, routines, report)
	checkOpenCodeDrift(agent, routines, opencode, report)
	checkKnowledgeTasks(dir, report)
	checkDeploy(dir, agent, report)
	return report.print()
}

func checkBinaryVersion(dir string, report *checkReport) {
	// A binary that doesn't match the agent's pinned framework version reads
	// the repo through the wrong schema, so name the real problem before any
	// field-level errors. Source builds intentionally run against pinned agents.
	if pin, err := readVersionPin(dir); err == nil && pin != version.Version && !strings.Contains(version.Version, "-dev") {
		report.section("binary")
		report.failf("this binary is %s but the agent pins %s -- every finding below is suspect until they match: update the binary (curl -fsSL https://get.openroutines.dev/install.sh | bash) or the agent (openroutines update)", version.Version, pin)
	}
}

func checkConfiguration(dir string, report *checkReport) (*config.Agent, *config.OpenCode) {
	configName := filepath.Base(config.Path(dir))
	report.section(configName)
	// Routine MCP grants are validated against opencode.json server names, so
	// a file opencode itself could not parse must fail here.
	opencode, err := config.LoadOpenCode(dir)
	if err != nil {
		report.failf("%v", err)
	} else if opencode.Missing {
		report.warnf("%s is missing -- runs fall back to opencode's defaults (session-title agent on, question tool allowed); scaffold ships a baseline, restore it", config.OpenCodeFileName)
	}
	agent, err := config.Load(dir)
	if err != nil {
		report.failf("%v", err)
	} else if problems := agent.Problems(); len(problems) > 0 {
		for _, problem := range problems {
			report.failf("%s", problem)
		}
	} else {
		report.okf("valid (%s, %s)", agent.Name, agent.Timezone)
		if configName != config.FileName {
			report.warnf("%s is a legacy configuration name -- rename it to %s (git mv %s %s); all spellings are read, and the rename is a one-line diff",
				configName, config.FileName, configName, config.FileName)
		}
	}
	return agent, opencode
}

func checkSkills(dir string, report *checkReport) map[string]bool {
	report.section("skills/")
	skills, errs := skill.ListAgent(dir)
	for _, err := range errs {
		report.failf("%v", err)
	}
	names := map[string]bool{}
	for _, skill := range skills {
		names[skill.Name] = true
	}
	if len(errs) == 0 {
		if len(skills) == 0 {
			report.okf("no skills")
		} else {
			report.okf("%d skill(s), globally unique", len(skills))
		}
	}
	return names
}

func checkPlugins(dir string, agentSkills map[string]bool, report *checkReport) {
	report.section(".openroutines/plugins/")
	entries, err := os.ReadDir(filepath.Join(dir, ".openroutines", "plugins"))
	if err != nil && !os.IsNotExist(err) {
		report.failf("%v", err)
	}
	count := 0
	for _, entry := range entries {
		if !entry.IsDir() {
			report.failf(".openroutines/plugins/%s is not a plugin directory", entry.Name())
			continue
		}
		count++
		pluginDir := filepath.Join(dir, ".openroutines", "plugins", entry.Name())
		installed, err := plugin.Load(pluginDir, agentSkills)
		if err != nil {
			report.failf("%s: %v", entry.Name(), err)
			continue
		}
		if installed.Manifest.Name != entry.Name() {
			report.failf("%s: manifest name is %q", entry.Name(), installed.Manifest.Name)
		}
		source, err := plugin.ReadSource(pluginDir)
		if err != nil {
			report.failf("%s: %v", entry.Name(), err)
		} else {
			report.okf("%s (%s @ %s)", entry.Name(), source.Repository, shortRevision(source.Revision))
		}
	}
	if count == 0 {
		report.okf("no plugins installed")
	}
}

func checkRoutines(dir string, agent *config.Agent, opencode *config.OpenCode, report *checkReport) []*routine.Routine {
	report.section("routines/")
	routines, parseErrs := routine.LoadAgent(dir)
	pluginRoutines, _ := routine.LoadPlugins(dir)
	pluginPaths := map[string][]string{}
	for _, routine := range pluginRoutines {
		rel, err := filepath.Rel(dir, routine.Path)
		if err == nil {
			pluginPaths[routine.Name] = append(pluginPaths[routine.Name], rel)
		}
	}
	for _, err := range parseErrs {
		report.failf("%v", err)
	}
	for _, routine := range routines {
		if routine.Path == filepath.Join(dir, "routines", routine.Name+".md") {
			for _, shadowed := range pluginPaths[routine.Name] {
				report.okf("%s overrides %s", routine.Name, shadowed)
			}
		}
		checkRoutine(dir, agent, opencode, routine, report)
	}
	if len(routines) == 0 && len(parseErrs) == 0 {
		report.warnf("no routines defined")
	}
	return routines
}

func checkRehearsals(dir string, routines []*routine.Routine, report *checkReport) {
	names := map[string]bool{}
	for _, routine := range routines {
		names[routine.Name] = true
	}
	entries, err := os.ReadDir(filepath.Join(dir, "rehearsals"))
	if err != nil {
		return
	}
	for _, entry := range entries {
		stem, isMarkdown := strings.CutSuffix(entry.Name(), ".md")
		switch {
		case entry.IsDir() && !names[entry.Name()]:
			report.warnf("rehearsals/%s/ matches no routine -- orphaned fixtures", entry.Name())
		case !entry.IsDir() && isMarkdown && !names[stem]:
			report.warnf("rehearsals/%s matches no routine -- orphaned fixture", entry.Name())
		}
	}
}

func checkOpenCodeDrift(agent *config.Agent, routines []*routine.Routine, opencode *config.OpenCode, report *checkReport) {
	// opencode.json belongs to the harness and update never rewrites it, so
	// framework concerns that creep in stay until check flags them.
	providers := map[string]bool{}
	if agent != nil && agent.Defaults.Model != "" {
		providers[strings.SplitN(agent.Defaults.Model, "/", 2)[0]] = true
	}
	for _, routine := range routines {
		if routine.Frontmatter.Model != "" {
			providers[strings.SplitN(routine.Frontmatter.Model, "/", 2)[0]] = true
		}
	}
	for _, warning := range opencode.Drift(slices.Sorted(maps.Keys(providers))) {
		report.warnf("%s", warning)
	}
}

func checkKnowledgeTasks(dir string, report *checkReport) {
	path := filepath.Join(knowledge.NewStore(dir).Worktree(), "tasks.md")
	raw, err := os.ReadFile(path)
	if err != nil {
		return
	}
	for _, task := range knowledge.ParseTaskEntries(string(raw)) {
		if !strings.HasPrefix(task.ID, "task-") {
			report.warnf("tasks.md entry without a stable `task-...` id: %.60s", task.Text)
		}
	}
}

func checkDeploy(dir string, agent *config.Agent, report *checkReport) {
	report.section("deploy")
	configured := ""
	if agent != nil {
		configured = strings.TrimSpace(agent.Repo)
	}
	if configured == "" {
		origin, _ := repository.Open(dir).Origin()
		if origin != "" {
			report.failf("repo is required for deployment -- record this checkout's origin (%s) in openroutines.yml", origin)
		} else {
			report.failf("repo is required for deployment -- set it in openroutines.yml")
		}
	} else if _, err := repository.GitOrigin(configured); err != nil {
		report.failf("%v", err)
	} else {
		report.okf("repo configured")
	}
	if pin, err := readVersionPin(dir); err == nil {
		if strings.Contains(pin, "-dev") {
			report.warnf("pinned %s -- a source-build version; no release exists for it, so this agent cannot deploy until the pin points at a release", pin)
		}
		if dockerfile, err := os.ReadFile(filepath.Join(dir, "Dockerfile")); err != nil {
			report.failf("Dockerfile: %v", err)
		} else if !dockerfileUsesVersion(dockerfile, pin) {
			report.failf("Dockerfile version pin does not match .openroutines/version %s", pin)
		} else {
			report.okf("Dockerfile version pin matches %s", pin)
		}
	}
}

func checkCredentials(dir string, agent *config.Agent, routines []*routine.Routine, report *checkReport) {
	providerNeeds := map[string][]string{}
	if agent != nil {
		for _, r := range routines {
			if !r.Frontmatter.IsActive() {
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
	_, store, err := creds.Load(dir)
	if err != nil {
		if len(providerNeeds) > 0 {
			report.failf("%v -- active routines cannot authenticate to their model provider without it", err)
		} else {
			report.warnf("%v", err)
		}
		return
	}
	report.okf("credentials decrypt (%d stored)", len(store))
	for _, keyName := range slices.Sorted(maps.Keys(providerNeeds)) {
		if _, ok := store[keyName]; !ok {
			report.failf("%s not set -- %s cannot authenticate (openroutines credentials set %s)", keyName, strings.Join(providerNeeds[keyName], ", "), keyName)
		}
	}
	for _, r := range routines {
		for _, credential := range r.Frontmatter.Credentials {
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
	if r.Frontmatter.Schedule == "" && r.Frontmatter.Trigger == nil {
		errs = append(errs, "needs a schedule or a trigger")
	}
	if r.Frontmatter.Schedule != "" {
		if _, err := cron.ParseStandard(r.Frontmatter.Schedule); err != nil {
			errs = append(errs, fmt.Sprintf("schedule %q: %v", r.Frontmatter.Schedule, err))
		}
	}
	if trigger := r.Frontmatter.Trigger; trigger != nil {
		if err := trigger.Validate(); err != nil {
			errs = append(errs, err.Error())
		} else if strings.HasPrefix(trigger.Poll, "http://") {
			report.warnf("%s: trigger polls over plain http -- the credential and response travel unencrypted", r.Name)
		}
		if trigger.Credential != "" && !slices.Contains(r.Frontmatter.Credentials, trigger.Credential) {
			errs = append(errs, fmt.Sprintf("trigger credential %q must also be listed in credentials", trigger.Credential))
		}
		if interval, err := trigger.IntervalDuration(); err == nil && interval < time.Minute {
			report.warnf("%s: trigger interval %q is below the 1m tick -- polls can't happen more often than the tick", r.Name, trigger.Interval)
		}
		if r.Frontmatter.Schedule == "" {
			report.warnf("%s: trigger with no schedule heartbeat -- a missed wake-up has no backstop", r.Name)
		}
	}
	if r.Frontmatter.Timeout != "" {
		if _, err := time.ParseDuration(r.Frontmatter.Timeout); err != nil {
			errs = append(errs, fmt.Sprintf("timeout %q is not a valid duration", r.Frontmatter.Timeout))
		}
	}
	if agent != nil {
		if declared, ceiling := runner.DeclaredTimeout(agent, r), agent.MaxRunTimeout(); declared > ceiling {
			report.warnf("%s: timeout %s exceeds the agent's %s ceiling, so attempts are capped at %s -- raise max_timeout in %s or split the work into shorter runs", r.Name, declared, ceiling, ceiling, config.FileName)
		}
	}
	if r.Frontmatter.Model != "" && !strings.Contains(r.Frontmatter.Model, "/") {
		errs = append(errs, fmt.Sprintf("model %q must be provider/model", r.Frontmatter.Model))
	}
	if r.Frontmatter.Effort != "" && !effortPattern.MatchString(r.Frontmatter.Effort) {
		errs = append(errs, fmt.Sprintf("effort %q should be a simple token like low, medium, high, or xhigh", r.Frontmatter.Effort))
	}
	for _, credential := range r.Frontmatter.Credentials {
		switch {
		case !creds.NamePattern.MatchString(credential):
			errs = append(errs, fmt.Sprintf("credential name %q must be lowercase snake_case", credential))
		case strings.HasPrefix(credential, creds.ReservedPrefix):
			errs = append(errs, fmt.Sprintf("credential name %q collides with the reserved %s_* prefix", credential, strings.ToUpper(creds.ReservedPrefix)))
		case creds.ReservedEnvName(credential):
			errs = append(errs, fmt.Sprintf("credential name %q would shadow the %s environment variable in the run", credential, strings.ToUpper(credential)))
		}
	}
	for _, skillName := range r.Frontmatter.Skills {
		if !skill.NamePattern.MatchString(skillName) {
			errs = append(errs, fmt.Sprintf("skill name %q is not a valid Agent Skills name", skillName))
			continue
		}
		if _, err := skill.Find(dir, skillName); err != nil {
			errs = append(errs, err.Error())
		}
	}
	for _, server := range r.Frontmatter.MCP {
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
	if r.Frontmatter.IsActive() {
		report.okf("%s (%s, active)", r.Name, scheduleSummary(r))
	} else {
		report.inactivef("%s (%s, inactive)", r.Name, scheduleSummary(r))
	}
}

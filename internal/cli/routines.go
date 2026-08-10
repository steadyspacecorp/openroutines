package cli

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/steadyspacecorp/openroutines/internal/knowledge"
	"github.com/steadyspacecorp/openroutines/internal/routine"
	"github.com/steadyspacecorp/openroutines/internal/runner"
)

const newRoutineTemplate = `---
schedule: "0 9 * * *"
active: false
skills: []
credentials: []
---
Describe the job here. The body of this file is the prompt: say what to do,
what knowledge to consult (your ledger is knowledge/ledgers/%s.md), and what to
record when done. Set active: true when it's ready to run on schedule.
`

func cmdRoutines(args []string) int {
	if len(args) == 0 {
		fmt.Print(routinesUsage)
		return 2
	}
	if wantsHelp(args[:1]) {
		fmt.Print(routinesUsage)
		return 0
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "new":
		return routinesNew(rest)
	case "list":
		positional, _, help, err := parseFlags(rest, nil)
		if err != nil {
			return fail(err)
		}
		if help {
			fmt.Println("usage: openroutines routines list")
			return 0
		}
		if len(positional) != 0 {
			return fail(fmt.Errorf("usage: openroutines routines list"))
		}
		return routinesList()
	case "run":
		return routinesRun(rest)
	case "edit":
		return routinesEdit(rest)
	case "activate":
		return routinesSetActive(rest, true)
	case "deactivate":
		return routinesSetActive(rest, false)
	case "remove":
		return routinesRemove(rest)
	default:
		fmt.Fprintf(os.Stderr, "openroutines: unknown routines command %q\n\n", sub)
		fmt.Print(routinesUsage)
		return 2
	}
}

const routinesUsage = `Manage this agent's routines (markdown files in routines/)

Usage:
  openroutines routines new <name>         create a routine (inactive until you activate it)
  openroutines routines list               names, schedules, grants
  openroutines routines run <name> [--write-knowledge] [--rehearse [scenario]]
                                           run once now, knowledge writes discarded; --write-knowledge
                                           settles them; --rehearse runs against rehearsals/ fixtures
                                           with every grant stripped
  openroutines routines edit <name>        open in $EDITOR, validate on close
  openroutines routines activate <name>    set active: true
  openroutines routines deactivate <name>  set active: false
  openroutines routines remove <name>      delete the routine and its scheduling state
`

func routinesRun(args []string) int {
	nameArg, scenario, writeKnowledge, rehearse, help, err := parseRoutineRunArgs(args)
	if err != nil {
		return fail(err)
	}
	if help {
		fmt.Println(routinesRunUsage)
		return 0
	}
	name, err := routineName(nameArg)
	if err != nil {
		return fail(err)
	}
	fixture := ""
	if rehearse {
		if fixture, err = resolveRehearsal(name, scenario); err != nil {
			return fail(err)
		}
		if fixture != "" {
			fmt.Printf("rehearsal: fixture world from %s -- grants stripped, knowledge discarded\n\n", fixture)
		} else {
			fmt.Println("rehearsal: live world -- external actions instructed read-only, knowledge discarded")
			fmt.Println()
		}
	}
	result, err := runner.RunManual(".", name, runner.ManualOptions{
		DiscardKnowledge: !writeKnowledge,
		Rehearse:         rehearse,
		Fixture:          fixture,
	})
	if err != nil {
		return fail(err)
	}
	fmt.Printf("\n%s: %s in %s (run %s)\n", name, result.Outcome, result.Duration, result.RunID)
	if result.Hint != "" {
		fmt.Println(result.Hint)
	}
	for _, conflict := range result.Conflicts {
		fmt.Printf("note: concurrent knowledge edit -- canonical %s left unchanged; competing version saved at %s; curate them if they disagree\n", conflict.Path, conflict.Quarantine)
	}
	if !writeKnowledge {
		fmt.Println("knowledge discarded: external actions were still performed and credentials were available -- pass --write-knowledge for a run that settles (first runs may still have initialized the knowledge worktree)")
	} else if result.Commit != "" {
		fmt.Printf("knowledge updated: commit %s on the %s branch\n", result.Commit, "knowledge")
	}
	if result.Outcome != runner.Completed {
		return 1
	}
	return 0
}

const routinesRunUsage = "usage: openroutines routines run <name> [--write-knowledge] [--rehearse [scenario]]"

func parseRoutineRunArgs(args []string) (name, scenario string, writeKnowledge, rehearse, help bool, err error) {
	positional, flags, help, err := parseFlags(args, map[string]flagSpec{"--write-knowledge": {}, "--rehearse": {}})
	if err != nil || help {
		return "", "", false, false, help, err
	}
	_, writeKnowledge = flags["--write-knowledge"]
	_, rehearse = flags["--rehearse"]
	switch {
	case len(positional) == 1:
		return positional[0], "", writeKnowledge, rehearse, false, nil
	case len(positional) == 2 && rehearse:
		if writeKnowledge {
			return "", "", false, false, false, errors.New("--rehearse and --write-knowledge cannot combine: a rehearsal never settles")
		}
		return positional[0], positional[1], writeKnowledge, rehearse, false, nil
	}
	if rehearse && writeKnowledge {
		return "", "", false, false, false, errors.New("--rehearse and --write-knowledge cannot combine: a rehearsal never settles")
	}
	return "", "", false, false, false, fmt.Errorf("%s", routinesRunUsage)
}

// Maps a routine and optional scenario to its fixture file:
// rehearsals/<name>.md for a single fixture, or rehearsals/<name>/<scenario>.md
// (default.md when unnamed) once a routine has more than one. No fixtures at
// all selects the live rehearsal rather than erroring.
func resolveRehearsal(name, scenario string) (string, error) {
	flat := filepath.Join("rehearsals", name+".md")
	dir := filepath.Join("rehearsals", name)
	if scenario == "" {
		if _, err := os.Stat(flat); err == nil {
			return flat, nil
		}
		if p := filepath.Join(dir, "default.md"); fileExists(p) {
			return p, nil
		}
	} else {
		if filepath.Base(scenario) != scenario || strings.HasSuffix(scenario, ".md") {
			return "", fmt.Errorf("invalid scenario name %q", scenario)
		}
		if p := filepath.Join(dir, scenario+".md"); fileExists(p) {
			return p, nil
		}
	}
	entries, _ := os.ReadDir(dir)
	var have []string
	for _, e := range entries {
		if s, ok := strings.CutSuffix(e.Name(), ".md"); ok && !e.IsDir() {
			have = append(have, s)
		}
	}
	if scenario != "" {
		if len(have) > 0 {
			return "", fmt.Errorf("no rehearsal fixture %q for %s -- have: %s", scenario, name, strings.Join(have, ", "))
		}
		return "", fmt.Errorf("no rehearsal fixtures for %s -- a named scenario needs %s", name, filepath.Join("rehearsals", name, scenario+".md"))
	}
	return "", nil
}

func fileExists(p string) bool {
	info, err := os.Stat(p)
	return err == nil && !info.IsDir()
}

func routinesNew(args []string) int {
	positional, _, help, err := parseFlags(args, nil)
	if err != nil {
		return fail(err)
	}
	if help {
		fmt.Println("usage: openroutines routines new <name>")
		return 0
	}
	if len(positional) != 1 {
		return fail(fmt.Errorf("usage: openroutines routines new <name>"))
	}
	name, err := routineName(positional[0])
	if err != nil {
		return fail(err)
	}
	path := routinePath(name)
	existing, err := routine.Find(".", name)
	if err == nil {
		return fail(fmt.Errorf("routine %q already exists at %s", name, existing.Path))
	}
	// A file that fails to load still claims its name; only "not found" clears
	// the way to create over it.
	if !errors.Is(err, routine.ErrNotFound) {
		return fail(fmt.Errorf("cannot create %q: %w", name, err))
	}
	if err := os.MkdirAll("routines", 0o755); err != nil {
		return fail(err)
	}
	content := fmt.Sprintf(newRoutineTemplate, name)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		return fail(err)
	}
	fmt.Printf("Created %s (inactive -- edit the schedule and prompt, then set active: true)\n", path)
	return 0
}

// Resolves the file a named routine lives in, for the two commands that act
// on the file rather than on what it declares. A routine that fails to load
// is exactly the one you'd want to edit or remove, so fall back to the file
// its load error names -- unless the name is claimed by two files, where
// picking one would be a guess.
func routineFile(name string) (string, error) {
	r, err := routine.Find(".", name)
	if err == nil {
		return r.Path, nil
	}
	var re *routine.Error
	if errors.As(err, &re) && re.Path != "" {
		return re.Path, nil
	}
	return "", err
}

// Validates a CLI routine argument against the name grammar. Names become
// paths (routines/<name>.md, lock files, ledgers), so this runs before any
// path construction.
func routineName(arg string) (string, error) {
	name := strings.TrimSuffix(arg, ".md")
	if !routine.NamePattern.MatchString(name) {
		return "", fmt.Errorf("routine name %q must be lowercase alphanumerics with hyphens/underscores", name)
	}
	return name, nil
}

func routinePath(name string) string {
	return filepath.Join("routines", name+".md")
}

func routinesEdit(args []string) int {
	positional, _, help, err := parseFlags(args, nil)
	if err != nil {
		return fail(err)
	}
	if help {
		fmt.Println("usage: openroutines routines edit <name>")
		return 0
	}
	if len(positional) != 1 {
		return fail(fmt.Errorf("usage: openroutines routines edit <name>"))
	}
	name, err := routineName(positional[0])
	if err != nil {
		return fail(err)
	}
	path, err := routineFile(name)
	if err != nil {
		return fail(err)
	}
	editor := os.Getenv("VISUAL")
	if editor == "" {
		editor = os.Getenv("EDITOR")
	}
	if editor == "" {
		return fail(fmt.Errorf("set $EDITOR (or $VISUAL) to use routines edit -- or just open %s", path))
	}
	cmd := exec.Command(editor, path)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		return fail(err)
	}
	// A routine you just edited should still be valid.
	if _, err := routine.Parse(path); err != nil {
		fmt.Fprintf(os.Stderr, "warning: %s: %v\n", path, err)
		return 1
	}
	return 0
}

func routinesSetActive(args []string, active bool) int {
	verb := "activate"
	if !active {
		verb = "deactivate"
	}
	positional, _, help, err := parseFlags(args, nil)
	if err != nil {
		return fail(err)
	}
	if help {
		fmt.Printf("usage: openroutines routines %s <name>\n", verb)
		return 0
	}
	if len(positional) != 1 {
		return fail(fmt.Errorf("usage: openroutines routines %s <name>", verb))
	}
	name, err := routineName(positional[0])
	if err != nil {
		return fail(err)
	}
	r, err := routine.Find(".", name)
	if err != nil {
		return fail(err)
	}
	path := r.Path
	if err := routine.SetActive(path, active); err != nil {
		return fail(err)
	}
	fmt.Printf("%sd %s (commit the diff to make it stick in production)\n", verb, path)
	return 0
}

func routinesRemove(args []string) int {
	positional, _, help, err := parseFlags(args, nil)
	if err != nil {
		return fail(err)
	}
	if help {
		fmt.Println("usage: openroutines routines remove <name>")
		return 0
	}
	if len(positional) != 1 {
		return fail(fmt.Errorf("usage: openroutines routines remove <name>"))
	}
	name, err := routineName(positional[0])
	if err != nil {
		return fail(err)
	}
	path, err := routineFile(name)
	if err != nil {
		return fail(err)
	}
	if err := os.Remove(path); err != nil {
		return fail(err)
	}
	fmt.Printf("Removed %s\n", path)
	// Best effort: also clear scheduling state, trigger baseline, and consumer
	// cursor from the knowledge branch, so a later routine with the same name
	// doesn't inherit orphaned state. The ledger stays -- that's knowledge.
	removed, _ := knowledge.NewStore(".").RemoveRoutineState(name)
	for _, p := range removed {
		fmt.Printf("Removed %s (commit inside knowledge/ to record it)\n", p)
	}
	return 0
}

func routinesList() int {
	routines, errs := routine.LoadAgent(".")
	for _, e := range errs {
		fmt.Fprintf(os.Stderr, "warning: %v\n", e)
	}
	if len(routines) == 0 {
		fmt.Println("No routines. Create one: openroutines routines new <name>")
		return 0
	}
	fmt.Printf("%-20s %-16s %-8s %s\n", "NAME", "SCHEDULE", "ACTIVE", "GRANTS")
	for _, r := range routines {
		fmt.Printf("%-20s %-16s %-8v %s\n", r.Name, scheduleSummary(r), r.Frontmatter.IsActive(), strings.Join(grantSummary(r), " "))
	}
	return 0
}

// Names every authority a routine declares -- skills, credentials, MCP
// servers, and web access are all grants.
func grantSummary(r *routine.Routine) []string {
	var grants []string
	if n := len(r.Frontmatter.Skills); n > 0 {
		grants = append(grants, fmt.Sprintf("skills:%d", n))
	}
	if n := len(r.Frontmatter.Credentials); n > 0 {
		grants = append(grants, fmt.Sprintf("creds:%d", n))
	}
	if n := len(r.Frontmatter.MCP); n > 0 {
		grants = append(grants, fmt.Sprintf("mcp:%d", n))
	}
	if r.Frontmatter.Webfetch {
		grants = append(grants, "webfetch")
	}
	if r.Frontmatter.Websearch {
		grants = append(grants, "websearch")
	}
	return grants
}

func scheduleSummary(r *routine.Routine) string {
	switch {
	case r.Frontmatter.Schedule != "" && r.Frontmatter.Trigger != nil:
		return r.Frontmatter.Schedule + " +trigger"
	case r.Frontmatter.Trigger != nil:
		return "trigger"
	default:
		return r.Frontmatter.Schedule
	}
}

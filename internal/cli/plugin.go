package cli

import (
	"bufio"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"

	"golang.org/x/term"

	"github.com/steadyspacecorp/openroutines/internal/config"
	"github.com/steadyspacecorp/openroutines/internal/plugin"
	"github.com/steadyspacecorp/openroutines/internal/routine"
	"github.com/steadyspacecorp/openroutines/internal/skill"
)

func cmdPlugin(args []string) int {
	if len(args) == 0 {
		return fail(fmt.Errorf("usage: openroutines plugin <add|list|update>"))
	}
	switch args[0] {
	case "add":
		return pluginAdd(args[1:])
	case "list":
		return pluginList(args[1:])
	case "update":
		return pluginUpdate(args[1:])
	default:
		return fail(fmt.Errorf("unknown plugin command %q (available: add, list, update)", args[0]))
	}
}

// pluginAdd installs a plugin: clone (or read a local directory), validate
// the whole payload, show the manifest and the grant summary, confirm, copy.
// Everything the bundle asks for is stated before anything lands -- review
// is the only gate (design decision "Plugins").
func pluginAdd(args []string) int {
	var source, subPath string
	yes := false
	rest := args[:0:0]
	for i := 0; i < len(args); i++ {
		if args[i] == "--path" && i+1 < len(args) {
			subPath = args[i+1]
			i++
			continue
		}
		if args[i] == "--yes" {
			yes = true
			continue
		}
		rest = append(rest, args[i])
	}
	if len(rest) != 1 {
		return fail(fmt.Errorf("usage: openroutines plugin add <git-url | owner/repo | local-dir> [--path sub/dir] [--yes]"))
	}
	source = rest[0]

	if _, err := os.Stat(config.Path(".")); err != nil {
		return fail(fmt.Errorf("run plugin add from inside an agent repository"))
	}

	// Local directories and remote sources both go through a temporary clone,
	// so the vendored payload always corresponds to the recorded commit.
	root, provenance, cleanup, err := resolvePluginSource(source, subPath, "")
	if err != nil {
		return fail(err)
	}
	defer cleanup()

	_, existingSkills, err := plugin.AgentNamespace(".")
	if err != nil {
		return fail(err)
	}
	agentSkills := map[string]bool{}
	for _, s := range existingSkills {
		agentSkills[s.Name] = true
	}

	p, err := plugin.Load(root, agentSkills)
	if err != nil {
		return fail(err)
	}
	collisions, err := p.Collisions(".")
	if err != nil {
		return fail(err)
	}
	if len(collisions) > 0 {
		return fail(fmt.Errorf("already present, refusing to replace: %s -- remove them first to reinstall", strings.Join(collisions, ", ")))
	}

	fmt.Printf("Plugin %q -- %s\n\n", p.Manifest.Name, firstLine(p.Manifest.Description))
	if p.Body != "" {
		fmt.Println(indent(p.Body, "  "))
		fmt.Println()
	}
	fmt.Println("This bundle asks for:")
	fmt.Println(indent(strings.TrimRight(p.Summary(), "\n"), "  "))
	fmt.Println()

	interactive := term.IsTerminal(int(os.Stdin.Fd()))
	if !interactive && !yes {
		return fail(fmt.Errorf("stdin is not interactive; review the bundle above, then rerun with --yes to install"))
	}
	if interactive && !yes {
		fmt.Print("Install? [y/N] ")
		line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
		if strings.TrimSpace(strings.ToLower(line)) != "y" {
			fmt.Println("Nothing installed.")
			return 0
		}
	}

	installed, pendingStubs, err := p.Install(".", provenance)
	if err != nil {
		return fail(err)
	}
	for _, f := range installed {
		fmt.Printf("  installed %s\n", f)
	}
	fmt.Println()

	fmt.Println("Next steps:")
	step := 0
	stepf := func(format string, args ...any) {
		step++
		fmt.Printf("  %d. %s\n", step, fmt.Sprintf(format, args...))
	}
	for _, name := range slices.Sorted(maps.Keys(p.Manifest.Credentials)) {
		c := p.Manifest.Credentials[name]
		if c.Type != "" {
			stepf("add to openroutines.yaml:  credentials: { %s: { type: %s, ... } }  (see the type's required fields)", name, c.Type)
		}
		stepf("openroutines credentials set %s  # %s", name, firstLine(c.Description))
	}
	for _, name := range slices.Sorted(maps.Keys(p.Manifest.Variables)) {
		stepf("set the %s variable in openroutines.yaml  # %s", name, firstLine(p.Manifest.Variables[name].Description))
	}
	for _, s := range pendingStubs {
		stepf("seed %s after the memory worktree exists (first run creates it)", s)
	}
	stepf("openroutines check")
	if len(p.Routines) > 0 {
		stepf("review the routine and skill contents, then activate wanted routines with openroutines routines activate <name>")
	}
	stepf("review the diff and commit -- suggested message: \"Install plugin %s (from %s @ %s)\"", p.Manifest.Name, provenance.Repository, shortRevision(provenance.Revision))
	return 0
}

func resolvePluginSource(source, subPath, revision string) (string, plugin.Source, func(), error) {
	cleanup := func() {}
	repository := source
	cloneURL := ""
	repoRoot := ""
	if fi, err := os.Stat(source); err == nil && fi.IsDir() {
		abs, err := filepath.Abs(source)
		if err != nil {
			return "", plugin.Source{}, cleanup, err
		}
		abs, err = filepath.EvalSymlinks(abs)
		if err != nil {
			return "", plugin.Source{}, cleanup, err
		}
		out, err := exec.Command("git", "-C", abs, "rev-parse", "--show-toplevel").Output()
		if err != nil {
			return "", plugin.Source{}, cleanup, fmt.Errorf("local plugin source must be inside a git repository: %w", err)
		}
		repoRoot, err = filepath.EvalSymlinks(strings.TrimSpace(string(out)))
		if err != nil {
			return "", plugin.Source{}, cleanup, err
		}
		cloneURL = repoRoot
		localPath, err := filepath.Rel(repoRoot, abs)
		if err != nil {
			return "", plugin.Source{}, cleanup, err
		}
		if subPath != "" {
			localPath = filepath.Join(localPath, subPath)
		}
		subPath = filepath.ToSlash(localPath)
		repository = repoRoot
	}
	if cloneURL == "" {
		cloneURL = repository
	}
	if !strings.Contains(repository, "://") && !strings.Contains(repository, "@") && strings.Count(repository, "/") == 1 {
		cloneURL = "https://github.com/" + repository + ".git"
		repository = cloneURL
	}
	tmp, err := os.MkdirTemp("", "openroutines-plugin-*")
	if err != nil {
		return "", plugin.Source{}, cleanup, err
	}
	cleanup = func() { _ = os.RemoveAll(tmp) }
	clone := exec.Command("git", "-c", "protocol.ext.allow=never", "clone", "--quiet", "--", cloneURL, tmp)
	clone.Stderr = os.Stderr
	if err := clone.Run(); err != nil {
		cleanup()
		return "", plugin.Source{}, func() {}, fmt.Errorf("clone %s: %w", cloneURL, err)
	}
	if revision != "" {
		checkout := exec.Command("git", "-C", tmp, "checkout", "--quiet", "--detach", revision)
		checkout.Stderr = os.Stderr
		if err := checkout.Run(); err != nil {
			cleanup()
			return "", plugin.Source{}, func() {}, fmt.Errorf("checkout %s: %w", revision, err)
		}
	}
	revBytes, err := exec.Command("git", "-C", tmp, "rev-parse", "HEAD").Output()
	if err != nil {
		cleanup()
		return "", plugin.Source{}, func() {}, err
	}
	root := tmp
	if subPath != "" && subPath != "." {
		root, err = pluginSubdir(tmp, filepath.FromSlash(subPath))
		if err != nil {
			cleanup()
			return "", plugin.Source{}, func() {}, err
		}
	}
	return root, plugin.Source{Repository: repository, Path: subPath, Revision: strings.TrimSpace(string(revBytes))}, cleanup, nil
}

func shortRevision(revision string) string {
	if len(revision) > 12 {
		return revision[:12]
	}
	return revision
}

func pluginList(args []string) int {
	if len(args) != 0 {
		return fail(fmt.Errorf("usage: openroutines plugin list"))
	}
	entries, err := os.ReadDir("plugins")
	if os.IsNotExist(err) || len(entries) == 0 {
		fmt.Println("No plugins installed.")
		return 0
	}
	if err != nil {
		return fail(err)
	}
	fmt.Printf("%-20s %-12s %s\n", "NAME", "REVISION", "SOURCE")
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		source, err := plugin.ReadSource(filepath.Join("plugins", entry.Name()))
		if err != nil {
			fmt.Printf("%-20s %-12s invalid provenance: %v\n", entry.Name(), "-", err)
			continue
		}
		display := source.Repository
		if source.Path != "" && source.Path != "." {
			display += " --path " + source.Path
		}
		fmt.Printf("%-20s %-12s %s\n", entry.Name(), shortRevision(source.Revision), display)
	}
	return 0
}

func pluginUpdate(args []string) int {
	yes := false
	var rest []string
	for _, arg := range args {
		if arg == "--yes" {
			yes = true
		} else {
			rest = append(rest, arg)
		}
	}
	if len(rest) != 1 {
		return fail(fmt.Errorf("usage: openroutines plugin update <name> [--yes]"))
	}
	name := rest[0]
	if !skill.NamePattern.MatchString(name) {
		return fail(fmt.Errorf("invalid plugin name %q", name))
	}
	ours := filepath.Join("plugins", name)
	oldSource, err := plugin.ReadSource(ours)
	if err != nil {
		return fail(fmt.Errorf("plugin %s: %w", name, err))
	}
	base, _, cleanupBase, err := resolvePluginSource(oldSource.Repository, oldSource.Path, oldSource.Revision)
	if err != nil {
		return fail(fmt.Errorf("load recorded revision: %w", err))
	}
	defer cleanupBase()
	theirs, newSource, cleanupTheirs, err := resolvePluginSource(oldSource.Repository, oldSource.Path, "")
	if err != nil {
		return fail(fmt.Errorf("load upstream: %w", err))
	}
	defer cleanupTheirs()
	if newSource.Revision == oldSource.Revision {
		fmt.Printf("Plugin %s is already current at %s.\n", name, shortRevision(oldSource.Revision))
		return 0
	}
	_, existingSkills, err := plugin.AgentNamespace(".")
	if err != nil {
		return fail(err)
	}
	agentSkills := map[string]bool{}
	for _, s := range existingSkills {
		agentSkills[s.Name] = true
	}
	upstream, err := plugin.Load(theirs, agentSkills)
	if err != nil {
		return fail(fmt.Errorf("new upstream revision is invalid: %w", err))
	}
	if upstream.Manifest.Name != name {
		return fail(fmt.Errorf("upstream plugin name changed from %q to %q", name, upstream.Manifest.Name))
	}
	collisions, err := pluginExternalCollisions(name, upstream)
	if err != nil {
		return fail(err)
	}
	if len(collisions) > 0 {
		return fail(fmt.Errorf("updated plugin collides with agent content: %s", strings.Join(collisions, ", ")))
	}
	fmt.Printf("Update plugin %q: %s -> %s\n\n", name, shortRevision(oldSource.Revision), shortRevision(newSource.Revision))
	changes, err := plugin.Changes(base, theirs)
	if err != nil {
		return fail(err)
	}
	fmt.Println("Upstream files:")
	for _, change := range changes {
		fmt.Printf("  %s\n", change)
	}
	fmt.Println()
	fmt.Println("The updated bundle asks for:")
	fmt.Println(indent(strings.TrimRight(upstream.Summary(), "\n"), "  "))
	fmt.Println()
	interactive := term.IsTerminal(int(os.Stdin.Fd()))
	if !interactive && !yes {
		return fail(fmt.Errorf("stdin is not interactive; review the bundle above, then rerun with --yes to update"))
	}
	if interactive && !yes {
		fmt.Print("Update? [y/N] ")
		line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
		if strings.TrimSpace(strings.ToLower(line)) != "y" {
			fmt.Println("Nothing updated.")
			return 0
		}
	}
	merged, err := os.MkdirTemp(".", ".openroutines-plugin-update-*")
	if err != nil {
		return fail(err)
	}
	defer os.RemoveAll(merged)
	conflicts, err := plugin.MergeTrees(base, ours, theirs, merged)
	if err != nil {
		return fail(err)
	}
	basePlugin, err := plugin.Load(base, agentSkills)
	if err != nil {
		return fail(fmt.Errorf("recorded upstream revision is invalid: %w", err))
	}
	baseRoutines := map[string]bool{}
	for _, r := range basePlugin.Routines {
		baseRoutines[r.Name] = true
	}
	for _, r := range upstream.Routines {
		if baseRoutines[r.Name] {
			continue
		}
		path := filepath.Join(merged, "routines", r.Name+".md")
		raw, err := os.ReadFile(path)
		if err != nil {
			return fail(err)
		}
		raw, err = routine.WithActive(raw, false)
		if err != nil {
			return fail(err)
		}
		if err := os.WriteFile(path, raw, 0o644); err != nil {
			return fail(err)
		}
	}
	sourceToWrite := newSource
	if len(conflicts) > 0 {
		sourceToWrite = oldSource
	}
	if err := plugin.WriteSource(merged, sourceToWrite); err != nil {
		return fail(err)
	}
	if len(conflicts) == 0 {
		mergedPlugin, err := plugin.Load(merged, agentSkills)
		if err != nil {
			return fail(fmt.Errorf("merged plugin is invalid: %w", err))
		}
		mergedCollisions, err := pluginExternalCollisions(name, mergedPlugin)
		if err != nil {
			return fail(err)
		}
		if len(mergedCollisions) > 0 {
			return fail(fmt.Errorf("merged plugin collides with agent content: %s", strings.Join(mergedCollisions, ", ")))
		}
	}
	backup := ours + ".update-backup"
	if err := os.Rename(ours, backup); err != nil {
		return fail(err)
	}
	if err := os.Rename(merged, ours); err != nil {
		_ = os.Rename(backup, ours)
		return fail(err)
	}
	if err := os.RemoveAll(backup); err != nil {
		return fail(err)
	}
	if len(conflicts) > 0 {
		return fail(fmt.Errorf("plugin update has conflicts in %s; resolve the markers and rerun update (recorded revision remains %s)", strings.Join(conflicts, ", "), shortRevision(oldSource.Revision)))
	}
	fmt.Printf("Updated plugins/%s to %s. Review the diff, run openroutines check, and commit.\n", name, shortRevision(newSource.Revision))
	return 0
}

func pluginExternalCollisions(name string, candidate *plugin.Plugin) ([]string, error) {
	routines, skills, err := plugin.AgentNamespace(".")
	if err != nil {
		return nil, fmt.Errorf("agent routines and skills must be valid before updating: %w", err)
	}
	ownPrefix := filepath.Clean(filepath.Join("plugins", name)) + string(filepath.Separator)
	routineNames := map[string]bool{}
	for _, r := range routines {
		clean := filepath.Clean(r.Path)
		if clean == filepath.Clean(filepath.Join("plugins", name)) || strings.HasPrefix(clean, ownPrefix) {
			continue
		}
		routineNames[r.Name] = true
	}
	skillNames := map[string]bool{}
	for _, s := range skills {
		clean := filepath.Clean(s.Dir)
		if clean == filepath.Clean(filepath.Join("plugins", name)) || strings.HasPrefix(clean, ownPrefix) {
			continue
		}
		skillNames[s.Name] = true
	}
	var collisions []string
	for _, r := range candidate.Routines {
		if routineNames[r.Name] {
			collisions = append(collisions, "routine "+r.Name)
		}
	}
	for _, s := range candidate.Skills {
		if skillNames[s.Name] {
			collisions = append(collisions, "skill "+s.Name)
		}
	}
	return collisions, nil
}

func pluginSubdir(root, subPath string) (string, error) {
	clean := filepath.Clean(subPath)
	if filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("--path %q escapes the plugin repository", subPath)
	}
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", err
	}
	candidate, err := filepath.EvalSymlinks(filepath.Join(resolvedRoot, clean))
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(resolvedRoot, candidate)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("--path %q escapes the plugin repository", subPath)
	}
	return candidate, nil
}

func indent(s, prefix string) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		if l != "" {
			lines[i] = prefix + l
		}
	}
	return strings.Join(lines, "\n")
}

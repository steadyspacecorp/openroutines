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
	"github.com/steadyspacecorp/openroutines/internal/skill"
)

func cmdPlugin(args []string) int {
	if len(args) == 0 {
		return fail(fmt.Errorf("usage: openroutines plugin add <git-url | owner/repo | local-dir> [--path sub/dir] [--yes]"))
	}
	switch args[0] {
	case "add":
		return pluginAdd(args[1:])
	default:
		return fail(fmt.Errorf("unknown plugin command %q (available: add)", args[0]))
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

	// A local directory installs directly -- the development and fixture
	// path. Anything else is cloned with the same hardening as skills add.
	root := ""
	revision := ""
	if fi, err := os.Stat(source); err == nil && fi.IsDir() {
		root = source
	} else {
		cloneURL := source
		if !strings.Contains(source, "://") && !strings.Contains(source, "@") && strings.Count(source, "/") == 1 {
			cloneURL = "https://github.com/" + source + ".git"
		}
		tmp, err := os.MkdirTemp("", "openroutines-plugin-*")
		if err != nil {
			return fail(err)
		}
		defer os.RemoveAll(tmp)
		clone := exec.Command("git", "-c", "protocol.ext.allow=never", "clone", "--quiet", "--depth", "1", "--", cloneURL, tmp)
		clone.Stderr = os.Stderr
		if err := clone.Run(); err != nil {
			return fail(fmt.Errorf("clone %s: %w", cloneURL, err))
		}
		revBytes, _ := exec.Command("git", "-C", tmp, "rev-parse", "--short", "HEAD").Output()
		revision = strings.TrimSpace(string(revBytes))
		root = tmp
	}
	if subPath != "" {
		selectedRoot, err := pluginSubdir(root, subPath)
		if err != nil {
			return fail(err)
		}
		root = selectedRoot
	}

	agentSkills := map[string]bool{}
	existing, _ := skill.List("skills")
	for _, s := range existing {
		agentSkills[s.Name] = true
	}

	p, err := plugin.Load(root, agentSkills)
	if err != nil {
		return fail(err)
	}
	if collisions := p.Collisions("."); len(collisions) > 0 {
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

	installed, pendingStubs, err := p.Install(".")
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
	provenance := source
	if revision != "" {
		provenance += " @ " + revision
	}
	stepf("review the diff and commit -- suggested message: \"Install plugin %s (from %s)\"", p.Manifest.Name, provenance)
	return 0
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

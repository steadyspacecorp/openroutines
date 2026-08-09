package cli

import (
	"bufio"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"golang.org/x/term"

	"github.com/steadyspacecorp/openroutines/internal/config"
	"github.com/steadyspacecorp/openroutines/internal/plugin"
)

const pluginUsage = "usage: openroutines plugin <add|list|update>"

func cmdPlugin(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, pluginUsage)
		return 2
	}
	if wantsHelp(args[:1]) {
		fmt.Println(pluginUsage)
		return 0
	}
	switch args[0] {
	case "add":
		return pluginAdd(args[1:])
	case "list":
		return pluginList(args[1:])
	case "update":
		return pluginUpdate(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "openroutines: unknown plugin command %q\n\n", args[0])
		fmt.Println(pluginUsage)
		return 2
	}
}

const pluginAddUsage = "usage: openroutines plugin add <git-url | owner/repo | local-dir> [--path sub/dir] [--yes]"

// Installs a plugin: fetch, validate, show the manifest and grant summary,
// confirm, copy. Everything it asks for is stated before anything lands --
// review is the only gate (design decision "Plugins").
func pluginAdd(args []string) int {
	rest, flags, help, err := parseFlags(args, map[string]flagSpec{
		"--path": {value: true},
		"--yes":  {},
	})
	if err != nil {
		return fail(err)
	}
	if help {
		fmt.Println(pluginAddUsage)
		return 0
	}
	if len(rest) != 1 {
		return fail(fmt.Errorf("%s", pluginAddUsage))
	}
	source := rest[0]
	subPath := flags["--path"]
	_, yes := flags["--yes"]

	root, provenance, cleanup, err := plugin.Fetch(source, subPath, "")
	if err != nil {
		return fail(err)
	}
	defer cleanup()

	inst, err := plugin.PrepareInstall(".", root, provenance)
	if err != nil {
		return fail(err)
	}
	p := inst.Plugin

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

	installed, pendingStubs, err := inst.Apply()
	if err != nil {
		return fail(err)
	}
	for _, f := range installed {
		fmt.Printf("  installed %s\n", f)
	}
	fmt.Println()

	// Declared MCP servers: one consent per endpoint, showing the exact
	// bytes. --yes covers the install but never an endpoint definition; a
	// non-interactive run only prints the snippet.
	mcpHandled := map[string]bool{}
	if interactive && !yes && len(p.Manifest.MCP) > 0 {
		oc, err := config.LoadOpenCode(".")
		if err != nil {
			return fail(err)
		}
		defined := oc.MCPServers()
		reader := bufio.NewReader(os.Stdin)
		for _, name := range slices.Sorted(maps.Keys(p.Manifest.MCP)) {
			if slices.Contains(defined, name) {
				fmt.Printf("  mcp server %q is already defined in opencode.json -- left untouched; review that it matches the plugin's declaration\n", name)
				mcpHandled[name] = true
				continue
			}
			m := p.Manifest.MCP[name]
			fmt.Printf("Define mcp server %q in opencode.json? This connects runs that grant it to an external endpoint.\n  %s\n  [y/N] ", name, config.MCPSnippet(name, m.URL, m.Credential))
			line, _ := reader.ReadString('\n')
			if strings.TrimSpace(strings.ToLower(line)) != "y" {
				continue // stays a printed next step
			}
			if err := config.AddMCPServer(".", name, m.URL, m.Credential); err != nil {
				return fail(err)
			}
			fmt.Printf("  wrote mcp server %q to opencode.json\n", name)
			mcpHandled[name] = true
		}
		fmt.Println()
	}

	fmt.Println("Next steps:")
	step := 0
	stepf := func(format string, args ...any) {
		step++
		fmt.Printf("  %d. %s\n", step, fmt.Sprintf(format, args...))
	}
	for _, name := range slices.Sorted(maps.Keys(p.Manifest.Credentials)) {
		c := p.Manifest.Credentials[name]
		if c.Type != "" {
			stepf("add to openroutines.yml:  credentials: { %s: { type: %s, ... } }  (see the type's required fields)", name, c.Type)
		}
		stepf("openroutines credentials set %s  # %s", name, firstLine(c.Description))
	}
	for _, name := range slices.Sorted(maps.Keys(p.Manifest.Variables)) {
		stepf("set the %s variable in openroutines.yml  # %s", name, firstLine(p.Manifest.Variables[name].Description))
	}
	for _, name := range slices.Sorted(maps.Keys(p.Manifest.MCP)) {
		// A plugin declares the server but never authors harness config;
		// only a person puts the endpoint into opencode.json.
		if mcpHandled[name] {
			continue
		}
		stepf("add to opencode.json's mcp block:  %s  # %s", config.MCPSnippet(name, p.Manifest.MCP[name].URL, p.Manifest.MCP[name].Credential), firstLine(p.Manifest.MCP[name].Description))
	}
	for _, s := range pendingStubs {
		stepf("seed %s after the knowledge worktree exists (first run creates it)", s)
	}
	stepf("openroutines check")
	if len(p.Routines) > 0 {
		stepf("review the routine and skill contents, then activate wanted routines with openroutines routines activate <name>")
	}
	stepf("review the diff and commit -- suggested message: \"Install plugin %s (from %s @ %s)\"", p.Manifest.Name, provenance.Repository, shortRevision(provenance.Revision))
	return 0
}

func shortRevision(revision string) string {
	if len(revision) > 12 {
		return revision[:12]
	}
	return revision
}

func pluginList(args []string) int {
	positional, _, help, err := parseFlags(args, nil)
	if err != nil {
		return fail(err)
	}
	if help {
		fmt.Println("usage: openroutines plugin list")
		return 0
	}
	if len(positional) != 0 {
		return fail(fmt.Errorf("usage: openroutines plugin list"))
	}
	entries, err := os.ReadDir(filepath.Join(".openroutines", "plugins"))
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
		source, err := plugin.ReadSource(filepath.Join(".openroutines", "plugins", entry.Name()))
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

const pluginUpdateUsage = "usage: openroutines plugin update <name> [--yes]"

func pluginUpdate(args []string) int {
	rest, flags, help, err := parseFlags(args, map[string]flagSpec{"--yes": {}})
	if err != nil {
		return fail(err)
	}
	if help {
		fmt.Println(pluginUpdateUsage)
		return 0
	}
	if len(rest) != 1 {
		return fail(fmt.Errorf("%s", pluginUpdateUsage))
	}
	name := rest[0]
	_, yes := flags["--yes"]
	upd, err := plugin.PrepareUpdate(".", name)
	if err != nil {
		return fail(err)
	}
	defer upd.Close()
	if upd.Current() {
		fmt.Printf("Plugin %s is already current at %s.\n", name, shortRevision(upd.Old.Revision))
		return 0
	}
	fmt.Printf("Update plugin %q: %s -> %s\n\n", name, shortRevision(upd.Old.Revision), shortRevision(upd.New.Revision))
	fmt.Println("Upstream files:")
	for _, change := range upd.Changes {
		fmt.Printf("  %s\n", change)
	}
	fmt.Println()
	fmt.Println("The updated bundle asks for:")
	fmt.Println(indent(strings.TrimRight(upd.Upstream.Summary(), "\n"), "  "))
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
	conflicts, err := upd.Apply()
	if err != nil {
		return fail(err)
	}
	if len(conflicts) > 0 {
		return fail(fmt.Errorf("plugin update has conflicts in %s; resolve the markers and rerun update (recorded revision remains %s)", strings.Join(conflicts, ", "), shortRevision(upd.Old.Revision)))
	}
	fmt.Printf("Updated .openroutines/plugins/%s to %s. Review the diff, run openroutines check, and commit.\n", name, shortRevision(upd.New.Revision))
	return 0
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

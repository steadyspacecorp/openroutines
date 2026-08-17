package cli

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	openroutines "github.com/steadyspacecorp/openroutines"
	"github.com/steadyspacecorp/openroutines/internal/avatar"
	"github.com/steadyspacecorp/openroutines/internal/creds"
	"github.com/steadyspacecorp/openroutines/internal/repository"
	"github.com/steadyspacecorp/openroutines/internal/version"
)

const templateRoot = "template"

const newUsage = "usage: openroutines new <path>"

func cmdNew(args []string) int {
	positional, _, help, err := parseFlags(args, nil)
	if err != nil {
		return fail(err)
	}
	if help {
		fmt.Println(newUsage)
		return 0
	}
	if len(positional) != 1 {
		return fail(fmt.Errorf("%s", newUsage))
	}
	target := positional[0]
	name := filepath.Base(target)

	if entries, err := os.ReadDir(target); err == nil && len(entries) > 0 {
		return fail(fmt.Errorf("%s already exists and is not empty", target))
	}
	if err := os.MkdirAll(target, 0o755); err != nil {
		return fail(err)
	}

	// Substitutions applied to every templated text file.
	replacer := strings.NewReplacer(
		"{{AGENT_NAME}}", name,
		"{{OPENROUTINES_VERSION}}", version.Version,
	)

	err = fs.WalkDir(openroutines.TemplateFS, templateRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel := strings.TrimPrefix(path, templateRoot)
		rel = strings.TrimPrefix(rel, "/")
		if rel == "" {
			return nil
		}
		dest := filepath.Join(target, rel)
		if d.IsDir() {
			return os.MkdirAll(dest, 0o755)
		}
		raw, err := openroutines.TemplateFS.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(dest, []byte(replacer.Replace(string(raw))), 0o644)
	})
	if err != nil {
		return fail(err)
	}

	// Claude Code reads CLAUDE.md, not AGENTS.md; a symlink keeps one source
	// of truth so update's AGENTS.md rewrites flow through it. Created here
	// (not in template/) because go:embed refuses symlinks.
	if err := os.Symlink("AGENTS.md", filepath.Join(target, "CLAUDE.md")); err != nil {
		return fail(err)
	}

	avatarSVG, avatarPNG, err := avatar.Generate(name)
	if err != nil {
		return fail(err)
	}
	if err := os.WriteFile(filepath.Join(target, "avatar.svg"), avatarSVG, 0o644); err != nil {
		return fail(err)
	}
	if err := os.WriteFile(filepath.Join(target, "avatar.png"), avatarPNG, 0o644); err != nil {
		return fail(err)
	}

	frameworkDir := filepath.Join(target, ".openroutines")
	if err := os.MkdirAll(frameworkDir, 0o755); err != nil {
		return fail(err)
	}
	if err := os.WriteFile(filepath.Join(frameworkDir, "version"), []byte(version.Version+"\n"), 0o644); err != nil {
		return fail(err)
	}
	if err := creds.Initialize(target); err != nil {
		return fail(err)
	}

	if err := repository.Initialize(target); err != nil {
		return fail(err)
	}

	fmt.Printf(`Created agent %q at %s

Next steps:

1. Configure your agent:

   cd %s && openroutines configure

2. Create a repository on GitHub or another Git host, then set its URL in the repo field of openroutines.yml.

3. Run openroutines check, then commit and push your agent to the repository.
`, name, target, target)
	return 0
}

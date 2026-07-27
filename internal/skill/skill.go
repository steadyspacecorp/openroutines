// Package skill reads skills in the Agent Skills format: a directory per
// skill containing SKILL.md with name + description frontmatter.
package skill

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// NamePattern is the Agent Skills name constraint: lowercase alphanumerics
// and hyphens, matching the directory name.
var NamePattern = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

// Skill is one parsed SKILL.md: name, description, directory.
type Skill struct {
	Name        string `yaml:"name"`
	Description string `yaml:"description"`
	Dir         string `yaml:"-"`
}

// Parse reads one SKILL.md's frontmatter.
func Parse(path string) (*Skill, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	text := strings.ReplaceAll(string(raw), "\r\n", "\n")
	if !strings.HasPrefix(text, "---\n") {
		return nil, fmt.Errorf("%s: missing frontmatter", path)
	}
	rest := text[len("---\n"):]
	end := strings.Index(rest, "\n---")
	if end < 0 {
		return nil, fmt.Errorf("%s: unterminated frontmatter", path)
	}
	var s Skill
	if err := yaml.Unmarshal([]byte(rest[:end]), &s); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	s.Dir = filepath.Dir(path)
	return &s, nil
}

// List reads every skill under dir, sorted by name. Missing dir is empty.
func List(dir string) ([]*Skill, []error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, []error{err}
	}
	var skills []*Skill
	var errs []error
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		s, err := Parse(filepath.Join(dir, e.Name(), "SKILL.md"))
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if s.Name != e.Name() {
			errs = append(errs, fmt.Errorf("skills/%s: frontmatter name %q must match the directory", e.Name(), s.Name))
		}
		skills = append(skills, s)
	}
	sort.Slice(skills, func(i, j int) bool { return skills[i].Name < skills[j].Name })
	return skills, errs
}

// ListAgent reads agent-owned skills plus every installed plugin's skills.
// Skill names are global because routine grants name them without a path.
func ListAgent(root string) ([]*Skill, []error) {
	skills, errs := List(filepath.Join(root, "skills"))
	pluginDirs, err := os.ReadDir(filepath.Join(root, "plugins"))
	if err != nil && !os.IsNotExist(err) {
		errs = append(errs, err)
	}
	for _, entry := range pluginDirs {
		if !entry.IsDir() {
			continue
		}
		found, foundErrs := List(filepath.Join(root, "plugins", entry.Name(), "skills"))
		skills = append(skills, found...)
		errs = append(errs, foundErrs...)
	}
	seen := map[string]string{}
	duplicates := map[string]bool{}
	for _, s := range skills {
		if prior, ok := seen[s.Name]; ok {
			errs = append(errs, fmt.Errorf("duplicate skill %q: %s and %s", s.Name, prior, s.Dir))
			duplicates[s.Name] = true
		} else {
			seen[s.Name] = s.Dir
		}
	}
	if len(duplicates) > 0 {
		filtered := skills[:0]
		for _, s := range skills {
			if !duplicates[s.Name] {
				filtered = append(filtered, s)
			}
		}
		skills = filtered
	}
	sort.Slice(skills, func(i, j int) bool { return skills[i].Name < skills[j].Name })
	return skills, errs
}

// Find returns one globally named skill from an agent repository.
func Find(root, name string) (*Skill, error) {
	skills, errs := ListAgent(root)
	for _, err := range errs {
		if strings.Contains(err.Error(), "duplicate skill "+fmt.Sprintf("%q", name)) {
			return nil, err
		}
	}
	for _, s := range skills {
		if s.Name == name {
			return s, nil
		}
	}
	return nil, fmt.Errorf("skill %q not found in skills/ or any installed plugin", name)
}

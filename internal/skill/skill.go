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

// Package routine parses routine markdown files: YAML frontmatter declaring
// the scope (schedule, grants) and a body that is the prompt.
package routine

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/steadyspacecorp/openroutines/internal/trigger"
)

// NamePattern constrains routine names: the filename is the routine's
// identity, and names become filesystem paths (routines/<name>.md, lock
// files, ledgers) -- so the grammar is closed under path construction:
// no separators, no dots, no way to spell an escape.
var NamePattern = regexp.MustCompile(`^[a-z0-9]+([_-][a-z0-9]+)*$`)

// Frontmatter is a routine's declared scope. Every field is optional except
// that at least one of Schedule and Trigger must be set; see design
// decisions "Routines are markdown files" and "Triggers" for defaults.
type Frontmatter struct {
	Schedule    string        `yaml:"schedule"`
	Trigger     *trigger.Spec `yaml:"trigger,omitempty"` // outbound change-detection wake-up
	Timeout     string        `yaml:"timeout,omitempty"`
	Active      *bool         `yaml:"active,omitempty"`
	Skills      []string      `yaml:"skills"`
	Credentials []string      `yaml:"credentials"`
	Model       string        `yaml:"model,omitempty"`
	Effort      string        `yaml:"effort,omitempty"` // provider-specific reasoning effort (opencode --variant)
	Events      *bool         `yaml:"events,omitempty"`
	Consumes    string        `yaml:"consumes,omitempty"` // "memory": this routine consumes the memory change feed
}

// IsActive applies the default: routines are active unless explicitly not.
func (f Frontmatter) IsActive() bool { return f.Active == nil || *f.Active }

// RecordsEvents applies the default: runs record events unless opted out.
func (f Frontmatter) RecordsEvents() bool { return f.Events == nil || *f.Events }

// IsConsumer reports whether the routine declared itself a memory consumer.
func (f Frontmatter) IsConsumer() bool { return f.Consumes == "memory" }

// Routine is one parsed routine file: identity, declared scope, prompt.
type Routine struct {
	Name string // filename without .md -- the human-readable name
	Path string
	FM   Frontmatter
	Body string // the prompt
}

// Parse reads one routine file. The file must begin with a "---" frontmatter
// block; everything after the closing "---" is the prompt body.
func Parse(path string) (*Routine, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	text := strings.ReplaceAll(string(raw), "\r\n", "\n")
	if !strings.HasPrefix(text, "---\n") {
		return nil, fmt.Errorf("%s: missing frontmatter (file must start with ---)", filepath.Base(path))
	}
	rest := text[len("---\n"):]
	end := strings.Index(rest, "\n---\n")
	if end < 0 {
		// Allow a file that is only frontmatter closed by a final "---".
		if strings.HasSuffix(rest, "\n---") {
			end = len(rest) - len("\n---")
		} else {
			return nil, fmt.Errorf("%s: unterminated frontmatter (no closing ---)", filepath.Base(path))
		}
	}
	// Strict decoding: a typo like `actve: false` must be an error, not a
	// silent fall-through to active-by-default.
	dec := yaml.NewDecoder(strings.NewReader(rest[:end]))
	dec.KnownFields(true)
	var fm Frontmatter
	if err := dec.Decode(&fm); err != nil && err != io.EOF {
		return nil, fmt.Errorf("%s: frontmatter: %w", filepath.Base(path), err)
	}
	body := ""
	if bodyStart := end + len("\n---\n"); bodyStart <= len(rest) {
		body = strings.TrimSpace(rest[min(bodyStart, len(rest)):])
	}
	name := strings.TrimSuffix(filepath.Base(path), ".md")
	return &Routine{Name: name, Path: path, FM: fm, Body: body}, nil
}

// SetActive rewrites the `active:` frontmatter field in place, preserving the
// rest of the file byte for byte. Both directions are explicit: activation
// and deactivation should each be a visible diff.
func SetActive(path string, active bool) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	out, err := WithActive(raw, active)
	if err != nil {
		return fmt.Errorf("%s: %w", filepath.Base(path), err)
	}
	return os.WriteFile(path, out, 0o644)
}

// WithActive returns routine markdown with an explicit active field, preserving
// every other byte. Installers use it before a routine becomes visible so an
// active-by-default source can never race a live supervisor.
func WithActive(raw []byte, active bool) ([]byte, error) {
	text := string(raw)
	if !strings.HasPrefix(text, "---\n") {
		return nil, fmt.Errorf("missing frontmatter")
	}
	end := strings.Index(text[4:], "\n---")
	if end < 0 {
		return nil, fmt.Errorf("unterminated frontmatter")
	}
	fmEnd := 4 + end // offset of the newline before the closing ---
	head, tail := text[:fmEnd], text[fmEnd:]

	value := "active: false"
	if active {
		value = "active: true"
	}
	activeLine := regexp.MustCompile(`(?m)^active:[^\n]*$`)
	if activeLine.MatchString(head) {
		head = activeLine.ReplaceAllString(head, value)
	} else {
		head += "\n" + value
	}
	return []byte(head + tail), nil
}

// LoadDir parses every *.md routine in dir, sorted by name. A missing dir is
// an empty agent, not an error.
func LoadDir(dir string) ([]*Routine, []error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, []error{err}
	}
	var routines []*Routine
	var errs []error
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		r, err := Parse(filepath.Join(dir, e.Name()))
		if err != nil {
			errs = append(errs, err)
			continue
		}
		routines = append(routines, r)
	}
	sort.Slice(routines, func(i, j int) bool { return routines[i].Name < routines[j].Name })
	return routines, errs
}

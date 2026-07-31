// Package routine parses routine markdown files: YAML frontmatter declaring
// the scope (schedule, grants) and a body that is the prompt.
package routine

import (
	"errors"
	"fmt"
	"io"
	"net/url"
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

// DefaultURL is the canonical link supplied to routines that do not declare
// one of their own.
const DefaultURL = "https://openroutines.dev"

// Frontmatter is a routine's declared scope. Every field is optional except
// that at least one of Schedule and Trigger must be set; see design
// decisions "Routines are markdown files" and "Triggers" for defaults.
type Frontmatter struct {
	Schedule    string        `yaml:"schedule"`
	Trigger     *trigger.Spec `yaml:"trigger,omitempty"` // outbound change-detection wake-up
	Timeout     string        `yaml:"timeout,omitempty"`
	URL         string        `yaml:"url,omitempty"`
	Active      *bool         `yaml:"active,omitempty"`
	Skills      []string      `yaml:"skills"`
	Credentials []string      `yaml:"credentials"`
	Model       string        `yaml:"model,omitempty"`
	Effort      string        `yaml:"effort,omitempty"` // provider-specific reasoning effort (opencode --variant)
	Events      *bool         `yaml:"events,omitempty"`
	Consumes    string        `yaml:"consumes,omitempty"`  // "memory": this routine consumes the memory change feed
	Webfetch    bool          `yaml:"webfetch,omitempty"`  // grants the webfetch tool; external content is an injection vector, so off by default
	Websearch   bool          `yaml:"websearch,omitempty"` // grants the websearch tool (and enables its search backend)
	MCP         []string      `yaml:"mcp,omitempty"`       // grants a configured MCP server's tools; third-party tool text is an injection vector, so none by default
}

// IsActive applies the default: routines are active unless explicitly not.
func (f Frontmatter) IsActive() bool { return f.Active == nil || *f.Active }

// RecordsEvents applies the default: runs record events unless opted out.
func (f Frontmatter) RecordsEvents() bool { return f.Events == nil || *f.Events }

// IsConsumer reports whether the routine declared itself a memory consumer.
func (f Frontmatter) IsConsumer() bool { return f.Consumes == "memory" }

// EffectiveURL applies the framework default for external records that want
// a canonical link back to the routine's source or project.
func (f Frontmatter) EffectiveURL() string {
	if f.URL != "" {
		return f.URL
	}
	return DefaultURL
}

// Routine is one parsed routine file: identity, declared scope, prompt.
type Routine struct {
	Name string // filename without .md -- the human-readable name
	Path string
	FM   Frontmatter
	Body string // the prompt
}

// Parse reads one routine file. The file must begin with a "---" frontmatter
// block; everything after the closing "---" is the prompt body. Errors name
// the failure, not the file: the caller passed the path and knows how to
// spell it for its reader (Error does, relative to nothing; a plugin
// validator does, relative to the payload).
func Parse(path string) (*Routine, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	text := strings.ReplaceAll(string(raw), "\r\n", "\n")
	if !strings.HasPrefix(text, "---\n") {
		return nil, errors.New("missing frontmatter (file must start with ---)")
	}
	rest := text[len("---\n"):]
	end := strings.Index(rest, "\n---\n")
	if end < 0 {
		// Allow a file that is only frontmatter closed by a final "---".
		if strings.HasSuffix(rest, "\n---") {
			end = len(rest) - len("\n---")
		} else {
			return nil, errors.New("unterminated frontmatter (no closing ---)")
		}
	}
	// Strict decoding: a typo like `actve: false` must be an error, not a
	// silent fall-through to active-by-default.
	dec := yaml.NewDecoder(strings.NewReader(rest[:end]))
	dec.KnownFields(true)
	var fm Frontmatter
	if err := dec.Decode(&fm); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("frontmatter: %w", err)
	}
	if fm.URL != "" {
		u, err := url.Parse(fm.URL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil {
			return nil, fmt.Errorf("frontmatter: url %q must be an absolute http(s) URL without credentials", fm.URL)
		}
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

// Error is a load failure attributed to the routine it concerns: the file
// that would not parse, or the name two files collide on. Attribution is what
// keeps one broken file from being everyone's problem -- a run of a healthy
// routine can tell that the error belongs to someone else. Path names the
// file when the failure is about one, since a name alone does not say which
// of routines/ and plugins/*/routines/ the failure is in -- and two plugins
// shipping the same broken filename would otherwise be indistinguishable.
type Error struct {
	Name string // the routine the failure is about
	Path string // the file it is about; "" when the failure is about two (a collision)
	Err  error
}

func (e *Error) Error() string {
	if e.Path == "" {
		return e.Err.Error()
	}
	return e.Path + ": " + e.Err.Error()
}

func (e *Error) Unwrap() error { return e.Err }

// Concerns reports whether one LoadAgent error stands between the caller and
// routine name. An error attributed to another routine does not; an
// unattributed one (an unreadable plugins directory, which could be hiding
// this very routine) concerns everyone -- fail closed. Pass a single error,
// never an errors.Join: attribution would match whichever error joined first,
// whatever name it is about.
func Concerns(err error, name string) bool {
	if err == nil {
		return false
	}
	var re *Error
	if errors.As(err, &re) {
		return re.Name == name
	}
	return true
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
		path := filepath.Join(dir, e.Name())
		r, err := Parse(path)
		if err != nil {
			// The filename is the identity, so a file that will not parse
			// still says which routine it is about.
			errs = append(errs, &Error{Name: strings.TrimSuffix(e.Name(), ".md"), Path: path, Err: err})
			continue
		}
		routines = append(routines, r)
	}
	sort.Slice(routines, func(i, j int) bool { return routines[i].Name < routines[j].Name })
	return routines, errs
}

// LoadAgent reads agent-owned routines plus every installed plugin's
// routines. Names are global identities, so duplicates are errors.
//
// Every routine it returns is one a caller may run: a name any attributed
// error is about is dropped from the list, not just a name two *parseable*
// files claim. A run whose workspace would be assembled around such a name
// refuses to start (routine.Concerns, in the runner), so a name left in the
// list is a name the tick would schedule, mint, and push before that refusal.
func LoadAgent(root string) ([]*Routine, []error) {
	routines, errs := LoadDir(filepath.Join(root, "routines"))
	pluginDirs, err := os.ReadDir(filepath.Join(root, "plugins"))
	if err != nil && !os.IsNotExist(err) {
		errs = append(errs, err)
	}
	for _, entry := range pluginDirs {
		if !entry.IsDir() {
			continue
		}
		found, foundErrs := LoadDir(filepath.Join(root, "plugins", entry.Name(), "routines"))
		routines = append(routines, found...)
		errs = append(errs, foundErrs...)
	}
	seen := map[string]string{}
	for _, r := range routines {
		if prior, ok := seen[r.Name]; ok {
			errs = append(errs, &Error{Name: r.Name, Err: fmt.Errorf("duplicate routine %q: %s and %s", r.Name, prior, r.Path)})
		} else {
			seen[r.Name] = r.Path
		}
	}
	broken := map[string]bool{}
	for _, err := range errs {
		var re *Error
		if errors.As(err, &re) {
			broken[re.Name] = true
		}
	}
	if len(broken) > 0 {
		filtered := routines[:0]
		for _, r := range routines {
			if !broken[r.Name] {
				filtered = append(filtered, r)
			}
		}
		routines = filtered
	}
	sort.Slice(routines, func(i, j int) bool { return routines[i].Name < routines[j].Name })
	return routines, errs
}

// ErrNotFound is the one Find failure that means the name is free. Every
// other one means the name is spoken for by something the agent could not
// read -- which a caller about to write the file needs to tell apart.
var ErrNotFound = errors.New("no routine")

// Find returns one globally named routine from an agent repository.
func Find(root, name string) (*Routine, error) {
	routines, errs := LoadAgent(root)
	// A routine that failed to load is missing from the list; reporting it as
	// "no routine" would send the reader looking for a file that is right
	// there. Report why it is not loadable instead.
	for _, err := range errs {
		if Concerns(err, name) {
			return nil, err
		}
	}
	for _, r := range routines {
		if r.Name == name {
			return r, nil
		}
	}
	return nil, fmt.Errorf("%w %q", ErrNotFound, name)
}

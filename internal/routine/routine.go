package routine

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/steadyspacecorp/openroutines/internal/frontmatter"
	"github.com/steadyspacecorp/openroutines/internal/trigger"
)

// Names become filesystem paths (routines/<name>.md, locks, ledgers), so
// separators and traversal must be rejected before any path construction.
var NamePattern = regexp.MustCompile(`^[a-z0-9]+([_-][a-z0-9]+)*$`)

const DefaultURL = "https://openroutines.dev"

type Frontmatter struct {
	Schedule    string        `yaml:"schedule"`
	Trigger     *trigger.Spec `yaml:"trigger,omitempty"`
	Timeout     string        `yaml:"timeout,omitempty"`
	URL         string        `yaml:"url,omitempty"`
	Active      *bool         `yaml:"active,omitempty"`
	Skills      []string      `yaml:"skills"`
	Credentials []string      `yaml:"credentials"`
	Model       string        `yaml:"model,omitempty"`
	Effort      string        `yaml:"effort,omitempty"`
	Teamwork    string        `yaml:"teamwork,omitempty"`
	Reports     bool          `yaml:"reports,omitempty"`
	Webfetch    bool          `yaml:"webfetch,omitempty"`
	Websearch   bool          `yaml:"websearch,omitempty"`
	MCP         []string      `yaml:"mcp,omitempty"`

	Events   *bool  `yaml:"events,omitempty"`
	Consumes string `yaml:"consumes,omitempty"`
}

const (
	TeamworkFull   = "full"
	TeamworkEvents = "events"
	TeamworkOff    = "off"
)

func (f Frontmatter) IsActive() bool { return f.Active == nil || *f.Active }

func (f Frontmatter) RecordsEvents() bool { return f.teamwork() != TeamworkOff }

func (f Frontmatter) FullTeamwork() bool { return f.teamwork() == TeamworkFull }

func (f Frontmatter) teamwork() string {
	switch {
	case f.Teamwork != "":
		return f.Teamwork
	case f.Reports:
		return TeamworkOff
	default:
		return TeamworkFull
	}
}

func (f Frontmatter) EffectiveURL() string {
	if f.URL != "" {
		return f.URL
	}
	return DefaultURL
}

type Routine struct {
	Name        string
	Path        string
	Frontmatter Frontmatter
	Body        string
}

func (r *Routine) Log() *slog.Logger {
	return slog.With("routine", r.Name)
}

func Parse(path string) (*Routine, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	doc, err := frontmatter.Split(raw)
	if errors.Is(err, frontmatter.ErrMissing) {
		return nil, errors.New("missing frontmatter (file must start with ---)")
	}
	if errors.Is(err, frontmatter.ErrUnterminated) {
		return nil, errors.New("unterminated frontmatter (no closing ---)")
	}
	if err != nil {
		return nil, err
	}
	dec := yaml.NewDecoder(strings.NewReader(string(doc.Frontmatter)))
	// Strict decoding prevents a typo like `actve: false` from silently
	// falling through to active-by-default.
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
	switch fm.Teamwork {
	case "", TeamworkFull, TeamworkEvents, TeamworkOff:
	default:
		return nil, fmt.Errorf("frontmatter: teamwork %q must be full, events, or off", fm.Teamwork)
	}
	if fm.Events != nil {
		return nil, errors.New(`frontmatter: the events key is retired -- "events: false" is now "teamwork: off"; "events: true" was the default, so delete the line`)
	}
	if fm.Consumes != "" {
		return nil, errors.New(`frontmatter: the consumes key is retired -- "consumes: knowledge" is now "reports: true"`)
	}
	body := strings.TrimSpace(string(doc.Body))
	name := strings.TrimSuffix(filepath.Base(path), ".md")
	return &Routine{Name: name, Path: path, Frontmatter: fm, Body: body}, nil
}

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

func WithActive(raw []byte, active bool) ([]byte, error) {
	doc, err := frontmatter.Split(raw)
	if err != nil {
		return nil, err
	}
	head := string(doc.Frontmatter)

	value := "active: false"
	if active {
		value = "active: true"
	}
	activeLine := regexp.MustCompile(`(?m)^active:[^\n]*$`)
	if activeLine.MatchString(head) {
		head = activeLine.ReplaceAllString(head, value)
	} else {
		head += doc.LineEnding() + value
	}
	return doc.WithFrontmatter([]byte(head)), nil
}

type Error struct {
	Name string
	Path string
	Err  error
}

func (e *Error) Error() string {
	if e.Path == "" {
		return e.Err.Error()
	}
	return e.Path + ": " + e.Err.Error()
}

func (e *Error) Unwrap() error { return e.Err }

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
			errs = append(errs, &Error{Name: strings.TrimSuffix(e.Name(), ".md"), Path: path, Err: err})
			continue
		}
		routines = append(routines, r)
	}
	sort.Slice(routines, func(i, j int) bool { return routines[i].Name < routines[j].Name })
	return routines, errs
}

func LoadPlugins(root string) ([]*Routine, []error) {
	var routines []*Routine
	var errs []error
	pluginDirs, err := os.ReadDir(filepath.Join(root, ".openroutines", "plugins"))
	if err != nil && !os.IsNotExist(err) {
		errs = append(errs, err)
	}
	for _, entry := range pluginDirs {
		if !entry.IsDir() {
			continue
		}
		found, foundErrs := LoadDir(filepath.Join(root, ".openroutines", "plugins", entry.Name(), "routines"))
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
	sort.Slice(routines, func(i, j int) bool { return routines[i].Name < routines[j].Name })
	return routines, errs
}

func LoadAgent(root string) ([]*Routine, []error) {
	routines, errs := LoadDir(filepath.Join(root, "routines"))
	claimed := map[string]bool{}
	for _, r := range routines {
		claimed[r.Name] = true
	}
	for _, err := range errs {
		var re *Error
		if errors.As(err, &re) {
			claimed[re.Name] = true
		}
	}
	pluginRoutines, pluginErrs := LoadPlugins(root)
	for _, r := range pluginRoutines {
		if !claimed[r.Name] {
			routines = append(routines, r)
		}
	}
	for _, err := range pluginErrs {
		var re *Error
		if errors.As(err, &re) && claimed[re.Name] {
			continue
		}
		errs = append(errs, err)
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

var ErrNotFound = errors.New("no routine")

func Find(root, name string) (*Routine, error) {
	routines, errs := LoadAgent(root)
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

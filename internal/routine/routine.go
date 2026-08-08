// Package routine parses routine markdown files: YAML frontmatter declaring
// the scope (schedule, grants) and a body that is the prompt.
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

	"github.com/steadyspacecorp/openroutines/internal/trigger"
)

// NamePattern constrains routine names: the filename is the routine's
// identity, and names become filesystem paths (routines/<name>.md, lock
// files, ledgers), so no separators, no dots, no way to spell an escape.
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
	Effort      string        `yaml:"effort,omitempty"`    // provider-specific reasoning effort (opencode --variant)
	Teamwork    string        `yaml:"teamwork,omitempty"`  // participation ladder: "full" (default), "events", "off"
	Reports     bool          `yaml:"reports,omitempty"`   // this routine reports: it receives changes.md, keeps a cursor, consumes the batch
	Webfetch    bool          `yaml:"webfetch,omitempty"`  // grants the webfetch tool; external content is an injection vector, so off by default
	Websearch   bool          `yaml:"websearch,omitempty"` // grants the websearch tool (and enables its search backend)
	MCP         []string      `yaml:"mcp,omitempty"`       // grants a configured MCP server's tools; third-party tool text is an injection vector, so none by default

	// Events and Consumes are retired, replaced by Teamwork and Reports.
	// Parsed only so Parse can reject them with a migration message instead
	// of a generic "unknown field" error.
	Events   *bool  `yaml:"events,omitempty"`
	Consumes string `yaml:"consumes,omitempty"`
}

// The teamwork ladder: each value names the highest tier of teamwork
// participation, strictly ordered so a routine can't fill the schedule's
// tables without also recording its runs as events.
const (
	TeamworkFull   = "full"   // default: runs recorded as events, fires fill the schedule's tables
	TeamworkEvents = "events" // runs recorded as events; fires appear as fact lines only
	TeamworkOff    = "off"    // invisible to the team: checking in is not work
)

// IsActive applies the default: routines are active unless explicitly not.
func (f Frontmatter) IsActive() bool { return f.Active == nil || *f.Active }

// RecordsEvents reports whether runs land in the shared record: every
// teamwork tier except off.
func (f Frontmatter) RecordsEvents() bool { return f.teamwork() != TeamworkOff }

// FullTeamwork reports whether the routine participates fully in teamwork:
// its runs are recorded and its scheduled fires fill the schedule's tables
// rather than its fact lines.
func (f Frontmatter) FullTeamwork() bool { return f.teamwork() == TeamworkFull }

// Resolves the ladder's default: off for a reporting routine
// (reporting is not work), full otherwise; an explicit value overrides.
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

// Log returns the process logger with this routine's identity bound, so an
// operator can filter one routine's output from concurrent runs sharing stdout.
func (r *Routine) Log() *slog.Logger {
	return slog.With("routine", r.Name)
}

// Parse reads one routine file. The file must begin with a "---" frontmatter
// block; everything after the closing "---" is the prompt body. Errors name
// the failure, not the file -- the caller already has the path and knows how
// to attribute it for its own reader.
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
	body := ""
	if bodyStart := end + len("\n---\n"); bodyStart <= len(rest) {
		body = strings.TrimSpace(rest[min(bodyStart, len(rest)):])
	}
	name := strings.TrimSuffix(filepath.Base(path), ".md")
	return &Routine{Name: name, Path: path, FM: fm, Body: body}, nil
}

// SetActive rewrites the `active:` frontmatter field in place, preserving the
// rest of the file byte for byte -- both directions are explicit so each is
// a visible diff.
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

// WithActive returns routine markdown with an explicit active field,
// preserving every other byte. Installers use it before a routine becomes
// visible so an active-by-default source can never race a live supervisor.
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
// that would not parse, or the name two files collide on. Attribution keeps
// one broken file from being everyone's problem -- a healthy routine's run
// can tell the error belongs to someone else.
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
// unattributed one (e.g. an unreadable plugins directory, which could be
// hiding this routine) concerns everyone -- fail closed. Pass a single
// error, never an errors.Join, which would match whichever joined error came
// first regardless of the name it's about.
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
			errs = append(errs, &Error{Name: strings.TrimSuffix(e.Name(), ".md"), Path: path, Err: err})
			continue
		}
		routines = append(routines, r)
	}
	sort.Slice(routines, func(i, j int) bool { return routines[i].Name < routines[j].Name })
	return routines, errs
}

// LoadPlugins reads every installed plugin's routines. Names are global
// identities across plugins, so duplicates are errors. The returned list
// includes every parseable claim, including duplicates: LoadAgent applies
// precedence and drops broken identities, while plugin installation uses the
// unfiltered claims to catch two plugins sharing a name even when an
// agent-owned routine shadows both.
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

// LoadAgent reads agent-owned routines plus every installed plugin's
// routines. An agent-owned filename shadows the same filename from plugins;
// duplicate names across plugins remain errors. Every routine it returns is
// one a caller may run: a name any attributed error is about is dropped, and
// an invalid agent-owned file still claims its name so a plugin routine
// can't silently take over for it.
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

// ErrNotFound is the one Find failure that means the name is free. Every
// other error means the name is spoken for by something the agent couldn't
// read.
var ErrNotFound = errors.New("no routine")

// Find returns one globally named routine from an agent repository.
func Find(root, name string) (*Routine, error) {
	routines, errs := LoadAgent(root)
	// A routine that failed to load is missing from the list; report why
	// instead of "no routine", which would send the reader looking for a
	// file that's right there.
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

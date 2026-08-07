// Package config loads and validates openroutines.yml -- the agent's identity:
// name, job description, owner, timezone, and routine defaults.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/steadyspacecorp/openroutines/internal/creds"
	"github.com/steadyspacecorp/openroutines/internal/knowledge"
)

// FileName is the agent configuration file at the repository root --
// named for the system that reads it, as opencode.json is named for the
// harness, and spelled .yml like .openroutines/credentials.yml.enc (#50).
const FileName = "openroutines.yml"

// LegacyFileNames are earlier spellings, newest first. Still read, and
// Save writes back to whichever file the agent actually has, so pinned
// agents migrate on their own schedule; check nudges the rename.
var LegacyFileNames = []string{"openroutines.yaml", "agent.yaml"}

// Path returns the configuration file dir actually has: FileName when
// present, the first legacy name that exists otherwise, FileName as the
// fallback (the name a fresh write should create).
func Path(dir string) string {
	preferred := filepath.Join(dir, FileName)
	if _, err := os.Stat(preferred); err == nil {
		return preferred
	}
	for _, name := range LegacyFileNames {
		legacy := filepath.Join(dir, name)
		if _, err := os.Stat(legacy); err == nil {
			return legacy
		}
	}
	return preferred
}

// Owner identifies the human accountable for the agent.
type Owner struct {
	Name  string `yaml:"name"`
	Email string `yaml:"email"`
}

// Defaults are agent-wide fallbacks that routine frontmatter overrides.
type Defaults struct {
	Model   string `yaml:"model"`
	Timeout string `yaml:"timeout"`
}

// DefaultMaxTimeout is the run-length ceiling when max_timeout is not set:
// long enough for real work, short enough that a runaway run cannot burn
// tokens for a day before anyone notices.
const DefaultMaxTimeout = 6 * time.Hour

// ScaffoldConcurrency is the run-slot count the scaffold template writes
// into new agents' openroutines.yml. Deliberately not a fallback the code
// applies to agents that never wrote the key: every concurrent run is a
// container plus live model spend, and an upgrade must not silently double
// either -- an existing agent stays serial until its operator opts in.
const ScaffoldConcurrency = 2

// Knowledge holds knowledge-behavior settings; see design decision
// "Knowledge records events, tasks, and context" for the retention
// window semantics.
type Knowledge struct {
	Retention string `yaml:"retention,omitempty"`
}

// Agent is the parsed configuration file. Description is the agent's job description.
// Variables are non-secret configuration values, injected into every run's
// environment (product_repo -> PRODUCT_REPO); secrets belong in credentials.
// Credentials is optional per-credential metadata: an entry gives a stored
// credential a derived type (see design decision "Credentials have types"); a
// credential without an entry is raw, injected verbatim.
type Agent struct {
	Name        string                `yaml:"name"`
	Description string                `yaml:"description"`
	Owner       Owner                 `yaml:"owner"`
	Timezone    string                `yaml:"timezone"`
	Defaults    Defaults              `yaml:"defaults"`
	MaxTimeout  string                `yaml:"max_timeout,omitempty"`
	Concurrency int                   `yaml:"concurrency,omitempty"`
	LogLevel    string                `yaml:"log_level,omitempty"`
	Knowledge   *Knowledge            `yaml:"knowledge,omitempty"`
	Variables   map[string]string     `yaml:"variables,omitempty"`
	Credentials map[string]creds.Spec `yaml:"credentials,omitempty"`

	// Memory is retired, replaced by Knowledge. It is parsed only so Load
	// can reject it with the rename: strict decoding alone would call it
	// an unknown field, which reads as a typo rather than the rename.
	Memory *Knowledge `yaml:"memory,omitempty"`
}

// MaxRunTimeout is the agent-wide ceiling on a single attempt's effective
// timeout: max_timeout in the configuration file, DefaultMaxTimeout when
// unset. An unparseable value falls back to the default -- Problems reports
// it; execution must not fail open to unlimited.
func (a *Agent) MaxRunTimeout() time.Duration {
	if d, err := time.ParseDuration(a.MaxTimeout); err == nil && d > 0 {
		return d
	}
	return DefaultMaxTimeout
}

// RunSlots is how many routines may execute at once: concurrency in the
// configuration file. Unset and 0 both mean serial -- parallelism is an
// opt-in an agent writes down, and the scaffold template opts new agents in
// at ScaffoldConcurrency. Problems flags a negative value; New refuses to
// boot on any problem, so the fallback here is for surfaces that read a
// broken config anyway (status, check).
func (a *Agent) RunSlots() int {
	if a.Concurrency >= 1 {
		return a.Concurrency
	}
	return 1
}

// MaxConcurrency bounds the reserved production UID pool. Each concurrent
// attempt needs a distinct identity; a finite ceiling keeps that security
// boundary explicit in the image and configuration.
const MaxConcurrency = 32

// Retention returns the configured knowledge retention string ("" = default).
func (a *Agent) Retention() string {
	if a.Knowledge == nil {
		return ""
	}
	return a.Knowledge.Retention
}

// Load reads the configuration file from dir (FileName, or a LegacyFileNames
// fallback). Decoding is strict: a misspelled key is an error, not
// silently ignored configuration.
func Load(dir string) (*Agent, error) {
	path := Path(dir)
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	var a Agent
	if err := dec.Decode(&a); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("%s: %w", filepath.Base(path), err)
	}
	if a.Memory != nil {
		return nil, fmt.Errorf(`%s: the memory key is retired -- memory is now knowledge ("memory.retention" is "knowledge.retention")`, filepath.Base(path))
	}
	return &a, nil
}

// Save writes the configuration back to the file dir actually has -- a
// legacy-named agent keeps its name until its operator renames it.
// Two-space indentation, matching the scaffold template and routine
// frontmatter: this is hand-edited, reviewed config, and a tool that
// reformats it on every run undercuts that (#65) -- yaml.v3's default
// is 4.
func (a *Agent) Save(dir string) error {
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(a); err != nil {
		return err
	}
	if err := enc.Close(); err != nil {
		return err
	}
	return os.WriteFile(Path(dir), buf.Bytes(), 0o644)
}

// isPlaceholder reports whether a value is still a {{SCAFFOLD}} placeholder.
func isPlaceholder(s string) bool {
	return strings.Contains(s, "{{")
}

// Problems returns human-readable validation failures, empty when valid.
func (a *Agent) Problems() []string {
	var out []string
	if a.Name == "" || isPlaceholder(a.Name) {
		out = append(out, "name is not set")
	}
	if a.Description == "" || isPlaceholder(a.Description) {
		out = append(out, "description (the job description) is not set")
	}
	if a.Owner.Email == "" || isPlaceholder(a.Owner.Email) {
		out = append(out, "owner email is not set")
	}
	if a.Timezone == "" || isPlaceholder(a.Timezone) {
		out = append(out, "timezone is not set")
	} else if _, err := time.LoadLocation(a.Timezone); err != nil {
		out = append(out, fmt.Sprintf("timezone %q is not a valid IANA timezone", a.Timezone))
	}
	if a.Defaults.Model == "" || isPlaceholder(a.Defaults.Model) {
		out = append(out, "defaults.model is not set")
	} else if !strings.Contains(a.Defaults.Model, "/") {
		out = append(out, fmt.Sprintf("defaults.model %q must be provider/model", a.Defaults.Model))
	}
	if a.Defaults.Timeout != "" {
		if _, err := time.ParseDuration(a.Defaults.Timeout); err != nil {
			out = append(out, fmt.Sprintf("defaults.timeout %q is not a valid duration", a.Defaults.Timeout))
		}
	}
	if a.MaxTimeout != "" {
		if d, err := time.ParseDuration(a.MaxTimeout); err != nil || d <= 0 {
			out = append(out, fmt.Sprintf("max_timeout %q is not a valid duration", a.MaxTimeout))
		}
	}
	if a.Concurrency < 0 {
		out = append(out, fmt.Sprintf("concurrency %d must be at least 1", a.Concurrency))
	} else if a.Concurrency > MaxConcurrency {
		out = append(out, fmt.Sprintf("concurrency %d exceeds the maximum of %d", a.Concurrency, MaxConcurrency))
	}
	if _, err := knowledge.ParseRetention(a.Retention()); err != nil {
		out = append(out, fmt.Sprintf("knowledge.retention: %v", err))
	}
	if a.LogLevel != "" {
		if _, err := ParseLogLevel(a.LogLevel); err != nil {
			out = append(out, err.Error())
		}
	}
	for _, name := range slices.Sorted(maps.Keys(a.Variables)) {
		switch {
		case !creds.NamePattern.MatchString(name):
			out = append(out, fmt.Sprintf("variable name %q must be lowercase snake_case", name))
		case strings.HasPrefix(name, creds.ReservedPrefix):
			out = append(out, fmt.Sprintf("variable name %q collides with the reserved %s_* prefix", name, strings.ToUpper(creds.ReservedPrefix)))
		case creds.ReservedEnvName(name):
			out = append(out, fmt.Sprintf("variable name %q collides with the %s environment variable", name, strings.ToUpper(name)))
		}
	}
	for _, name := range slices.Sorted(maps.Keys(a.Credentials)) {
		if !creds.NamePattern.MatchString(name) {
			out = append(out, fmt.Sprintf("credential entry %q must name a stored credential in lowercase snake_case", name))
			continue
		}
		out = append(out, creds.SpecProblems(name, a.Credentials[name])...)
	}
	return out
}

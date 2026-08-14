// Loads and validates openroutines.yml -- the agent's identity:
// name, standing instructions, owner, timezone, and routine defaults.
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
)

// The agent configuration file at the repository root --
// named for the system that reads it, as opencode.json is named for the
// harness, and spelled .yml like .openroutines/credentials.yml.enc (#50).
const FileName = "openroutines.yml"

// Earlier spellings, newest first. Still read, and
// Save writes back to whichever file the agent actually has, so pinned
// agents migrate on their own schedule; check nudges the rename.
var LegacyFileNames = []string{"openroutines.yaml", "agent.yaml"}

// Returns the configuration file dir actually has, falling back to
// the first legacy name found, then FileName.
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

type Owner struct {
	Name  string `yaml:"name"`
	Email string `yaml:"email"`
}

type Defaults struct {
	Model   string `yaml:"model"`
	Timeout string `yaml:"timeout"`
}

// The run-length ceiling when max_timeout is not
// set: long enough for real work, short enough that a runaway run cannot
// burn tokens for a day before anyone notices.
const DefaultMaxTimeout = 6 * time.Hour

// The run-slot count the scaffold template writes
// into new agents' openroutines.yml. Deliberately not a fallback the code
// applies to agents that never wrote the key: every concurrent run is a
// container plus live model spend, and an upgrade must not silently
// double either -- an existing agent stays serial until its operator
// opts in.
const ScaffoldConcurrency = 2

// Holds knowledge-behavior settings; see design decision
// "Knowledge records events, tasks, and context" for the retention
// window semantics.
type Knowledge struct {
	Retention string `yaml:"retention,omitempty"`
}

// The parsed configuration file. Instructions is the agent's optional
// standing prompt -- injected into every run before the routine's own
// instructions; absent means the runs get only the routine bodies.
// Variables are non-secret values injected into every run's
// environment (product_repo -> PRODUCT_REPO); secrets belong in credentials.
// Credentials is optional per-credential metadata giving a stored credential
// a derived type (design decision "Credentials have types"); an entry-less
// credential is raw, injected verbatim.
type Agent struct {
	Name         string                `yaml:"name"`
	Instructions string                `yaml:"instructions,omitempty"`
	Repo         string                `yaml:"repo"`
	Owner        Owner                 `yaml:"owner"`
	Timezone     string                `yaml:"timezone"`
	Defaults     Defaults              `yaml:"defaults"`
	MaxTimeout   string                `yaml:"max_timeout,omitempty"`
	Concurrency  int                   `yaml:"concurrency,omitempty"`
	Knowledge    *Knowledge            `yaml:"knowledge,omitempty"`
	Variables    map[string]string     `yaml:"variables,omitempty"`
	Credentials  map[string]creds.Spec `yaml:"credentials,omitempty"`

	// Retired, replaced by Knowledge. Parsed only so Load can reject it
	// with the rename: strict decoding alone would call it an unknown
	// field, which reads as a typo rather than the rename.
	Memory *Knowledge `yaml:"memory,omitempty"`
}

// The agent-wide ceiling on a single attempt's effective
// timeout. An unparseable max_timeout falls back to DefaultMaxTimeout
// rather than failing open to unlimited; Problems reports the bad value
// separately.
func (a *Agent) MaxRunTimeout() time.Duration {
	if d, err := time.ParseDuration(a.MaxTimeout); err == nil && d > 0 {
		return d
	}
	return DefaultMaxTimeout
}

// How many routines may execute at once. Unset and 0 both
// mean serial -- parallelism is an opt-in an agent writes down (the
// scaffold template opts new agents in at ScaffoldConcurrency).
func (a *Agent) RunSlots() int {
	if a.Concurrency >= 1 {
		return a.Concurrency
	}
	return 1
}

// Ceiling on run slots. Nothing structural requires one -- each attempt
// builds its own sandbox -- but dozens of model processes in one container
// is a configuration mistake worth catching at load.
const MaxConcurrency = 32

func (a *Agent) Retention() string {
	if a.Knowledge == nil {
		return ""
	}
	return a.Knowledge.Retention
}

// Reads the configuration file from dir (FileName, or a
// LegacyFileNames fallback). Decoding is strict: a misspelled key is an
// error, not silently ignored configuration.
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

// Writes the configuration back to the file dir actually has -- a
// legacy-named agent keeps its name until its operator renames it. Uses
// two-space indentation to match the scaffold template (yaml.v3 defaults
// to 4, which would reformat this hand-edited file on every write, #65).
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

func isPlaceholder(s string) bool {
	return strings.Contains(s, "{{")
}

// Returns human-readable validation failures, empty when valid.
func (a *Agent) Problems() []string {
	var out []string
	if a.Name == "" || isPlaceholder(a.Name) {
		out = append(out, "name is not set")
	}
	if isPlaceholder(a.Instructions) {
		out = append(out, "instructions still holds a scaffold placeholder -- write the standing instructions or remove the key")
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
	if _, err := ParseRetention(a.Retention()); err != nil {
		out = append(out, fmt.Sprintf("knowledge.retention: %v", err))
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

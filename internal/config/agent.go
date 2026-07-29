// Package config loads and validates openroutines.yml -- the agent's identity:
// name, job description, owner, timezone, and routine defaults.
package config

import (
	"bytes"
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
	"github.com/steadyspacecorp/openroutines/internal/memory"
)

// FileName is the agent configuration file at the repository root --
// named for the system that reads it, as opencode.json is named for the
// harness, and spelled .yml like credentials.yml.enc beside it (#50).
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

// Memory holds memory-behavior settings; see design decision "Memory has three
// shared primitives" for the retention window semantics.
type Memory struct {
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
	Memory      *Memory               `yaml:"memory,omitempty"`
	Variables   map[string]string     `yaml:"variables,omitempty"`
	Credentials map[string]creds.Spec `yaml:"credentials,omitempty"`
}

// Retention returns the configured memory retention string ("" = default).
func (a *Agent) Retention() string {
	if a.Memory == nil {
		return ""
	}
	return a.Memory.Retention
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
	if err := dec.Decode(&a); err != nil && err != io.EOF {
		return nil, fmt.Errorf("%s: %w", filepath.Base(path), err)
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
	if _, err := memory.ParseRetention(a.Retention()); err != nil {
		out = append(out, fmt.Sprintf("memory.retention: %v", err))
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

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

const FileName = "openroutines.yml"

var LegacyFileNames = []string{"openroutines.yaml", "agent.yaml"}

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

const DefaultMaxTimeout = 6 * time.Hour

const ScaffoldConcurrency = 2

type Knowledge struct {
	Retention string `yaml:"retention,omitempty"`
}

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

func (a *Agent) MaxRunTimeout() time.Duration {
	if d, err := time.ParseDuration(a.MaxTimeout); err == nil && d > 0 {
		return d
	}
	return DefaultMaxTimeout
}

func (a *Agent) RunSlots() int {
	if a.Concurrency >= 1 {
		return a.Concurrency
	}
	return 1
}

const MaxConcurrency = 32

func (a *Agent) Retention() string {
	if a.Knowledge == nil {
		return ""
	}
	return a.Knowledge.Retention
}

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

func (a *Agent) Save(dir string) error {
	path := Path(dir)
	var existing yaml.Node
	if raw, err := os.ReadFile(path); err == nil {
		if err := yaml.Unmarshal(raw, &existing); err != nil {
			return err
		}
	}

	var next yaml.Node
	if err := next.Encode(a); err != nil {
		return err
	}
	if len(existing.Content) > 0 {
		from := &existing
		if from.Kind == yaml.DocumentNode {
			from = from.Content[0]
		}
		to := &next
		if to.Kind == yaml.DocumentNode {
			to = to.Content[0]
		}
		copyYAMLComments(from, to)
	}

	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(&next); err != nil {
		return err
	}
	if err := enc.Close(); err != nil {
		return err
	}
	return os.WriteFile(path, buf.Bytes(), 0o644)
}

func copyYAMLComments(from, to *yaml.Node) {
	to.HeadComment = from.HeadComment
	to.LineComment = from.LineComment
	to.FootComment = from.FootComment
	if from.Kind == yaml.DocumentNode && to.Kind == yaml.DocumentNode && len(from.Content) > 0 && len(to.Content) > 0 {
		copyYAMLComments(from.Content[0], to.Content[0])
		return
	}
	if from.Kind != yaml.MappingNode || to.Kind != yaml.MappingNode {
		return
	}
	old := make(map[string][2]*yaml.Node, len(from.Content)/2)
	for i := 0; i+1 < len(from.Content); i += 2 {
		old[from.Content[i].Value] = [2]*yaml.Node{from.Content[i], from.Content[i+1]}
	}
	for i := 0; i+1 < len(to.Content); i += 2 {
		pair, ok := old[to.Content[i].Value]
		if !ok {
			continue
		}
		copyYAMLComments(pair[0], to.Content[i])
		copyYAMLComments(pair[1], to.Content[i+1])
	}
}

func isPlaceholder(s string) bool {
	return strings.Contains(s, "{{")
}

func (a *Agent) Problems() []string {
	var out []string
	if a.Name == "" || isPlaceholder(a.Name) {
		out = append(out, "name is not set")
	}
	if isPlaceholder(a.Instructions) {
		out = append(out, "instructions still holds a scaffold placeholder -- write the standing instructions or remove the key")
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

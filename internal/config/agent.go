// Package config loads and validates agent.yaml -- the agent's identity:
// name, job description, owner, timezone, and routine defaults.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/steadyspacecorp/openroutines/internal/memory"
)

const FileName = "agent.yaml"

type Owner struct {
	Name  string `yaml:"name"`
	Email string `yaml:"email"`
}

type Defaults struct {
	Model   string `yaml:"model"`
	Timeout string `yaml:"timeout"`
}

// Memory holds memory-behavior settings; see DESIGN.md "Memory has three
// shared primitives" for the retention window semantics.
type Memory struct {
	Retention string `yaml:"retention,omitempty"`
}

// Agent is the parsed agent.yaml. Description is the agent's job description.
type Agent struct {
	Name        string   `yaml:"name"`
	Description string   `yaml:"description"`
	Owner       Owner    `yaml:"owner"`
	Timezone    string   `yaml:"timezone"`
	Defaults    Defaults `yaml:"defaults"`
	Memory      *Memory  `yaml:"memory,omitempty"`
}

// Retention returns the configured memory retention string ("" = default).
func (a *Agent) Retention() string {
	if a.Memory == nil {
		return ""
	}
	return a.Memory.Retention
}

// Load reads agent.yaml from dir.
func Load(dir string) (*Agent, error) {
	raw, err := os.ReadFile(filepath.Join(dir, FileName))
	if err != nil {
		return nil, err
	}
	var a Agent
	if err := yaml.Unmarshal(raw, &a); err != nil {
		return nil, fmt.Errorf("%s: %w", FileName, err)
	}
	return &a, nil
}

// Save writes agent.yaml to dir.
func (a *Agent) Save(dir string) error {
	out, err := yaml.Marshal(a)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, FileName), out, 0o644)
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
	return out
}

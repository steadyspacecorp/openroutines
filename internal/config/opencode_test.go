package config

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func writeOpenCode(t *testing.T, dir, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, OpenCodeFileName), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestLoadOpenCode(t *testing.T) {
	dir := t.TempDir()
	oc, err := LoadOpenCode(dir)
	if err != nil {
		t.Fatalf("a missing opencode.json is an agent without harness config, got %v", err)
	}
	if got := oc.MCPServers(); got != nil {
		t.Fatalf("no opencode.json should mean no servers, got %v", got)
	}

	writeOpenCode(t, dir, `{"permission": {}}`)
	if oc, err = LoadOpenCode(dir); err != nil {
		t.Fatal(err)
	}
	if got := oc.MCPServers(); got != nil {
		t.Fatalf("no mcp block should mean no servers, got %v", got)
	}

	writeOpenCode(t, dir, `{"mcp": {"steady": {"type": "remote"}, "acme": {"type": "remote"}}}`)
	if oc, err = LoadOpenCode(dir); err != nil {
		t.Fatal(err)
	}
	if got := oc.MCPServers(); !slices.Equal(got, []string{"acme", "steady"}) {
		t.Fatalf("expected sorted server names, got %v", got)
	}

	writeOpenCode(t, dir, `{`)
	oc, err = LoadOpenCode(dir)
	if err == nil || !strings.Contains(err.Error(), OpenCodeFileName) {
		t.Fatalf("corrupt opencode.json must be a named error, got %v", err)
	}
	if oc == nil || oc.MCPServers() != nil {
		t.Fatalf("the errored view must be empty and usable, got %+v", oc)
	}
}

func TestOpenCodeDrift(t *testing.T) {
	dir := t.TempDir()
	writeOpenCode(t, dir, `{
		"$schema": "https://opencode.ai/config.json",
		"permission": {},
		"mcp": {"steady": {"type": "remote"}},
		"model": "anthropic/claude-sonnet-5",
		"agent": {"title": {"disable": true}, "build": {"model": "anthropic/claude-sonnet-5"}},
		"provider": {"anthropic": {}, "openai": {}}
	}`)
	oc, err := LoadOpenCode(dir)
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(oc.Drift([]string{"anthropic"}), "\n")
	for _, want := range []string{
		`opencode.json contains "model"`,
		`agent "build" in opencode.json sets a model`,
		`provider "openai" in opencode.json is not referenced`,
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("drift missing %q:\n%s", want, got)
		}
	}
	for _, quiet := range []string{`"title"`, `provider "anthropic"`, `"$schema"`, `"permission"`, `"mcp"`} {
		if strings.Contains(got, quiet) {
			t.Fatalf("drift wrongly flags %s:\n%s", quiet, got)
		}
	}
}

func TestAddMCPServer(t *testing.T) {
	dir := t.TempDir()

	if err := AddMCPServer(dir, "steady", "https://example.test/mcp", ""); err == nil {
		t.Fatal("no opencode.json should refuse, not create harness config")
	}

	path := filepath.Join(dir, OpenCodeFileName)
	if err := os.WriteFile(path, []byte(`{"permission": {"question": "deny"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := AddMCPServer(dir, "steady", "https://example.test/mcp", "steady_token"); err != nil {
		t.Fatal(err)
	}
	oc, err := LoadOpenCode(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := oc.MCPServers(); !slices.Equal(got, []string{"steady"}) {
		t.Fatalf("server not inserted: %v", got)
	}
	raw, _ := os.ReadFile(path)
	for _, want := range []string{
		`"question": "deny"`,
		`"type": "remote"`,
		`"url": "https://example.test/mcp"`,
		`"Authorization": "Bearer {env:STEADY_TOKEN}"`,
	} {
		if !strings.Contains(string(raw), want) {
			t.Fatalf("rewritten opencode.json missing %q:\n%s", want, raw)
		}
	}

	if err := AddMCPServer(dir, "steady", "https://evil.test", ""); err == nil || !strings.Contains(err.Error(), "already defined") {
		t.Fatalf("existing entry must be refused, got %v", err)
	}
	if raw, _ := os.ReadFile(path); strings.Contains(string(raw), "evil.test") {
		t.Fatal("refused insert must not touch the file")
	}
}

func TestMCPSnippetMatchesWhatLands(t *testing.T) {
	snippet := MCPSnippet("steady", "https://example.test/mcp", "steady_token")
	for _, want := range []string{`"steady":`, `"type":"remote"`, `"url":"https://example.test/mcp"`, `"Authorization":"Bearer {env:STEADY_TOKEN}"`} {
		if !strings.Contains(snippet, want) {
			t.Fatalf("snippet missing %s:\n%s", want, snippet)
		}
	}
	if snippet := MCPSnippet("open", "https://example.test/open", ""); strings.Contains(snippet, "headers") {
		t.Fatalf("credential-less snippet must carry no auth header:\n%s", snippet)
	}
}

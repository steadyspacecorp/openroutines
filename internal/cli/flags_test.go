package cli

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steadyspacecorp/openroutines/internal/config"
	"github.com/steadyspacecorp/openroutines/internal/creds"
)

func TestUnknownFlagIsRejected(t *testing.T) {
	dir := statusAgent(t, nil)
	for _, tc := range []struct {
		name string
		run  func([]string) int
	}{
		{"status", cmdStatus},
		{"check", cmdCheck},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Chdir(dir)
			if code := tc.run([]string{"--bogus"}); code == 0 {
				t.Fatalf("%s --bogus: expected a nonzero exit, got 0", tc.name)
			}
		})
	}
}

func TestHelpFlagShowsUsageWithoutRunning(t *testing.T) {
	dir := statusAgent(t, nil)
	t.Chdir(dir)

	out := capture(t, dir, func() { cmdCheck([]string{"--help"}) })
	if strings.Contains(out, "check passed") || strings.Contains(out, "check failed") {
		t.Fatalf("check --help must not run the check:\n%s", out)
	}
	if !strings.Contains(out, "usage") {
		t.Fatalf("check --help should print usage:\n%s", out)
	}
}

func TestConfigureRefusesNonInteractiveWithoutYes(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "openroutines.yml"), []byte(statusAgentYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := creds.Initialize(dir); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(filepath.Join(dir, config.FileName))
	if err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)

	stdin := os.Stdin
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	w.Close()
	os.Stdin = r
	defer func() { os.Stdin = stdin }()

	if code := cmdConfigure(nil); code == 0 {
		t.Fatalf("configure on non-interactive stdin without --yes should fail, got exit 0")
	}
	after, err := os.ReadFile(filepath.Join(dir, config.FileName))
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatal("configure wrote configuration after stdin ended")
	}
}

func TestConfigureRequiresTheCredentialStore(t *testing.T) {
	dir := statusAgent(t, nil)
	before, err := os.ReadFile(filepath.Join(dir, config.FileName))
	if err != nil {
		t.Fatal(err)
	}
	code, out := captureCredentialsError(t, dir, func() int { return cmdConfigure([]string{"--yes"}) })
	if code == 0 || !strings.Contains(out, creds.FileName+" is missing") {
		t.Fatalf("configure without store: code=%d, output=%s", code, out)
	}
	for _, path := range []string{creds.KeyFileName, creds.FileName} {
		if _, err := os.Stat(filepath.Join(dir, path)); !os.IsNotExist(err) {
			t.Fatalf("configure created %s: %v", path, err)
		}
	}
	after, err := os.ReadFile(filepath.Join(dir, config.FileName))
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatal("configure changed configuration before rejecting the missing store")
	}
}

func TestConfigurePromptsForTheDefaultProviderCredential(t *testing.T) {
	dir := statusAgent(t, nil)
	if err := creds.Initialize(dir); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)

	var out string
	withStdin(t, "\n\n\n\n\nprovider-secret\n", func() {
		out = capture(t, dir, func() {
			if code := cmdConfigure(nil); code != 0 {
				t.Fatalf("configure exited %d", code)
			}
		})
	})
	if !strings.Contains(out, "fake API key (hidden; enter to skip): ") {
		t.Fatalf("configure did not prompt for the provider credential:\n%s", out)
	}
	_, store, err := creds.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := store["fake_api_key"]; got != "provider-secret" {
		t.Fatalf("stored provider key = %q", got)
	}
}

func TestConfigureLeavesExistingCredentialStoreAlone(t *testing.T) {
	dir := statusAgent(t, nil)
	key := []byte(strings.Repeat("a", 32))
	if err := creds.Write(dir, key, map[string]string{"fake_api_key": "secret"}); err != nil {
		t.Fatal(err)
	}
	storePath := filepath.Join(dir, creds.FileName)
	beforeStore, err := os.ReadFile(storePath)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(creds.EnvMasterKey, "")
	t.Setenv(creds.EnvMasterKeyFile, "")
	beforeConfig, err := os.ReadFile(filepath.Join(dir, config.FileName))
	if err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	stderr := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stderr = w
	code := cmdConfigure([]string{"--yes"})
	os.Stderr = stderr
	w.Close()
	out, err := os.ReadFile(filepath.Join(dir, config.FileName))
	if err != nil {
		t.Fatal(err)
	}
	if code == 0 {
		t.Fatal("configure accepted a locked credential store")
	}
	message, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, creds.KeyFileName)); !os.IsNotExist(err) {
		t.Fatalf("configure created a master key: %v", err)
	}
	afterStore, _ := os.ReadFile(storePath)
	if string(afterStore) != string(beforeStore) {
		t.Fatal("configure changed the credential store")
	}
	if string(out) != string(beforeConfig) {
		t.Fatal("configure changed the configuration before validating credentials")
	}
	if !strings.Contains(string(message), "restore "+creds.KeyFileName) {
		t.Fatalf("configure did not explain how to unlock the store:\n%s", message)
	}
}

func TestConfigureDoesNotChooseAModel(t *testing.T) {
	dir := t.TempDir()
	raw := "name: agent\nrepo:\nowner:\n  name:\n  email: owner@example.com\ntimezone: UTC\ndefaults:\n  model: '{{DEFAULT_MODEL}}'\n  timeout: 5m\n"
	if err := os.WriteFile(filepath.Join(dir, config.FileName), []byte(raw), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := creds.Initialize(dir); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)

	stdin := os.Stdin
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	w.Close()
	os.Stdin = r
	defer func() { os.Stdin = stdin }()

	out := capture(t, dir, func() {
		if code := cmdConfigure([]string{"--yes"}); code != 0 {
			t.Fatalf("configure exited %d", code)
		}
	})
	saved, err := os.ReadFile(filepath.Join(dir, config.FileName))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(saved), "anthropic/") {
		t.Fatalf("configure chose a model:\n%s", saved)
	}
	for _, want := range []string{"Owner name (optional)", "Owner email (optional)", "browse https://models.dev", "defaults.model is not set"} {
		if !strings.Contains(out, want) {
			t.Fatalf("configure output missing %q:\n%s", want, out)
		}
	}
}

package creds

import (
	"bytes"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadRefusesExistingStoreWithoutItsKey(t *testing.T) {
	dir := t.TempDir()
	key := []byte(strings.Repeat("a", 32))
	if err := Write(dir, key, map[string]string{"token": "secret"}); err != nil {
		t.Fatal(err)
	}

	_, _, err := Load(dir)
	if err == nil {
		t.Fatal("Load accepted a store without its key")
	}
	for _, want := range []string{FileName + " exists", "restore " + KeyFileName} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("Load error missing %q: %v", want, err)
		}
	}
	if strings.Contains(err.Error(), "create "+KeyFileName) {
		t.Fatalf("Load suggested creating an incompatible key: %v", err)
	}
}

func TestLoadRequiresEncryptedStore(t *testing.T) {
	for _, withKey := range []bool{false, true} {
		dir := t.TempDir()
		if withKey {
			if err := os.WriteFile(filepath.Join(dir, KeyFileName), []byte(GenerateKey()), 0o600); err != nil {
				t.Fatal(err)
			}
		}

		_, _, err := Load(dir)
		if err == nil || !strings.Contains(err.Error(), FileName+" is missing") {
			t.Fatalf("Load without store (key=%v) error = %v", withKey, err)
		}
		if _, err := os.Stat(filepath.Join(dir, FileName)); !os.IsNotExist(err) {
			t.Fatalf("Load created the missing store: %v", err)
		}
	}
}

func TestInitializeNeverReplacesAnExistingMasterKey(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, KeyFileName)
	if err := os.WriteFile(path, []byte("existing-key\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := Initialize(dir); !errors.Is(err, os.ErrExist) {
		t.Fatalf("Initialize error = %v, want os.ErrExist", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != "existing-key\n" {
		t.Fatalf("Initialize replaced the existing key with %q", raw)
	}
}

func TestInitializeNeverReplacesAnExistingStore(t *testing.T) {
	dir := t.TempDir()
	key := []byte(strings.Repeat("a", 32))
	if err := Write(dir, key, map[string]string{"token": "secret"}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, FileName)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	if err := Initialize(dir); err == nil || !strings.Contains(err.Error(), FileName+" already exists") {
		t.Fatalf("Initialize error = %v", err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, before) {
		t.Fatal("Initialize replaced the existing store")
	}
	if _, err := os.Stat(filepath.Join(dir, KeyFileName)); !os.IsNotExist(err) {
		t.Fatalf("Initialize created a replacement key: %v", err)
	}
}

func TestMasterKeyPrecedence(t *testing.T) {
	dir := t.TempDir()
	defaultKey := GenerateKey()
	fileKey := GenerateKey()
	valueKey := GenerateKey()
	file := filepath.Join(t.TempDir(), "mounted-master.key")
	if err := os.WriteFile(filepath.Join(dir, KeyFileName), []byte(defaultKey), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte(fileKey), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(EnvMasterKeyFile, file)
	t.Setenv(EnvMasterKey, valueKey)

	assertLoadedKey(t, dir, valueKey)
	t.Setenv(EnvMasterKey, "")
	assertLoadedKey(t, dir, fileKey)
	t.Setenv(EnvMasterKeyFile, "")
	assertLoadedKey(t, dir, defaultKey)
}

func TestConventionalMasterKeyRequiresPrivateModeInProduction(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(EnvMasterKey, "")
	t.Setenv(EnvMasterKeyFile, "")
	t.Setenv("OPENROUTINES_IN_CONTAINER", "1")
	if err := os.WriteFile(filepath.Join(dir, KeyFileName), []byte(GenerateKey()), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := LoadKey(dir); err == nil {
		t.Fatal("a conventional master key readable by group and other users was accepted in production")
	}
}

func assertLoadedKey(t *testing.T, dir, wantHex string) {
	t.Helper()
	got, err := LoadKey(dir)
	if err != nil {
		t.Fatal(err)
	}
	want, err := hex.DecodeString(wantHex)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("loaded key = %x, want %x", got, want)
	}
}

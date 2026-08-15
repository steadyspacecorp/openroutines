package creds

import (
	"bytes"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
)

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

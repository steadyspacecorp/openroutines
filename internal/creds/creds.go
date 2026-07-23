// Package creds implements the Rails-style encrypted credentials store:
// one AES-256-GCM encrypted YAML file committed to the repo, one master key
// kept out of it (master.key locally, OPENROUTINES_MASTER_KEY in production).
package creds

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

const (
	KeyFileName      = "master.key"
	FileName         = "credentials.yml.enc"
	EnvMasterKey     = "OPENROUTINES_MASTER_KEY"
	EnvMasterKeyFile = "OPENROUTINES_MASTER_KEY_FILE"
	header           = "ORV1:" // versioned format header
)

// NamePattern constrains credential names: lowercase snake_case, so the
// env-var mapping (slack_webhook -> SLACK_WEBHOOK) is always well-formed.
var NamePattern = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

// ReservedPrefix is never a valid credential name prefix: OPENROUTINES_* env
// vars are framework metadata, never secrets.
const ReservedPrefix = "openroutines"

// GenerateKey mints a 32-byte master key, hex-encoded.
func GenerateKey() string {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		panic(err) // crypto/rand failure is not recoverable
	}
	return hex.EncodeToString(buf)
}

// LoadKey resolves the master key, in order: the OPENROUTINES_MASTER_KEY_FILE
// path (preferred in production -- a file never appears in /proc/pid/environ),
// the OPENROUTINES_MASTER_KEY environment variable, then the gitignored
// master.key file (local).
func LoadKey(dir string) ([]byte, error) {
	keyHex := ""
	if path := os.Getenv(EnvMasterKeyFile); path != "" {
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", EnvMasterKeyFile, err)
		}
		keyHex = strings.TrimSpace(string(raw))
	}
	if keyHex == "" {
		keyHex = strings.TrimSpace(os.Getenv(EnvMasterKey))
	}
	if keyHex == "" {
		raw, err := os.ReadFile(filepath.Join(dir, KeyFileName))
		if err != nil {
			return nil, fmt.Errorf("no master key: set %s or create %s (openroutines configure generates one)", EnvMasterKey, KeyFileName)
		}
		keyHex = strings.TrimSpace(string(raw))
	}
	key, err := hex.DecodeString(keyHex)
	if err != nil || len(key) != 32 {
		return nil, fmt.Errorf("master key must be 64 hex characters (32 bytes)")
	}
	return key, nil
}

func seal(key, plaintext []byte) (string, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	sealed := gcm.Seal(nonce, nonce, plaintext, nil)
	return header + base64.StdEncoding.EncodeToString(sealed), nil
}

func open(key []byte, encoded string) ([]byte, error) {
	if !strings.HasPrefix(encoded, header) {
		return nil, fmt.Errorf("unrecognized credentials file format")
	}
	sealed, err := base64.StdEncoding.DecodeString(strings.TrimSpace(encoded[len(header):]))
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	if len(sealed) < gcm.NonceSize() {
		return nil, fmt.Errorf("credentials file truncated")
	}
	plaintext, err := gcm.Open(nil, sealed[:gcm.NonceSize()], sealed[gcm.NonceSize():], nil)
	if err != nil {
		return nil, fmt.Errorf("cannot decrypt credentials: wrong master key or corrupted file")
	}
	return plaintext, nil
}

// Read decrypts and parses the credentials file. A missing file is an empty
// store, not an error.
func Read(dir string, key []byte) (map[string]string, error) {
	raw, err := os.ReadFile(filepath.Join(dir, FileName))
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]string{}, nil
		}
		return nil, err
	}
	plaintext, err := open(key, string(raw))
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	if err := yaml.Unmarshal(plaintext, &out); err != nil {
		return nil, fmt.Errorf("credentials content: %w", err)
	}
	return out, nil
}

// Write encrypts and writes the credentials map.
func Write(dir string, key []byte, values map[string]string) error {
	plaintext, err := yaml.Marshal(values)
	if err != nil {
		return err
	}
	encoded, err := seal(key, plaintext)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, FileName), []byte(encoded+"\n"), 0o644)
}

// ProviderKeyName returns the reserved credential name for a model provider,
// e.g. "anthropic" -> "anthropic_api_key". These are auto-injected per the
// routine's declared model provider.
func ProviderKeyName(provider string) string {
	return provider + "_api_key"
}

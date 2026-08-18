package creds

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/steadyspacecorp/openroutines/internal/mode"
	"github.com/steadyspacecorp/openroutines/internal/scrub"
)

const (
	KeyFileName      = "master.key"
	FileName         = ".openroutines/credentials.yml.enc"
	EnvMasterKey     = "OPENROUTINES_MASTER_KEY"
	EnvMasterKeyFile = "OPENROUTINES_MASTER_KEY_FILE"
	header           = "ORV1:"
)

var errNoMasterKey = errors.New("no master key")

var NamePattern = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

const ReservedPrefix = "openroutines"

func ReservedEnvName(name string) bool {
	switch name {
	case "tz", "path", "home", "tmpdir":
		return true
	}
	return strings.HasPrefix(name, "ld_") || strings.HasPrefix(name, "xdg_")
}

func generateKey() []byte {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		panic(err)
	}
	return buf
}

func GenerateKey() string {
	return hex.EncodeToString(generateKey())
}

func Initialize(dir string) error {
	if _, err := os.Stat(filepath.Join(dir, FileName)); err == nil {
		return fmt.Errorf("%s already exists", FileName)
	} else if !os.IsNotExist(err) {
		return err
	}
	key := generateKey()
	keyPath := filepath.Join(dir, KeyFileName)
	file, err := os.OpenFile(keyPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := file.WriteString(hex.EncodeToString(key) + "\n"); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return Write(dir, key, map[string]string{})
}

func Absent(dir string) bool {
	if _, err := os.Stat(filepath.Join(dir, FileName)); !os.IsNotExist(err) {
		return false
	}
	return strings.TrimSpace(os.Getenv(EnvMasterKey)) == "" && MasterKeyFilePath(dir) == ""
}

func Load(dir string) ([]byte, map[string]string, error) {
	_, err := os.Stat(filepath.Join(dir, FileName))
	if os.IsNotExist(err) {
		return nil, nil, fmt.Errorf("%s is missing", FileName)
	}
	if err != nil {
		return nil, nil, err
	}

	key, err := LoadKey(dir)
	if err != nil {
		return nil, nil, unavailableKey(err)
	}
	values, err := Read(dir, key)
	if err != nil {
		return nil, nil, unavailableKey(err)
	}
	return key, values, nil
}

func unavailableKey(err error) error {
	message := fmt.Sprintf("%s exists but no usable master key is available -- restore %s, set %s, or point %s at the original key", FileName, KeyFileName, EnvMasterKey, EnvMasterKeyFile)
	if errors.Is(err, errNoMasterKey) {
		return errors.New(message)
	}
	return fmt.Errorf("%s: %w", message, err)
}

func MasterKeyFilePath(dir string) string {
	if strings.TrimSpace(os.Getenv(EnvMasterKey)) != "" {
		return ""
	}
	if path := os.Getenv(EnvMasterKeyFile); path != "" {
		return path
	}
	path := filepath.Join(dir, KeyFileName)
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return ""
	}
	return path
}

func LoadKey(dir string) ([]byte, error) {
	keyHex := strings.TrimSpace(os.Getenv(EnvMasterKey))
	if keyHex == "" {
		path := MasterKeyFilePath(dir)
		if path == "" {
			return nil, fmt.Errorf("%w: set %s or %s, or create %s", errNoMasterKey, EnvMasterKey, EnvMasterKeyFile, KeyFileName)
		}
		if mode.Current() == mode.DeployedContainer {
			info, err := os.Stat(path)
			if err != nil {
				return nil, fmt.Errorf("master key file %s: %w", path, err)
			}
			if info.Mode().Perm()&0o077 != 0 {
				return nil, fmt.Errorf("master key file %s must not be readable by group or other users in production (mode %04o)", path, info.Mode().Perm())
			}
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("master key file %s: %w", path, err)
		}
		keyHex = strings.TrimSpace(string(raw))
	}
	key, err := hex.DecodeString(keyHex)
	if err != nil || len(key) != 32 {
		return nil, fmt.Errorf("master key must be 64 hex characters (32 bytes)")
	}
	// The key just entered the process; from here on any output that quotes
	// it is redacted, wherever it leaks from.
	scrub.Register(map[string]string{"master_key": hex.EncodeToString(key)})
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
	// Prefixed so an agent credential name can never collide with the master
	// or deploy key scrub entries.
	values := make(map[string]string, len(out))
	for name, v := range out {
		values["credential "+name] = v
	}
	scrub.Register(values)
	return out, nil
}

func Write(dir string, key []byte, values map[string]string) error {
	plaintext, err := yaml.Marshal(values)
	if err != nil {
		return err
	}
	encoded, err := seal(key, plaintext)
	if err != nil {
		return err
	}
	path := filepath.Join(dir, FileName)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(encoded+"\n"), 0o644)
}

func ProviderKeyName(provider string) string {
	return provider + "_api_key"
}

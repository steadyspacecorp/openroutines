package cli

import (
	"os"
	"path/filepath"
	"strings"
)

func versionPinPath(dir string) string {
	return filepath.Join(dir, ".openroutines", "version")
}

func readVersionPin(dir string) (string, error) {
	raw, err := os.ReadFile(versionPinPath(dir))
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(raw)), nil
}

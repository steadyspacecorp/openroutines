package openroutines

import (
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestInstallerReleaseFallback(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("installer supports macOS and Linux")
	}
	arch := runtime.GOARCH
	if arch != "amd64" && arch != "arm64" {
		t.Skip("installer supports amd64 and arm64")
	}
	version := "v9.9.9-test"
	binary := fmt.Sprintf("openroutines_%s_%s_%s", version, runtime.GOOS, arch)
	releases := t.TempDir()
	payload := []byte("installer fixture\n")
	if err := os.WriteFile(filepath.Join(releases, binary), payload, 0o644); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(payload)
	checksums := fmt.Sprintf("%x  %s\n", sum, binary)
	if err := os.WriteFile(filepath.Join(releases, "checksums.txt"), []byte(checksums), 0o644); err != nil {
		t.Fatal(err)
	}

	bin := t.TempDir()
	curl := `#!/bin/sh
destination=
url=
while [ "$#" -gt 0 ]; do
  case "$1" in
    -o) destination="$2"; shift 2 ;;
    -w) shift 2 ;;
    -*) shift ;;
    *) url="$1"; shift ;;
  esac
done
printf '%s\n' "$url" >> "$TEST_CURL_LOG"
case "$url" in
  https://get.openroutines.dev/*) status="$TEST_MIRROR_STATUS" ;;
  https://github.com/*) status="$TEST_GITHUB_STATUS" ;;
  *) status=500 ;;
esac
if [ "$status" = 200 ]; then cp "$TEST_RELEASE_DIR/${url##*/}" "$destination"; fi
printf '%s' "$status"
`
	if err := os.WriteFile(filepath.Join(bin, "curl"), []byte(curl), 0o755); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name, mirror, github string
		wantOK               bool
		want                 string
	}{
		{"mirror hit", "200", "500", true, "get.openroutines.dev"},
		{"GitHub fallback", "404", "200", true, "github.com/steadyspacecorp/openroutines/releases/download"},
		{"both unavailable", "404", "500", false, "no longer mirrored"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			installDir := t.TempDir()
			logPath := filepath.Join(t.TempDir(), "curl.log")
			cmd := exec.Command("bash", "www/install.sh")
			cmd.Env = append(os.Environ(),
				"PATH="+bin+":/usr/bin:/bin",
				"OPENROUTINES_VERSION="+version,
				"OPENROUTINES_INSTALL_DIR="+installDir,
				"TEST_RELEASE_DIR="+releases,
				"TEST_CURL_LOG="+logPath,
				"TEST_MIRROR_STATUS="+tc.mirror,
				"TEST_GITHUB_STATUS="+tc.github,
			)
			out, err := cmd.CombinedOutput()
			if tc.wantOK && err != nil {
				t.Fatalf("installer failed: %v\n%s", err, out)
			}
			if !tc.wantOK && err == nil {
				t.Fatalf("installer succeeded unexpectedly:\n%s", out)
			}
			log, _ := os.ReadFile(logPath)
			combined := string(out) + string(log)
			if !strings.Contains(combined, tc.want) {
				t.Fatalf("installer output/log missing %q:\n%s", tc.want, combined)
			}
			if tc.wantOK {
				installed, err := os.ReadFile(filepath.Join(installDir, "openroutines"))
				if err != nil || string(installed) != string(payload) {
					t.Fatalf("installed binary = %q, %v", installed, err)
				}
			}
		})
	}
}

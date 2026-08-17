package runner

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestColdRuntimeImageBuildReportsProgress(t *testing.T) {
	for _, tc := range []struct {
		name, buildExit, want string
	}{
		{"success", "0", "local runtime image ready"},
		{"failure", "1", "local runtime image build failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bin := t.TempDir()
			docker := "#!/bin/sh\nif [ \"$1\" = image ]; then exit 1; fi\nexit " + tc.buildExit + "\n"
			if err := os.WriteFile(filepath.Join(bin, "docker"), []byte(docker), 0o755); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

			stderr := os.Stderr
			r, w, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			os.Stderr = w
			buildErr := ensureRuntimeImage(t.TempDir(), "openroutines-runtime:test")
			os.Stderr = stderr
			w.Close()
			out, err := io.ReadAll(r)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(out), "first build downloads") || !strings.Contains(string(out), tc.want) {
				t.Fatalf("cold-build output missing progress:\n%s", out)
			}
			if (buildErr == nil) != (tc.buildExit == "0") {
				t.Fatalf("build error = %v", buildErr)
			}
		})
	}
}

func TestWarmRuntimeImageBuildDoesNotPrintColdNotice(t *testing.T) {
	bin := t.TempDir()
	docker := "#!/bin/sh\nexit 0\n"
	if err := os.WriteFile(filepath.Join(bin, "docker"), []byte(docker), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	stderr := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stderr = w
	buildErr := ensureRuntimeImage(t.TempDir(), "openroutines-runtime:test")
	os.Stderr = stderr
	w.Close()
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	if buildErr != nil || len(out) != 0 {
		t.Fatalf("warm build error/output = %v, %q", buildErr, out)
	}
}

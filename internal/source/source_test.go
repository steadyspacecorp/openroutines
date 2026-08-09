package source

import (
	"context"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestFetchLocalRepository(t *testing.T) {
	repo := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repo, "skills", "demo"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "skills", "demo", "SKILL.md"), []byte("demo"), 0o644); err != nil {
		t.Fatal(err)
	}
	git := func(args ...string) string {
		t.Helper()
		args = append([]string{"-C", repo, "-c", "user.name=t", "-c", "user.email=t@t"}, args...)
		out, err := exec.Command("git", args...).CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	git("init", "--quiet")
	git("add", "-A")
	git("commit", "--quiet", "-m", "init")
	revision := git("rev-parse", "HEAD")

	root, provenance, cleanup, err := Fetch(filepath.Join(repo, "skills", "demo"), "", "")
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if _, err := os.Stat(filepath.Join(root, "SKILL.md")); err != nil {
		t.Fatal(err)
	}
	if provenance.Path != "skills/demo" || provenance.Revision != revision {
		t.Fatalf("provenance = %#v", provenance)
	}
	if _, _, _, err := Fetch(repo, "", "missing-revision"); err == nil {
		t.Fatal("Fetch accepted a missing revision")
	}
}

func TestResolvePathRejectsEscapes(t *testing.T) {
	root := t.TempDir()
	if _, err := ResolvePath(root, "../outside"); err == nil {
		t.Fatal("ResolvePath accepted parent traversal")
	}
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, "linked")); err == nil {
		if _, err := ResolvePath(root, "linked"); err == nil {
			t.Fatal("ResolvePath accepted a symlink escape")
		}
	}
}

func TestGitOutputStopsTransportChildrenAtDeadline(t *testing.T) {
	bin := t.TempDir()
	childPID := filepath.Join(t.TempDir(), "child.pid")
	git := filepath.Join(bin, "git")
	script := "#!/bin/sh\nexec \"$SOURCE_GIT_HELPER\" -test.run=TestSourceGitHelperProcess\n"
	if err := os.WriteFile(git, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("SOURCE_GIT_HELPER", os.Args[0])
	t.Setenv("SOURCE_GIT_CHILD_PID", childPID)
	t.Setenv("GO_WANT_SOURCE_GIT_HELPER", "1")
	t.Cleanup(func() {
		raw, err := os.ReadFile(childPID)
		if err != nil {
			return
		}
		pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
		if err != nil {
			return
		}
		if process, err := os.FindProcess(pid); err == nil {
			_ = process.Kill()
		}
	})

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := gitOutput(ctx, "status")
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("gitOutput succeeded after its deadline")
		}
	case <-time.After(7 * time.Second):
		t.Fatal("gitOutput waited on a transport child after its deadline")
	}
}

func TestSourceGitHelperProcess(_ *testing.T) {
	if os.Getenv("GO_WANT_SOURCE_GIT_HELPER") != "1" {
		return
	}
	if os.Getenv("SOURCE_GIT_HELPER_CHILD") == "1" {
		signal.Ignore(syscall.SIGHUP, syscall.SIGTERM)
		if err := os.WriteFile(os.Getenv("SOURCE_GIT_CHILD_PID"), []byte(strconv.Itoa(os.Getpid())), 0o644); err != nil {
			os.Exit(2)
		}
		time.Sleep(30 * time.Second)
		os.Exit(0)
	}
	cmd := exec.Command(os.Args[0], "-test.run=TestSourceGitHelperProcess")
	cmd.Env = append(os.Environ(), "SOURCE_GIT_HELPER_CHILD=1")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		os.Exit(2)
	}
	for {
		if _, err := os.Stat(os.Getenv("SOURCE_GIT_CHILD_PID")); err == nil {
			break
		}
		time.Sleep(time.Millisecond)
	}
	_ = cmd.Wait()
	os.Exit(0)
}

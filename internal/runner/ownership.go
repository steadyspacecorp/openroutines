package runner

import (
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"

	"github.com/steadyspacecorp/openroutines/internal/sandbox"
)

// The attempt identity's filesystem access is granted on the group axis,
// never by changing ownership: the supervisor holds no CAP_CHOWN, but it
// owns everything it stages and belongs to every attempt group (template
// Dockerfile), and chgrp to a group the owner belongs to is unprivileged.
// Each attempt's primary gid equals its uid, so group bits reach exactly one
// attempt identity, the supervisor keeps owner access for import and
// cleanup, and other attempts have no bits at all.

// Makes the staged workspace readable by exactly one attempt identity:
// every path joins the attempt's group with read-only group bits and zero
// world bits.
func prepareWorkspaceAccess(gid uint32, workspace string) error {
	return filepath.WalkDir(workspace, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := os.Chown(path, -1, int(gid)); err != nil {
			return err
		}
		if entry.IsDir() {
			return os.Chmod(path, 0o750)
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		mode := fs.FileMode(0o640)
		if info.Mode()&0o111 != 0 {
			mode = 0o750
		}
		return os.Chmod(path, mode)
	})
}

// Removes a tree an attempt identity may have written into. The supervisor
// holds no CAP_DAC_OVERRIDE, and a process running as the attempt can leave
// paths the group axis can't cover (explicit modes like opencode's 0644, a
// chmod 0700) -- so a denied removal is retried after the attempt identity
// itself reopens its own paths. uid 0 means no attempt identity was
// involved, so removal must succeed outright.
func removeAttemptTree(uid uint32, path string) error {
	err := os.RemoveAll(path)
	if err == nil || uid == 0 {
		return err
	}
	reclaimErr := reclaimAttemptTrees(uid, path)
	if retryErr := os.RemoveAll(path); retryErr != nil {
		return errors.Join(reclaimErr, retryErr)
	}
	return nil
}

// Spawns the capless helper as the attempt identity to restore group bits
// on paths that identity owns. removeAttemptTree retries removal either way
// and includes this error if the retry also fails.
func reclaimAttemptTrees(uid uint32, root string) error {
	cmd := exec.Command(sandbox.HelperPath, "sandbox-reclaim", root)
	cmd.Env = []string{"PATH=" + os.Getenv("PATH")}
	cmd.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: uid, Gid: uid}}
	return cmd.Run()
}

// Makes the trees the attempt may mutate -- staged knowledge, the run tmp,
// the attempt home -- group-writable for it. Files the model process
// creates inside arrive owned by the attempt uid with its own gid, and the
// sandbox shim's umask keeps them group-rw, so the supervisor can still
// import and remove them afterwards.
func prepareAttemptTrees(gid uint32, roots ...string) error {
	for _, root := range roots {
		if err := os.MkdirAll(root, 0o770); err != nil {
			return err
		}
		if err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if err := os.Chown(path, -1, int(gid)); err != nil {
				return err
			}
			if entry.IsDir() {
				return os.Chmod(path, 0o770)
			}
			return os.Chmod(path, 0o660)
		}); err != nil {
			return err
		}
	}
	return nil
}

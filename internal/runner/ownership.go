package runner

import (
	"io/fs"
	"os"
	"path/filepath"
)

// The attempt identity's filesystem access is granted on the group axis,
// never by changing ownership: the supervisor holds no CAP_CHOWN, but it
// owns everything it stages and belongs to every attempt group (template
// Dockerfile), and chgrp to a group the owner belongs to is unprivileged.
// Each attempt's primary gid equals its uid, so group bits reach exactly one
// attempt identity, the supervisor keeps owner access for import and
// cleanup, and other attempts have no bits at all.

// prepareWorkspaceAccess makes the staged workspace readable by exactly one
// attempt identity: every path joins the attempt's group with read-only
// group bits and zero world bits.
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

// prepareAttemptTrees makes the trees the attempt may mutate -- staged
// memory, the run tmp, the attempt home -- group-writable for it. Files the
// model process creates inside arrive owned by the attempt uid with its own
// gid, and the sandbox shim's umask keeps them group-rw, so the supervisor
// can still import and remove them afterwards.
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

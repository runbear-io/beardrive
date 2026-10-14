package store

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// The paused marker is per-device, per-volume state: `bdrive stop` sets it,
// `bdrive init` clears it. While set, nothing may sync the volume — not the
// daemon, not `bdrive sync`, and especially not the agent turn hooks, which
// would otherwise silently resume a project the user explicitly paused.
// Free functions on the volume dir so the gate is checked without opening
// (and flocking) the store.

func pausedPath(dir string) string { return filepath.Join(dir, "paused") }

func pausedMountPath(dir, mountID string) (string, error) {
	if mountID == "" {
		return pausedPath(dir), nil
	}
	if mountID == "." || mountID == ".." || strings.ContainsAny(mountID, `/\`) {
		return "", fmt.Errorf("invalid mount id %q", mountID)
	}
	return filepath.Join(dir, "paused-"+mountID), nil
}

// Paused reports whether syncing is paused for the volume dir.
func Paused(dir string) bool {
	_, err := os.Stat(pausedPath(dir))
	return err == nil
}

// PausedMount reports whether syncing is paused for the volume or mount.
func PausedMount(dir, mountID string) bool {
	if Paused(dir) {
		return true
	}
	if mountID == "" {
		return false
	}
	p, err := pausedMountPath(dir, mountID)
	if err != nil {
		return false
	}
	_, err = os.Stat(p)
	return err == nil
}

// SetPausedMount sets or clears the mount-specific pause marker.
func SetPausedMount(dir, mountID string, on bool) error {
	if mountID == "" {
		return SetPaused(dir, on)
	}
	p, err := pausedMountPath(dir, mountID)
	if err != nil {
		return err
	}
	if !on {
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			return err
		}
		if err := os.Remove(pausedPath(dir)); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	return WriteFileAtomic(p, []byte("paused by `bdrive stop`; `bdrive init` resumes\n"), 0o644)
}

// SetPaused sets or clears the paused marker.
func SetPaused(dir string, on bool) error {
	if !on {
		err := os.Remove(pausedPath(dir))
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	return WriteFileAtomic(pausedPath(dir), []byte("paused by `bdrive stop`; `bdrive init` resumes\n"), 0o644)
}

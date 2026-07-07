package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// ProjectFile is the name of the per-folder settings file at the mount root.
// It travels with the project (copy the folder, `sfs mnt .`, and the same
// volume/remote apply) but is never synced — remotes and credentials setups
// are often device-specific, and syncing it would let one device silently
// repoint another.
const ProjectFile = ".sfs"

// Project holds the settings stored in <folder>/.sfs.
type Project struct {
	Volume string `json:"volume,omitempty"`
	Remote string `json:"remote,omitempty"`
	// Include optionally narrows what syncs: when non-empty, only paths
	// matching one of these patterns (gitignore-style, same syntax as
	// .sfsignore) are scanned and materialized.
	Include []string `json:"include,omitempty"`
}

// LoadProject reads <folder>/.sfs; ok is false if the file does not exist.
func LoadProject(folder string) (Project, bool, error) {
	var p Project
	data, err := os.ReadFile(filepath.Join(folder, ProjectFile))
	if err != nil {
		if os.IsNotExist(err) {
			return p, false, nil
		}
		return p, false, err
	}
	if err := json.Unmarshal(data, &p); err != nil {
		return p, false, fmt.Errorf("parse %s: %w", ProjectFile, err)
	}
	return p, true, nil
}

// SaveProject writes <folder>/.sfs.
func SaveProject(folder string, p Project) error {
	return writeJSON(filepath.Join(folder, ProjectFile), p)
}

// EffectiveMount resolves a folder's mount settings: the project file wins
// over the global registry, so hand-edits to .sfs (or a folder copied with
// its .sfs) take effect without re-registering. Found reports whether the
// folder is known at all (registered or carrying a project file).
func EffectiveMount(folder string) (mi MountInfo, proj Project, found bool, err error) {
	mounts, err := LoadMounts()
	if err != nil {
		return mi, proj, false, err
	}
	mi, registered := mounts[folder]
	proj, hasProj, err := LoadProject(folder)
	if err != nil {
		return mi, proj, false, err
	}
	if hasProj {
		if proj.Volume != "" {
			mi.Volume = proj.Volume
		}
		if proj.Remote != "" {
			mi.Remote = proj.Remote
		}
	}
	return mi, proj, registered || hasProj, nil
}

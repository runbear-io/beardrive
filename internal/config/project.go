package config

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// ProjectDir is the per-folder settings directory at the mount root. It
// carries the mount's stable identity, so a project keeps syncing after the
// folder is renamed or moved — nothing is keyed by the path. It travels with
// the folder (copy the folder to a new machine and `bdrive init` resumes the
// same project) but is never synced, and it holds no session credentials —
// those stay in the bdrive home.
const ProjectDir = ".bdrive"

// Project holds the settings stored in <folder>/.bdrive/config.json.
type Project struct {
	// ID is the stable mount identity (m-xxxxxxxx). The volume store, the
	// daemon, and the registry are keyed by it, never by the folder path.
	ID     string `json:"id"`
	Volume string `json:"volume,omitempty"`
	Remote string `json:"remote,omitempty"`
	// Include optionally narrows what syncs: when non-empty, only paths
	// matching one of these patterns (gitignore-style, same syntax as
	// .bdriveignore) are scanned and materialized.
	Include []string `json:"include,omitempty"`
}

// NewMountID mints a stable mount identity.
func NewMountID() string {
	b := make([]byte, 4)
	rand.Read(b)
	return "m-" + hex.EncodeToString(b)
}

func projectConfigPath(folder string) string {
	return filepath.Join(folder, ProjectDir, "config.json")
}

// LoadProject reads <folder>/.bdrive/config.json; ok is false if it does not
// exist.
func LoadProject(folder string) (Project, bool, error) {
	var p Project
	data, err := os.ReadFile(projectConfigPath(folder))
	if err != nil {
		if os.IsNotExist(err) {
			return p, false, nil
		}
		return p, false, err
	}
	if err := json.Unmarshal(data, &p); err != nil {
		return p, false, fmt.Errorf("parse %s: %w", projectConfigPath(folder), err)
	}
	return p, true, nil
}

// SaveProject writes <folder>/.bdrive/config.json, assigning a mount ID on
// first save.
func SaveProject(folder string, p Project) (Project, error) {
	if p.ID == "" {
		p.ID = NewMountID()
	}
	if err := os.MkdirAll(filepath.Join(folder, ProjectDir), 0o755); err != nil {
		return p, err
	}
	return p, writeJSON(projectConfigPath(folder), p)
}

// ResolveMount loads a folder's project settings and self-heals the
// registry: if the folder was renamed or moved, the registry entry is
// updated to the new path so `bdrive status` and the daemon find it again.
func ResolveMount(folder string) (Project, bool, error) {
	p, ok, err := LoadProject(folder)
	if err != nil || !ok {
		return p, ok, err
	}
	mounts, err := LoadMounts()
	if err != nil {
		return p, true, err
	}
	mi, registered := mounts[p.ID]
	if !registered || mi.Path != folder || mi.Volume != p.Volume || mi.Remote != p.Remote {
		mounts[p.ID] = MountInfo{Path: folder, Volume: p.Volume, Remote: p.Remote}
		if err := SaveMounts(mounts); err != nil {
			return p, true, err
		}
	}
	return p, true, nil
}

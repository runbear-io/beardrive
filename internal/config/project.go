package config

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// ProjectDir is the per-folder settings directory at the mount root. It
// carries the mount's stable identity, so a project keeps syncing after the
// folder is renamed or moved — nothing is keyed by the path. It travels with
// the folder (copy the folder to a new machine and `bdrive init` resumes the
// same project) but is never synced, and it holds no session credentials —
// those stay in the bdrive home.
const ProjectDir = ".bdrive"

// ReservedDirs are directory names BearDrive never syncs, at any depth in a
// mount: .bdrive is the mount's own identity (syncing it would let one device
// silently repoint another) and .git carries hook scripts that would run on a
// teammate's next commit. The rule lives here beside ProjectDir because two
// packages enforce it — the sync engine on scan and on materialize, the hub
// on every destination path a client names — and two copies would drift.
//
// Match through ReservedDir, never by indexing this map: the comparison is
// case-insensitive because BearDrive's primary filesystems (APFS, NTFS) are.
// An exact-match guard lets ".GIT/hooks/pre-commit" through, and the
// filesystem then resolves it into the real .git/hooks.
var ReservedDirs = map[string]bool{".git": true, ProjectDir: true}

// ReservedDir reports whether a path segment names a reserved directory,
// case-insensitively.
func ReservedDir(name string) bool {
	for reserved := range ReservedDirs {
		if strings.EqualFold(name, reserved) {
			return true
		}
	}
	return false
}

// ReservedName reports whether a bare file name never syncs. Case-insensitive
// for the same reason as ReservedDir.
func ReservedName(name string) bool {
	lower := strings.ToLower(name)
	return lower == ".ds_store" || strings.HasPrefix(lower, ".bdrive-tmp-")
}

// ReservedPath reports whether a slash-separated path is one BearDrive never
// carries: under a reserved directory, named like one, or a reserved file
// name.
func ReservedPath(p string) bool {
	for _, part := range strings.Split(p, "/") {
		if ReservedDir(part) {
			return true
		}
	}
	return ReservedName(path.Base(p))
}

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

// IsMount reports whether folder is a BearDrive mount root, i.e. has a
// .bdrive/config.json — even an unparseable one, so callers that must not
// treat a mount as plain files (e.g. a parent mount's scanner) stay safe.
func IsMount(folder string) bool {
	_, err := os.Stat(projectConfigPath(folder))
	return err == nil
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
	p.Include = normalizeInclude(p.Include)
	return p, true, nil
}

// normalizeInclude anchors bare single-segment include entries to the mount
// root, so a config written before the fix ("wiki/") stops matching nested
// directories of the same name without needing a re-init. Only single-segment
// entries need it: compile() already anchors anything containing a slash.
// Entries with glob syntax are left alone — a hand-written pattern is a
// deliberate pattern.
func normalizeInclude(include []string) []string {
	for n, i := range include {
		s := strings.TrimSuffix(i, "/")
		if s == "" || strings.ContainsAny(s, "/*?[!") {
			continue
		}
		include[n] = "/" + i
	}
	return include
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

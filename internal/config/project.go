package config

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
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
// under every spelling a filesystem folds onto the same directory.
//
// Case is one such folding (APFS, NTFS). Trailing dots and spaces are another:
// NTFS and SMB strip them when opening a path, so ".git./hooks/pre-commit" IS
// .git/hooks/pre-commit there — the same executable-hook plant an exact-match
// guard let through as ".GIT".
func ReservedDir(name string) bool {
	name = strings.TrimRight(name, ". ")
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

// mountIDRe is the shape of a mount identity. The id is read verbatim from a
// folder's .bdrive/config.json — a file that arrives with the folder (a zip, a
// clone, a colleague's copy) — and is then joined straight onto $BDRIVE_HOME
// by VolumeDir and onto the volume dir by the store's state cache. Checking it
// here, where it is read, is what stops the whole volume store (cached blobs
// of every synced file, journals, the daemon's pid and lock) being created
// wherever the config's author chose.
var mountIDRe = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

// ValidMountID reports whether id may be used as a mount identity.
func ValidMountID(id string) bool {
	return id != "." && id != ".." && mountIDRe.MatchString(id)
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
	// An empty id is a config written before one was assigned; anything else
	// has to be a mount id, since everything downstream builds a path from it.
	if p.ID != "" && !ValidMountID(p.ID) {
		return Project{}, false, fmt.Errorf("%s: invalid mount id", projectConfigPath(folder))
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

// mountLivesAt reports whether path still holds the config of mount id.
func mountLivesAt(path, id string) bool {
	p, ok, err := LoadProject(path)
	return err == nil && ok && p.ID == id
}

// samePath reports whether two spellings name the same directory (macOS
// /var vs /private/var, a symlinked home): a spelling difference is not a
// move, and must not read as one in either direction.
func samePath(a, b string) bool {
	if a == b {
		return true
	}
	ra, err1 := filepath.EvalSymlinks(a)
	rb, err2 := filepath.EvalSymlinks(b)
	return err1 == nil && err2 == nil && ra == rb
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
	// The self-heal follows a mount that MOVED, and .bdrive/config.json
	// travels with the folder — a clone, an unpacked archive, a colleague's
	// copy — so "some folder carries this id" is not "this mount is now
	// there". If the recorded path still holds this mount's own config, the
	// mount did not move and the arriving folder is a copy: re-pointing the
	// row would hand the real project's Path, Volume and Remote to it, and
	// `bdrive resume` (and the login autostart) start the daemon from that
	// row. Enrolling a folder is what `bdrive init` is for.
	if registered && !samePath(mi.Path, folder) && mountLivesAt(mi.Path, p.ID) {
		return p, false, fmt.Errorf("%s carries the settings of project %s, which this device already "+
			"syncs at %s — a copy of a project folder is not that project; run `bdrive init` here to "+
			"connect this folder to a project", folder, p.ID, mi.Path)
	}
	if !registered || mi.Path != folder || mi.Volume != p.Volume || mi.Remote != p.Remote {
		mounts[p.ID] = MountInfo{Path: folder, Volume: p.Volume, Remote: p.Remote}
		if err := SaveMounts(mounts); err != nil {
			return p, true, err
		}
	}
	return p, true, nil
}

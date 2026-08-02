// Package config manages beardrive's global state under the beardrive home directory
// (default ~/.bdrive, overridable with $BDRIVE_HOME): the device identity and the
// registry of mounted folders.
package config

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
)

// Home returns the beardrive home directory ($BDRIVE_HOME or ~/.bdrive).
func Home() (string, error) {
	if h := os.Getenv("BDRIVE_HOME"); h != "" {
		// Absolute, always: every caller joins paths onto this or compares
		// paths against it, and a relative value silently resolves against
		// whatever working directory the process happens to have — which made
		// the guard that refuses to mount the home fail open (filepath.Rel of
		// an absolute path against a relative root is an error).
		return filepath.Abs(h)
	}
	uh, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(uh, ".bdrive"), nil
}

// ensureHome creates $BDRIVE_HOME and returns it. It is the ONE creator.
//
// Every file under this directory is 0600 so no other local account can read
// this device's project list, its authorship or the signed-in address — but the
// LISTING is the same metadata and needs no file opened: volumes/<mount-id>/
// names every project this device syncs, journal/<device>.jsonl names every
// device in the fleet, and blobs/<aa>/<sha256> answers "does this machine hold
// the file whose sha256 is X". The two creators used to disagree (0700 here,
// 0755 in LoadDevice, which runs on essentially every command path) and
// MkdirAll does not re-mode a directory that already exists, so whichever ran
// first on a fresh machine decided.
func ensureHome() (string, error) {
	home, err := Home()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(home, 0o700); err != nil {
		return "", err
	}
	// MkdirAll leaves an existing directory's mode alone, and every install
	// made before this one created it 0755.
	if fi, err := os.Stat(home); err == nil && fi.Mode().Perm()&0o077 != 0 {
		_ = os.Chmod(home, fi.Mode().Perm()&^0o077)
	}
	return home, nil
}

// Device identifies this machine and its operator in journals.
type Device struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Author string `json:"author"`
}

// LoadDevice loads the device identity, creating one on first use.
func LoadDevice() (Device, error) {
	home, err := Home()
	if err != nil {
		return Device{}, err
	}
	p := filepath.Join(home, "device.json")
	if data, err := os.ReadFile(p); err == nil {
		var d Device
		if err := json.Unmarshal(data, &d); err == nil && d.ID != "" {
			return d, nil
		}
	}
	d := Device{ID: randID(), Name: hostname(), Author: detectAuthor()}
	if _, err := ensureHome(); err != nil {
		return Device{}, err
	}
	if err := writeJSON(p, d); err != nil {
		return Device{}, err
	}
	return d, nil
}

func randID() string {
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		return "device000000"
	}
	return hex.EncodeToString(b)
}

func hostname() string {
	h, _ := os.Hostname()
	h = strings.TrimSuffix(h, ".local")
	if h == "" {
		h = "device"
	}
	return h
}

func detectAuthor() string {
	if out, err := exec.Command("git", "config", "--get", "user.email").Output(); err == nil {
		if s := strings.TrimSpace(string(out)); s != "" {
			return s
		}
	}
	u := os.Getenv("USER")
	if u == "" {
		if cu, err := user.Current(); err == nil {
			u = cu.Username
		}
	}
	if u == "" {
		u = "unknown"
	}
	return u + "@" + hostname()
}

// MountInfo is the registry's view of one mount: where the folder currently
// lives. The source of truth for identity/settings is the folder's own
// .bdrive/config.json; the registry only remembers the last-known path (for
// `bdrive status` and the daemon) and self-heals when the folder moves.
type MountInfo struct {
	Path   string `json:"path"`
	Volume string `json:"volume,omitempty"`
	Remote string `json:"remote,omitempty"`
	// Dev+Ino are the filesystem's identity for Path at the time this row was
	// written. They are what tells a MOVED folder (rename: same inode, new
	// path) from a second folder carrying a copy of the same
	// .bdrive/config.json (new inode). Zero on rows written before this field
	// existed, and on platforms with no such identity; see dirID.
	Dev uint64 `json:"dev,omitempty"`
	Ino uint64 `json:"ino,omitempty"`
}

func mountsPath() (string, error) {
	home, err := Home()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "mounts.json"), nil
}

// LoadMounts returns the mount-ID → mount registry.
func LoadMounts() (map[string]MountInfo, error) {
	p, err := mountsPath()
	if err != nil {
		return nil, err
	}
	out := map[string]MountInfo{}
	data, err := os.ReadFile(p)
	if err != nil {
		if os.IsNotExist(err) {
			return out, nil
		}
		return nil, err
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// SaveMounts writes the registry, stamping an identity on any row that has
// none. Every row written before Dev/Ino existed carries zeros, and the
// move-vs-copy discriminator is only consulted when a row HAS one — so without
// this backfill the guard is inert on every mount already enrolled on every
// machine, which is to say on all of them. The stamp is taken while the
// recorded path still holds this mount, which is the only moment the answer is
// unambiguous; a row whose path has already stopped answering is left alone
// rather than given the identity of whatever is sitting there now.
func SaveMounts(m map[string]MountInfo) error {
	p, err := mountsPath()
	if err != nil {
		return err
	}
	if _, err := ensureHome(); err != nil {
		return err
	}
	for id, mi := range m {
		if mi.Dev != 0 || mi.Ino != 0 || mi.Path == "" || !mountLivesAt(mi.Path, id) {
			continue
		}
		if dev, ino := dirID(mi.Path); dev != 0 {
			mi.Dev, mi.Ino = dev, ino
			m[id] = mi
		}
	}
	return writeJSON(p, m)
}

// VolumeDir returns the local store dir of a mount, keyed by its stable
// mount ID (never the folder path — that's what makes renames/moves free).
//
// The id is validated here, where it becomes a path: LoadProject checks the
// one it reads out of a folder's config, but mounts.json is unmarshalled into
// a map whose KEYS nothing looks at, and `bdrive resume` — what the login
// agent runs at every boot — builds this path out of the key. A registry entry
// is plain JSON in $BDRIVE_HOME that anything running as the user can write.
func VolumeDir(mountID string) (string, error) {
	if !ValidMountID(mountID) {
		return "", fmt.Errorf("invalid mount id %q", mountID)
	}
	home, err := Home()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "volumes", mountID), nil
}

func writeJSON(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".bdrive-tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// Package config manages beardrive's global state under the beardrive home directory
// (default ~/.beardrive, overridable with $BEARDRIVE_HOME): the device identity and the
// registry of mounted folders.
package config

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
)

// Home returns the beardrive home directory ($BEARDRIVE_HOME or ~/.beardrive).
func Home() (string, error) {
	if h := os.Getenv("BEARDRIVE_HOME"); h != "" {
		return h, nil
	}
	uh, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(uh, ".beardrive"), nil
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
	if err := os.MkdirAll(home, 0o755); err != nil {
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

// MountInfo describes one mounted folder.
type MountInfo struct {
	Volume string `json:"volume"`
	Remote string `json:"remote,omitempty"`
}

func mountsPath() (string, error) {
	home, err := Home()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "mounts.json"), nil
}

// LoadMounts returns the abs-folder → mount registry.
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

func SaveMounts(m map[string]MountInfo) error {
	p, err := mountsPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	return writeJSON(p, m)
}

// MountID derives a stable identifier for a mounted folder. One volume can
// be mounted at several folders (even on the same device); everything
// folder-specific — the sync daemon and the materialization cache — is keyed
// by this ID, while blobs and journals stay shared per volume.
func MountID(folder string) string {
	sum := sha256.Sum256([]byte(folder))
	return hex.EncodeToString(sum[:])[:12]
}

// VolumeDir returns (and creates parents for) the local store dir of a volume.
func VolumeDir(volume string) (string, error) {
	home, err := Home()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "volumes", volume), nil
}

func writeJSON(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".beardrive-tmp-*")
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

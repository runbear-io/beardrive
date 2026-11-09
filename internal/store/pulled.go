package store

import (
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
)

// The pulled marker is the daemon vouching that this volume's files are
// current: which daemon process, for which mounts, as of when. An agent turn
// hook beside a running daemon reads the files on disk instead of pulling
// itself, and only while the marker is recent, names the daemon holding the
// lock now, and lists the hook's mount. The daemon writes it after a tick that
// pulled every mount, keeps it fresh while the hub's change stream is
// demonstrably alive, and clears it the moment a peer announces a push it has
// not pulled yet. Free functions on the volume dir, like the paused marker.

func pulledPath(dir string) string { return filepath.Join(dir, "pulled") }

// MarkPulled records that this process vouches for mounts as of now.
func MarkPulled(dir string, mounts []string) error {
	body := strconv.Itoa(os.Getpid()) + "\n" + strings.Join(mounts, "\n") + "\n"
	return os.WriteFile(pulledPath(dir), []byte(body), 0o600)
}

// ClearPulled withdraws the marker.
func ClearPulled(dir string) {
	os.Remove(pulledPath(dir))
}

// Pulled is what the marker vouches for.
type Pulled struct {
	At     time.Time
	PID    int
	Mounts []string
}

// Covers reports whether the marker lists mountID.
func (p Pulled) Covers(mountID string) bool {
	return slices.Contains(p.Mounts, mountID)
}

// LoadPulled reads the marker; the zero value when there is none.
func LoadPulled(dir string) Pulled {
	f, err := os.Open(pulledPath(dir))
	if err != nil {
		return Pulled{}
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return Pulled{}
	}
	data := make([]byte, min(fi.Size(), 1<<16))
	n, _ := f.Read(data)
	lines := strings.Split(strings.TrimSpace(string(data[:n])), "\n")
	pid, _ := strconv.Atoi(lines[0])
	return Pulled{At: fi.ModTime(), PID: pid, Mounts: lines[1:]}
}

package store

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// The pulled marker is when the daemon last finished a pass against the hub
// for this volume, and which daemon process finished it. An agent turn hook
// beside a running daemon reads the files on disk instead of pulling itself,
// and only while the marker is recent and names the daemon holding the lock
// now: a daemon that is offline or reconnecting holds its lock but stops
// refreshing the marker, and a restarted one has not passed yet. Free
// functions on the volume dir, like the paused marker.

func pulledPath(dir string) string { return filepath.Join(dir, "pulled") }

// MarkPulled records that this process just completed a pass against the hub.
func MarkPulled(dir string) error {
	return os.WriteFile(pulledPath(dir), []byte(strconv.Itoa(os.Getpid())+"\n"), 0o600)
}

// PulledAt reports when the last pass against the hub completed and the pid
// of the process that completed it; zero values if none is recorded.
func PulledAt(dir string) (time.Time, int) {
	data, err := os.ReadFile(pulledPath(dir))
	if err != nil {
		return time.Time{}, 0
	}
	fi, err := os.Stat(pulledPath(dir))
	if err != nil {
		return time.Time{}, 0
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(string(data)))
	return fi.ModTime(), pid
}

package store

import (
	"io"
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
	return MarkPulledUntil(dir, mounts, time.Time{})
}

// MarkPulledUntil is MarkPulled with a deadline past which the vouch stands
// for nothing however recently it was written: the moment the daemon would
// stop believing its change stream is alive. Zero means no deadline.
func MarkPulledUntil(dir string, mounts []string, until time.Time) error {
	head := strconv.Itoa(os.Getpid())
	if !until.IsZero() {
		head += " " + strconv.FormatInt(until.UnixNano(), 10)
	}
	body := head + "\n" + strings.Join(mounts, "\n") + "\n"
	// Replaced whole, never rewritten in place: a hook reading while the
	// daemon renews must see the old marker or the new one, not a prefix.
	tmp, err := os.CreateTemp(dir, ".pulled-*")
	if err != nil {
		return err
	}
	if _, err := tmp.WriteString(body); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	if err := os.Chmod(tmp.Name(), 0o600); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	if err := os.Rename(tmp.Name(), pulledPath(dir)); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	return nil
}

// ClearPulled withdraws the marker.
func ClearPulled(dir string) {
	os.Remove(pulledPath(dir))
}

// Pulled is what the marker vouches for.
type Pulled struct {
	At     time.Time
	Until  time.Time // zero when the marker carries no deadline
	PID    int
	Mounts []string
}

// FreshWithin reports whether the marker was written within window and has
// not passed its deadline.
func (p Pulled) FreshWithin(window time.Duration) bool {
	now := time.Now()
	return !p.At.IsZero() && now.Sub(p.At) < window && (p.Until.IsZero() || now.Before(p.Until))
}

// Covers reports whether the marker lists mountID.
func (p Pulled) Covers(mountID string) bool {
	return slices.Contains(p.Mounts, mountID)
}

// LoadPulled reads the marker; the zero value when there is none, or when what
// is there is not a whole marker (no final newline, no pid), which vouches for
// nothing.
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
	n, _ := io.ReadFull(f, data)
	if n == 0 || int64(n) != fi.Size() || data[n-1] != '\n' {
		return Pulled{}
	}
	lines := strings.Split(strings.TrimSpace(string(data[:n])), "\n")
	head := strings.Fields(lines[0])
	var p Pulled
	if len(head) > 0 {
		p.PID, _ = strconv.Atoi(head[0])
	}
	if len(head) > 1 {
		if n, err := strconv.ParseInt(head[1], 10, 64); err == nil {
			p.Until = time.Unix(0, n)
		}
	}
	if p.PID <= 0 {
		return Pulled{}
	}
	p.At, p.Mounts = fi.ModTime(), lines[1:]
	return p
}

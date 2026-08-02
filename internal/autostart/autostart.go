// Package autostart registers beardrive to start syncing again when the user
// logs in, so a reboot doesn't silently stop sync.
//
// The daemon is a detached child process (see internal/daemon): a reboot kills
// it and nothing brought it back — every project stayed unsynced until someone
// ran `bdrive init` in that folder again. Agent hooks still synced per turn,
// which is exactly what made the gap easy to miss: the folder looked fine
// while an agent was working in it and went stale the moment one wasn't.
//
// The unit registered here is ONE per machine, not one per project. It runs
// `bdrive resume`, which walks the mount registry and starts a daemon for
// every enrolled, unpaused mount — so mounts added or removed later need no
// change to the registration, and `bdrive stop` keeps meaning "stay stopped".
//
// Platform support: macOS (launchd user agent), Linux (systemd user unit), and
// Windows (a per-user Run entry). Anything else gets the stub, which returns
// ErrUnsupported — callers already treat that as "nothing to do".
//
// No implementation shells out to a service manager (`launchctl`, `systemctl`,
// `schtasks`). Writing the registration IS the registration: launchd reads
// ~/Library/LaunchAgents at login, systemd reads the enable symlink in
// default.target.wants, Explorer reads HKCU\...\Run at logon. All of them
// matter only at the NEXT login — the caller has just started the daemon for
// this session — and shelling out would let a test or a packaging script
// register something real as a side effect, or fail on a machine with no
// session bus at all (ssh, container, CI).
package autostart

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/runbear-io/beardrive/internal/store"
)

// ErrUnsupported is returned by Install/Uninstall on platforms that have no
// implementation yet. Callers treat it as "nothing to do", never as failure:
// autostart is a convenience, and a hard error would break `bdrive init` on a
// platform that syncs perfectly well without it.
var ErrUnsupported = errors.New("autostart is not supported on this platform yet")

// Result reports what Install did, mirroring agenthooks.Result so callers can
// print the two the same way.
type Result struct {
	Path    string // the file written (or that would be)
	Changed bool   // false when it was already correct
}

// writeIfDifferent writes content to path unless it is already exactly that,
// and reports whether it wrote. The write goes through store.WriteFileAtomic —
// the repo's one atomic write — rather than a second copy of it: this copy
// used a fully predictable temp name (".bdrive-tmp-" + base), and os.WriteFile
// follows a symlink at the destination, so a link planted at that name turned
// "register autostart" into "truncate a file the user owns", leaving no
// registration behind either.
//
// "Unless already exactly that" is what makes Install idempotent enough for
// `bdrive init` to call on every run, while still correcting a stale binary
// path (a Homebrew prefix change, a moved binary) instead of skipping it.
func writeIfDifferent(path, content string) (bool, error) {
	if have, err := os.ReadFile(path); err == nil && string(have) == content {
		return false, nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return false, err
	}
	if err := store.WriteFileAtomic(path, []byte(content), 0o644); err != nil {
		return false, err
	}
	return true, nil
}

// loginPath is selfPath() checked for what the registration formats can carry.
// The registration is a command line a service manager runs at every login,
// before any guard of ours: a control character in the path ends the plist's
// string or the unit's ExecStart= line and starts something the format reads
// as a new directive. There is no escaping for that in either format, so it is
// refused — Install failing loudly beats writing a login command nobody
// reviewed (or one launchd silently never loads).
func loginPath() (string, error) {
	exe, err := selfPath()
	if err != nil {
		return "", err
	}
	for _, r := range exe {
		if r < 0x20 || r == 0x7f {
			return "", fmt.Errorf("refusing to register %q at login: the path contains a control character", exe)
		}
	}
	return exe, nil
}

// selfPath is the binary to register. Symlinks are resolved because the
// service manager holds this path for the next login: Homebrew installs a
// symlink into its prefix, and an upgrade can repoint it.
func selfPath() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		return resolved, nil
	}
	return exe, nil
}

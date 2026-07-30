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
// Platform support: macOS (launchd user agent) today. Linux (systemd user
// unit) and Windows (Startup task) are the same three functions with a
// different file; the unsupported stub returns ErrUnsupported so callers are
// already written for them.
package autostart

import "errors"

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

//go:build darwin

package autostart

import (
	"os"
	"path/filepath"
)

// Label is the launchd job label, and names the plist file.
const Label = "ai.beardrive.daemon"

// Path is where the user's LaunchAgent lives. User-level on purpose: no sudo,
// no machine-wide state, and it starts as that user with their $HOME — a
// LaunchDaemon would run as root and sync the wrong account's projects.
func Path() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "Library", "LaunchAgents", Label+".plist"), nil
}

// plist renders the agent. RunAtLoad with no KeepAlive is deliberate:
// `bdrive resume` starts the daemons and exits, so KeepAlive would read that
// exit as a crash and respawn it forever.
func plist(exe string) string {
	return `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>` + Label + `</string>
	<key>ProgramArguments</key>
	<array>
		<string>` + exe + `</string>
		<string>resume</string>
	</array>
	<key>RunAtLoad</key>
	<true/>
	<key>ProcessType</key>
	<string>Background</string>
</dict>
</plist>
`
}

// Install writes (or refreshes) the LaunchAgent. Writing the file is the whole
// job: launchd loads agents from this directory at login. See the package doc
// for why there is no `launchctl` call.
func Install() (Result, error) {
	path, err := Path()
	if err != nil {
		return Result{}, err
	}
	exe, err := selfPath()
	if err != nil {
		return Result{}, err
	}
	changed, err := writeIfDifferent(path, plist(exe))
	if err != nil {
		return Result{}, err
	}
	return Result{Path: path, Changed: changed}, nil
}

// Installed reports whether the agent file is in place.
func Installed() bool {
	path, err := Path()
	if err != nil {
		return false
	}
	_, err = os.Stat(path)
	return err == nil
}

// Uninstall removes the agent, so it no longer loads at login. Missing is
// success. Nothing to unload: the job is RunAtLoad-and-exit, so by now it has
// already run and gone.
func Uninstall() (Result, error) {
	path, err := Path()
	if err != nil {
		return Result{}, err
	}
	if _, err := os.Stat(path); err != nil {
		return Result{Path: path, Changed: false}, nil
	}
	if err := os.Remove(path); err != nil {
		return Result{}, err
	}
	return Result{Path: path, Changed: true}, nil
}

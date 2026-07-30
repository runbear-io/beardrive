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

// Install writes (or refreshes) the LaunchAgent. Idempotent: an identical
// plist is left alone and reported as unchanged, so `bdrive init` can call
// this on every run.
//
// Writing the file is the whole job — launchd loads agents in this directory
// at login, which is the only moment that matters here. There is deliberately
// no `launchctl bootstrap`: the caller has just started the daemon for this
// session, so activating the job now would at best re-run `bdrive resume`
// against already-running daemons, and shelling out to launchctl from a test
// or a packaging script would register a real login item as a side effect.
//
// The binary path is baked in, so an upgrade that moves the binary (Homebrew
// prefix change, a move out of /usr/local) needs a re-run — which init does
// anyway, and which is why a stale path rewrites rather than being skipped.
func Install() (Result, error) {
	path, err := Path()
	if err != nil {
		return Result{}, err
	}
	exe, err := os.Executable()
	if err != nil {
		return Result{}, err
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved // Homebrew installs a symlink; launchd should hold the real path
	}
	want := plist(exe)
	if have, err := os.ReadFile(path); err == nil && string(have) == want {
		return Result{Path: path, Changed: false}, nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return Result{}, err
	}
	tmp := filepath.Join(filepath.Dir(path), ".bdrive-tmp-"+Label+".plist")
	if err := os.WriteFile(tmp, []byte(want), 0o644); err != nil {
		return Result{}, err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return Result{}, err
	}
	return Result{Path: path, Changed: true}, nil
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

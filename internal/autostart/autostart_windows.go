//go:build windows

package autostart

import (
	"strings"

	"golang.org/x/sys/windows/registry"
)

// Windows has no user service manager in the launchd/systemd sense, so the
// registration is a per-user Run entry: HKCU\...\CurrentVersion\Run, which
// Explorer executes once at logon. Chosen over the alternatives because it
// needs no admin rights, no COM (a Startup-folder .lnk does), and no
// shell-out to schtasks — the same "write the registration, don't talk to a
// service manager" rule the other platforms follow.
//
// It is also honestly discoverable: the entry shows up in Task Manager's
// Startup tab, where a user can disable it without knowing bdrive exists.
const (
	runKey    = `Software\Microsoft\Windows\CurrentVersion\Run`
	valueName = "BearDrive"
)

// Path names the registration for display. There is no file, so this is the
// registry location — what a user would look at to verify or remove it.
func Path() (string, error) {
	return `HKCU\` + runKey + `\` + valueName, nil
}

// command is the Run value. The executable is quoted because Program Files
// has a space in it and Explorer parses this as a command line.
//
// A brief console window can flash at logon: bdrive is a console binary and
// Run gives it a console. `resume --quiet` exits in milliseconds, so this is
// a flicker rather than a window; hiding it entirely would mean shipping a
// second GUI-subsystem launcher, which is not worth a binary for.
func command(exe string) string {
	return `"` + exe + `" resume --quiet`
}

// Install writes the Run value, creating the key if needed. Idempotent: an
// identical value is reported as unchanged, so `bdrive init` can call it every
// run, while a stale path (an upgrade that moved bdrive.exe) is rewritten.
func Install() (Result, error) {
	path, _ := Path()
	exe, err := selfPath()
	if err != nil {
		return Result{}, err
	}
	want := command(exe)

	key, _, err := registry.CreateKey(registry.CURRENT_USER, runKey, registry.QUERY_VALUE|registry.SET_VALUE)
	if err != nil {
		return Result{}, err
	}
	defer key.Close()

	if have, _, err := key.GetStringValue(valueName); err == nil && strings.EqualFold(have, want) {
		return Result{Path: path, Changed: false}, nil
	}
	if err := key.SetStringValue(valueName, want); err != nil {
		return Result{}, err
	}
	return Result{Path: path, Changed: true}, nil
}

// Installed reports whether the Run value is present.
func Installed() bool {
	key, err := registry.OpenKey(registry.CURRENT_USER, runKey, registry.QUERY_VALUE)
	if err != nil {
		return false
	}
	defer key.Close()
	_, _, err = key.GetStringValue(valueName)
	return err == nil
}

// Uninstall removes the Run value. Missing is success. Running daemons are
// untouched — this only decides what happens at the next logon.
func Uninstall() (Result, error) {
	path, _ := Path()
	key, err := registry.OpenKey(registry.CURRENT_USER, runKey, registry.SET_VALUE|registry.QUERY_VALUE)
	if err != nil {
		if err == registry.ErrNotExist {
			return Result{Path: path, Changed: false}, nil
		}
		return Result{}, err
	}
	defer key.Close()
	if _, _, err := key.GetStringValue(valueName); err != nil {
		return Result{Path: path, Changed: false}, nil
	}
	if err := key.DeleteValue(valueName); err != nil {
		return Result{}, err
	}
	return Result{Path: path, Changed: true}, nil
}

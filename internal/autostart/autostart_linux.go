//go:build linux

package autostart

import (
	"os"
	"path/filepath"
)

// Unit is the systemd user unit's name, and the file it lives in.
const Unit = "beardrive.service"

// wantsDir is the directory systemd reads to decide what starts with the
// user's default target. A symlink in here is exactly what
// `systemctl --user enable` creates for a unit with WantedBy=default.target —
// so we create it ourselves and never shell out (see the package doc).
const wantsDir = "default.target.wants"

// unitDir is the user unit directory: $XDG_CONFIG_HOME/systemd/user, falling
// back to ~/.config/systemd/user. os.UserConfigDir already implements that
// rule, which matters on distros and desktops that relocate XDG_CONFIG_HOME.
func unitDir() (string, error) {
	cfg, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(cfg, "systemd", "user"), nil
}

// Path is the unit file this package owns.
func Path() (string, error) {
	dir, err := unitDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, Unit), nil
}

// booted reports whether systemd is the init system. This is sd_booted(3):
// /run/systemd/system exists only under systemd. Without it a unit file is
// inert decoration — Alpine/runit, WSL1, and slim containers would be told
// "registered" while nothing would ever start.
func booted() bool {
	fi, err := os.Stat("/run/systemd/system")
	return err == nil && fi.IsDir()
}

// unit renders the service. Type=oneshot because `bdrive resume` starts the
// daemons and exits; systemd must not treat that exit as a failure or restart
// it. No After=network-online.target on purpose: a cycle with an unreachable
// hub degrades to offline and retries, so waiting for the network would delay
// local scanning for no benefit.
func unit(exe string) string {
	return `[Unit]
Description=BearDrive — resume folder sync
Documentation=https://docs.beardrive.ai/manual/hooks/

[Service]
Type=oneshot
ExecStart=` + exe + ` resume --quiet

[Install]
WantedBy=default.target
`
}

// Install writes the unit and enables it by linking it into
// default.target.wants. Both steps are needed: systemd ignores a unit file
// that nothing wants.
func Install() (Result, error) {
	if !booted() {
		return Result{}, ErrUnsupported
	}
	path, err := Path()
	if err != nil {
		return Result{}, err
	}
	exe, err := selfPath()
	if err != nil {
		return Result{}, err
	}
	changed, err := writeIfDifferent(path, unit(exe))
	if err != nil {
		return Result{}, err
	}
	linked, err := enable(path)
	if err != nil {
		return Result{}, err
	}
	return Result{Path: path, Changed: changed || linked}, nil
}

// enable creates (or repairs) the default.target.wants symlink, reporting
// whether it had to. A relative target keeps the link valid if the config
// directory moves with the user.
func enable(unitPath string) (bool, error) {
	link := filepath.Join(filepath.Dir(unitPath), wantsDir, Unit)
	want := filepath.Join("..", Unit)
	if have, err := os.Readlink(link); err == nil {
		if have == want || have == unitPath {
			return false, nil
		}
	}
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		return false, err
	}
	os.Remove(link) // a wrong link, or a regular file left by something else
	if err := os.Symlink(want, link); err != nil {
		return false, err
	}
	return true, nil
}

// Installed reports whether the unit is both present and enabled — a unit
// file with no wants symlink never starts, so it does not count.
func Installed() bool {
	path, err := Path()
	if err != nil {
		return false
	}
	if _, err := os.Stat(path); err != nil {
		return false
	}
	_, err = os.Lstat(filepath.Join(filepath.Dir(path), wantsDir, Unit))
	return err == nil
}

// Uninstall removes the enable symlink and the unit. Missing is success.
//
// systemd keeps its loaded copy until `daemon-reload` or the next login, but
// the daemons already running are untouched either way, and nothing will start
// at the next login — which is what "uninstalled" has to mean here.
func Uninstall() (Result, error) {
	path, err := Path()
	if err != nil {
		return Result{}, err
	}
	link := filepath.Join(filepath.Dir(path), wantsDir, Unit)
	var changed bool
	if err := os.Remove(link); err == nil {
		changed = true
	}
	if err := os.Remove(path); err == nil {
		changed = true
	} else if !os.IsNotExist(err) {
		return Result{}, err
	}
	return Result{Path: path, Changed: changed}, nil
}

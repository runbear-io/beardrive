//go:build !darwin

package autostart

// Linux wants a systemd user unit (~/.config/systemd/user/beardrive.service
// with WantedBy=default.target, then `systemctl --user enable`), Windows a
// Startup entry or a Task Scheduler logon task. Both are this file plus the
// same three functions; `bdrive resume` is already the command they run.

func Path() (string, error)      { return "", ErrUnsupported }
func Install() (Result, error)   { return Result{}, ErrUnsupported }
func Uninstall() (Result, error) { return Result{}, ErrUnsupported }
func Installed() bool            { return false }

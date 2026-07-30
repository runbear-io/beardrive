//go:build !darwin && !linux

package autostart

// Windows is the remaining platform: a shortcut in the per-user Startup folder
// (%APPDATA%\Microsoft\Windows\Start Menu\Programs\Startup) or a Task
// Scheduler logon task. Either is this file plus the same three functions;
// `bdrive resume` is already the command it needs to run.

func Path() (string, error)      { return "", ErrUnsupported }
func Install() (Result, error)   { return Result{}, ErrUnsupported }
func Uninstall() (Result, error) { return Result{}, ErrUnsupported }
func Installed() bool            { return false }

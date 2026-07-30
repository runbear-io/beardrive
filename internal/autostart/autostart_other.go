//go:build !darwin && !linux && !windows

package autostart

// Everything that is not macOS, Linux, or Windows lands here — the BSDs
// mainly, where the answer would be an rc.d script or the desktop's own
// autostart directory. `bdrive resume` is already the command to run; only the
// registration differs.

func Path() (string, error)      { return "", ErrUnsupported }
func Install() (Result, error)   { return Result{}, ErrUnsupported }
func Uninstall() (Result, error) { return Result{}, ErrUnsupported }
func Installed() bool            { return false }

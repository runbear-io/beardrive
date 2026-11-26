package main

import "golang.org/x/sys/unix"

// swapFiles atomically exchanges two paths (renameat2 RENAME_EXCHANGE).
// errNoSwap means the kernel or filesystem cannot.
func swapFiles(a, b string) error {
	err := unix.Renameat2(unix.AT_FDCWD, a, unix.AT_FDCWD, b, unix.RENAME_EXCHANGE)
	if err == unix.ENOSYS || err == unix.EINVAL || err == unix.ENOTSUP {
		return errNoSwap
	}
	return err
}

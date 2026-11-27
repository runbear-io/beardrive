package main

import "golang.org/x/sys/unix"

// swapFiles atomically exchanges two paths in one directory (renamex_np
// RENAME_SWAP). errNoSwap means the filesystem cannot.
func swapFiles(a, b string) error {
	err := unix.RenamexNp(a, b, unix.RENAME_SWAP)
	if err == unix.ENOTSUP || err == unix.EINVAL {
		return errNoSwap
	}
	return err
}

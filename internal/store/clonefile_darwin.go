//go:build darwin

package store

import (
	"errors"
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

func tryCloneFile(srcPath, dstPath string) error {
	if fi, err := os.Stat(dstPath); err == nil {
		if fi.Size() != 0 {
			return errors.New("clone destination exists")
		}
		if err := os.Remove(dstPath); err != nil {
			return err
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	if _, err := syscall.BytePtrFromString(srcPath); err != nil {
		return err
	}
	if _, err := syscall.BytePtrFromString(dstPath); err != nil {
		return err
	}
	return unix.Clonefile(srcPath, dstPath, 0)
}

func FileIdentity(fi os.FileInfo) (dev uint64, ino uint64, ok bool) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0, false
	}
	return uint64(st.Dev), uint64(st.Ino), true
}

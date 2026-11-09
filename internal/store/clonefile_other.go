//go:build !darwin && !linux

package store

import (
	"errors"
	"os"
)

func tryCloneFile(srcPath, dstPath string) error {
	return errors.New("clonefile unsupported")
}

func FileIdentity(fi os.FileInfo) (dev uint64, ino uint64, ok bool) {
	return 0, 0, false
}

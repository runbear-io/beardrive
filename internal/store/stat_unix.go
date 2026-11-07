//go:build unix || darwin || linux

package store

import (
	"os"
	"syscall"
)

type fileID struct {
	dev     uint64
	ino     uint64
	size    int64
	mtimeNS int64
	ctimeNS int64
	valid   bool
}

func statFileID(path string) (fileID, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return fileID{}, err
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return fileID{}, os.ErrInvalid
	}
	return fileID{dev: uint64(st.Dev), ino: uint64(st.Ino), size: fi.Size(), mtimeNS: fi.ModTime().UnixNano(), valid: true}, nil
}

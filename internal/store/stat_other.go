//go:build !unix && !darwin && !linux

package store

import "os"

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
	return fileID{size: fi.Size(), mtimeNS: fi.ModTime().UnixNano(), valid: true}, nil
}

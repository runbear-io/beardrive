package remote

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"syscall"
)

/* Compare-and-swap for a directory.

   One process per directory was this backend's contract, and the reason it
   offered no compare-and-swap: a rename is atomic, a read-then-rename across
   two processes is not. But two hub processes on one directory — a second
   `bdrive serve` beside the first, or two in a test standing in for two
   instances on one bucket — append to the same journal, and the second
   write silently erases the first.

   A lock makes it real: PutIf holds an exclusive flock on the directory's
   lock file while it compares and renames, GetVersioned reads under the same
   lock, and an object's version is the hash of its bytes — only ever
   compared, never inspected. Local filesystems only: flock over a network
   mount is whatever the mount says it is, which is why the managed hub
   stores in GCS rather than on a shared disk. */

var _ ConditionalPutter = (*localBackend)(nil)

// Under the temp-file prefix, so the scanner and List already skip it.
const localLockName = ".bdrive-tmp-cas.lock"

func (b *localBackend) GetVersioned(_ context.Context, key string) (io.ReadCloser, Version, error) {
	p, err := b.path(key)
	if err != nil {
		return nil, VersionAbsent, err
	}
	unlock, err := b.lock()
	if err != nil {
		return nil, VersionAbsent, err
	}
	defer unlock()
	data, err := os.ReadFile(p)
	if err != nil {
		return nil, VersionAbsent, err
	}
	return io.NopCloser(bytes.NewReader(data)), versionOf(data), nil
}

func (b *localBackend) PutIf(ctx context.Context, key string, r io.Reader, _ int64, expect Version) (Version, error) {
	p, err := b.path(key)
	if err != nil {
		return VersionAbsent, err
	}
	body, err := io.ReadAll(r)
	if err != nil {
		return VersionAbsent, err
	}
	unlock, err := b.lock()
	if err != nil {
		return VersionAbsent, err
	}
	defer unlock()
	cur := VersionAbsent
	switch data, err := os.ReadFile(p); {
	case err == nil:
		cur = versionOf(data)
	case !errors.Is(err, os.ErrNotExist):
		return VersionAbsent, err
	}
	if cur != expect {
		return VersionAbsent, ErrVersionMismatch
	}
	if err := b.Put(ctx, key, bytes.NewReader(body), int64(len(body))); err != nil {
		return VersionAbsent, err
	}
	return versionOf(body), nil
}

func versionOf(data []byte) Version {
	sum := sha256.Sum256(data)
	return Version(hex.EncodeToString(sum[:]))
}

func (b *localBackend) lock() (func(), error) {
	f, err := os.OpenFile(filepath.Join(b.root, localLockName), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return nil, err
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
	}, nil
}

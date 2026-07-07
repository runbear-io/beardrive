package remote

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// localBackend stores objects in a plain directory. Useful for tests and for
// syncing through any mounted network drive.
type localBackend struct {
	root string
}

func newLocal(root string) (*localBackend, error) {
	if root == "" {
		return nil, fmt.Errorf("file:// remote needs an absolute path")
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, err
	}
	return &localBackend{root: root}, nil
}

func (b *localBackend) path(key string) string {
	return filepath.Join(b.root, filepath.FromSlash(key))
}

func (b *localBackend) Put(_ context.Context, key string, r io.Reader, _ int64) error {
	dst := b.path(key)
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(dst), ".beardrive-tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := io.Copy(tmp, r); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), dst)
}

func (b *localBackend) Get(_ context.Context, key string) (io.ReadCloser, error) {
	return os.Open(b.path(key))
}

func (b *localBackend) List(_ context.Context, prefix string) ([]Object, error) {
	var out []Object
	err := filepath.WalkDir(b.root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if strings.HasPrefix(d.Name(), ".beardrive-tmp-") {
			return nil
		}
		rel, err := filepath.Rel(b.root, p)
		if err != nil {
			return nil
		}
		key := filepath.ToSlash(rel)
		if !strings.HasPrefix(key, prefix) {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		out = append(out, Object{Key: key, Size: info.Size()})
		return nil
	})
	return out, err
}

func (b *localBackend) Exists(_ context.Context, key string) (bool, error) {
	_, err := os.Stat(b.path(key))
	if err == nil {
		return true, nil
	}
	if os.IsNotExist(err) {
		return false, nil
	}
	return false, err
}

func (b *localBackend) Close() error { return nil }

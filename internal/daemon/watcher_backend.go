package daemon

import (
	"errors"
	"path/filepath"
	"sync"
	"syscall"

	"github.com/fsnotify/fsnotify"
)

// opRescanRoot marks a backend event that requires a full scan of its root.
// It is deliberately outside fsnotify's bit range.
const opRescanRoot fsnotify.Op = 1 << 30

var errWatchBackendUnsupported = errors.New("watch backend unsupported")

type rawWatcher interface {
	Add(dir string) error
	ForgetPrefix(prefix string)
	Recursive() bool
	Events() <-chan fsnotify.Event
	Errors() <-chan error
	Close() error
}

type fsnotifyRaw struct{ *fsnotify.Watcher }

func (w *fsnotifyRaw) Add(dir string) error          { return w.Watcher.Add(dir) }
func (w *fsnotifyRaw) ForgetPrefix(string)           {}
func (w *fsnotifyRaw) Recursive() bool               { return false }
func (w *fsnotifyRaw) Events() <-chan fsnotify.Event { return w.Watcher.Events }
func (w *fsnotifyRaw) Errors() <-chan error          { return w.Watcher.Errors }
func (w *fsnotifyRaw) Close() error                  { return w.Watcher.Close() }

func isStructuralWatchErr(err error) bool {
	return errors.Is(err, errWatchBackendUnsupported) ||
		errors.Is(err, syscall.ENOSYS) || errors.Is(err, syscall.EINVAL) ||
		errors.Is(err, syscall.EPERM) || errors.Is(err, syscall.EACCES) ||
		errors.Is(err, syscall.EOPNOTSUPP) || errors.Is(err, syscall.ENOTSUP) ||
		errors.Is(err, syscall.ENODEV) || errors.Is(err, syscall.EXDEV)
}

func isResourceWatchErr(err error) bool {
	return errors.Is(err, syscall.EMFILE) || errors.Is(err, syscall.ENFILE) ||
		errors.Is(err, syscall.ENOSPC) || errors.Is(err, syscall.EOVERFLOW)
}

// recursiveMockWatcher is a small in-memory backend used by daemon tests. It
// models a backend such as fanotify/FSEvents: one root watch observes all
// descendants and no incremental Add calls are needed.
type recursiveMockWatcher struct {
	mu     sync.Mutex
	events chan fsnotify.Event
	errors chan error
	closed bool
}

func newRecursiveMockWatcher() *recursiveMockWatcher {
	return &recursiveMockWatcher{
		events: make(chan fsnotify.Event, 128),
		errors: make(chan error, 16),
	}
}

func (w *recursiveMockWatcher) Add(string) error              { return nil }
func (w *recursiveMockWatcher) ForgetPrefix(string)           {}
func (w *recursiveMockWatcher) Recursive() bool               { return true }
func (w *recursiveMockWatcher) Events() <-chan fsnotify.Event { return w.events }
func (w *recursiveMockWatcher) Errors() <-chan error          { return w.errors }
func (w *recursiveMockWatcher) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return nil
	}
	w.closed = true
	close(w.events)
	close(w.errors)
	return nil
}
func (w *recursiveMockWatcher) emit(ev fsnotify.Event) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return false
	}
	w.events <- ev
	return true
}
func (w *recursiveMockWatcher) emitError(err error) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return false
	}
	w.errors <- err
	return true
}

var (
	recursiveMockMu      sync.RWMutex
	recursiveMockEnabled bool
)

// setLocalWatchRecursiveMockForTest enables the recursive mock backend for
// subsequent watcher construction. It is intentionally package-local and is
// only called from tests.
func setLocalWatchRecursiveMockForTest(enabled bool) {
	recursiveMockMu.Lock()
	recursiveMockEnabled = enabled
	recursiveMockMu.Unlock()
}

func recursiveMockEnabledForTest() bool {
	recursiveMockMu.RLock()
	defer recursiveMockMu.RUnlock()
	return recursiveMockEnabled
}

func cleanWatchPrefix(prefix, name string) bool {
	prefix, name = filepath.Clean(prefix), filepath.Clean(name)
	return name == prefix || (prefix != "." && len(name) > len(prefix) && name[:len(prefix)] == prefix && name[len(prefix)] == filepath.Separator)
}

var _ rawWatcher = (*fsnotifyRaw)(nil)
var _ rawWatcher = (*recursiveMockWatcher)(nil)

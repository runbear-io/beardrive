//go:build !linux

package daemon

import "fmt"

func newFanotifyRawWatcher(roots []string) (rawWatcher, error) {
	return nil, fmt.Errorf("%w: fanotify requires linux", errWatchBackendUnsupported)
}

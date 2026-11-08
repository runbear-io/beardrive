//go:build !(darwin && (arm64 || amd64))

package daemon

import "fmt"

func newFSEventsRawWatcher(roots []string) (rawWatcher, error) {
	return nil, fmt.Errorf("%w: fsevents requires darwin", errWatchBackendUnsupported)
}

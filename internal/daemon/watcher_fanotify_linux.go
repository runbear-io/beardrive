//go:build linux

package daemon

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/fsnotify/fsnotify"
	"golang.org/x/sys/unix"
)

const (
	atHandleFID = 0x200

	fanotifyInfoTypeDfidName = 2
	fanotifyInfoTypeDfid     = 3

	fanotifyMask uint64 = unix.FAN_CREATE |
		unix.FAN_DELETE |
		unix.FAN_MODIFY |
		unix.FAN_CLOSE_WRITE |
		unix.FAN_ATTRIB |
		unix.FAN_MOVED_FROM |
		unix.FAN_MOVED_TO |
		unix.FAN_DELETE_SELF |
		unix.FAN_MOVE_SELF |
		unix.FAN_ONDIR |
		unix.FAN_EVENT_ON_CHILD
)

type fanotifyRaw struct {
	fd     int
	stopR  int
	stopW  int
	stopCh chan struct{}

	events chan fsnotify.Event
	errors chan error

	readerDone chan struct{}
	closeOnce  sync.Once

	mu            sync.RWMutex
	byHandle      map[string]string
	byPath        map[string]string
	unremovedKeys map[string]bool
	rootSet       map[string]bool

	orphanCount    atomic.Uint64
	orphanReported atomic.Bool
}

func newFanotifyRawWatcher(roots []string) (rawWatcher, error) {
	fd, err := unix.FanotifyInit(
		unix.FAN_CLASS_NOTIF|unix.FAN_REPORT_DFID_NAME|unix.FAN_CLOEXEC|unix.FAN_NONBLOCK,
		unix.O_RDONLY,
	)
	if err != nil {
		return nil, err
	}

	var p [2]int
	if err := unix.Pipe2(p[:], unix.O_CLOEXEC|unix.O_NONBLOCK); err != nil {
		_ = unix.Close(fd)
		return nil, err
	}

	w := &fanotifyRaw{
		fd:            fd,
		stopR:         p[0],
		stopW:         p[1],
		stopCh:        make(chan struct{}),
		events:        make(chan fsnotify.Event, 256),
		errors:        make(chan error, 16),
		readerDone:    make(chan struct{}),
		byHandle:      make(map[string]string),
		byPath:        make(map[string]string),
		unremovedKeys: make(map[string]bool),
		rootSet:       make(map[string]bool, len(roots)),
	}
	for _, root := range roots {
		w.rootSet[filepath.Clean(root)] = true
	}

	go w.readLoop()
	for _, root := range roots {
		if err := w.Add(root); err != nil {
			_ = w.Close()
			return nil, err
		}
	}
	return w, nil
}

func fanotifyNotExist(err error) error {
	if errors.Is(err, unix.ENOTDIR) || errors.Is(err, unix.ELOOP) || errors.Is(err, unix.ESTALE) || errors.Is(err, unix.ENOENT) {
		return fmt.Errorf("%w: %v", fs.ErrNotExist, err)
	}
	return err
}

func fanotifyHandleKey(fsid [8]byte, fh unix.FileHandle) string {
	handleBytes := fh.Bytes()
	key := make([]byte, 16+len(handleBytes))
	copy(key[:8], fsid[:])
	binary.LittleEndian.PutUint32(key[8:12], uint32(fh.Size()))
	binary.LittleEndian.PutUint32(key[12:16], uint32(fh.Type()))
	copy(key[16:], handleBytes)
	return string(key)
}

func (w *fanotifyRaw) Add(dir string) error {
	dir = filepath.Clean(dir)
	dfd, err := unix.Open(dir, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return fanotifyNotExist(err)
	}
	defer unix.Close(dfd)

	var stfs unix.Statfs_t
	if err := unix.Fstatfs(dfd, &stfs); err != nil {
		return fanotifyNotExist(err)
	}
	var fsid [8]byte
	binary.LittleEndian.PutUint32(fsid[0:4], uint32(stfs.Fsid.Val[0]))
	binary.LittleEndian.PutUint32(fsid[4:8], uint32(stfs.Fsid.Val[1]))

	fh, _, err := unix.NameToHandleAt(dfd, "", unix.AT_EMPTY_PATH|atHandleFID)
	if errors.Is(err, unix.EINVAL) {
		fh, _, err = unix.NameToHandleAt(dfd, "", unix.AT_EMPTY_PATH)
	}
	if err != nil {
		return fanotifyNotExist(err)
	}
	key := fanotifyHandleKey(fsid, fh)

	w.mu.Lock()
	prevPath, hadPrevHandle := w.byHandle[key]
	prevKey, hadPrevPath := w.byPath[dir]
	if hadPrevPath && prevKey != key {
		delete(w.byHandle, prevKey)
	}
	if hadPrevHandle && prevPath != dir {
		delete(w.byPath, prevPath)
	}
	wasUnremoved := w.unremovedKeys[key]
	delete(w.unremovedKeys, key)
	w.byHandle[key] = dir
	w.byPath[dir] = key
	w.mu.Unlock()
	if err := unix.FanotifyMark(w.fd, unix.FAN_MARK_ADD|unix.FAN_MARK_ONLYDIR, fanotifyMask, dfd, ""); err != nil {
		err = fanotifyNotExist(err)
		w.mu.Lock()
		if current, ok := w.byHandle[key]; ok && current == dir {
			if hadPrevHandle {
				w.byHandle[key] = prevPath
			} else {
				delete(w.byHandle, key)
			}
		}
		if current, ok := w.byPath[dir]; ok && current == key {
			if hadPrevPath {
				w.byPath[dir] = prevKey
			} else {
				delete(w.byPath, dir)
			}
		}
		if hadPrevPath && prevKey != key {
			w.byHandle[prevKey] = dir
		}
		if hadPrevHandle && prevPath != dir {
			w.byPath[prevPath] = key
		}
		if wasUnremoved {
			w.unremovedKeys[key] = true
		}
		w.mu.Unlock()
		return err
	}
	return nil
}

func (w *fanotifyRaw) ForgetPrefix(prefix string) {
	prefix = filepath.Clean(prefix)
	type forgotten struct {
		path string
		key  string
	}
	var removed []forgotten
	w.mu.Lock()
	for p, k := range w.byPath {
		if p != prefix && !strings.HasPrefix(p, prefix+string(filepath.Separator)) {
			continue
		}
		delete(w.byPath, p)
		if w.byHandle[k] == p {
			delete(w.byHandle, k)
			removed = append(removed, forgotten{path: p, key: k})
		}
	}
	w.mu.Unlock()
	if len(removed) == 0 {
		return
	}
	var unremoved []string
	for _, item := range removed {
		if err := unix.FanotifyMark(w.fd, unix.FAN_MARK_REMOVE|unix.FAN_MARK_ONLYDIR, fanotifyMask, unix.AT_FDCWD, item.path); err != nil {
			if !errors.Is(err, unix.ENOENT) && !errors.Is(err, unix.EINVAL) {
				unremoved = append(unremoved, item.key)
			} else if _, serr := os.Lstat(item.path); serr != nil {
				unremoved = append(unremoved, item.key)
			}
		}
	}
	if len(unremoved) == 0 {
		return
	}
	w.mu.Lock()
	for _, k := range unremoved {
		if _, active := w.byHandle[k]; !active {
			w.unremovedKeys[k] = true
		}
	}
	needReclaim := len(w.unremovedKeys) >= 256
	w.mu.Unlock()
	if needReclaim && w.orphanReported.CompareAndSwap(false, true) {
		w.sendError(fmt.Errorf("%w: fanotify mark reclamation threshold", fs.ErrNotExist))
	}
}

func (w *fanotifyRaw) Recursive() bool { return false }

func (w *fanotifyRaw) Events() <-chan fsnotify.Event { return w.events }

func (w *fanotifyRaw) Errors() <-chan error { return w.errors }

func (w *fanotifyRaw) sendError(err error) bool {
	select {
	case w.errors <- err:
		return true
	case <-w.stopCh:
		return false
	}
}

func (w *fanotifyRaw) sendEvent(event fsnotify.Event) bool {
	select {
	case w.events <- event:
		return true
	case <-w.stopCh:
		return false
	}
}

func (w *fanotifyRaw) readLoop() {
	defer close(w.readerDone)
	defer unix.Close(w.fd)
	defer unix.Close(w.stopR)
	defer unix.Close(w.stopW)
	defer close(w.events)
	defer close(w.errors)

	buf := make([]byte, 64*1024)
	pfds := []unix.PollFd{
		{Fd: int32(w.fd), Events: unix.POLLIN},
		{Fd: int32(w.stopR), Events: unix.POLLIN},
	}
	for {
		select {
		case <-w.stopCh:
			return
		default:
		}
		pfds[0].Revents = 0
		pfds[1].Revents = 0
		_, err := unix.Poll(pfds, -1)
		if err == unix.EINTR || err == unix.EAGAIN {
			continue
		}
		select {
		case <-w.stopCh:
			return
		default:
		}
		if pfds[1].Revents != 0 {
			return
		}
		if err != nil {
			w.sendError(err)
			return
		}
		if pfds[0].Revents&(unix.POLLERR|unix.POLLNVAL) != 0 {
			w.sendError(errors.New("fanotify poll error"))
			return
		}
		if pfds[0].Revents&unix.POLLIN == 0 {
			continue
		}
		n, err := unix.Read(w.fd, buf)
		if err == unix.EINTR || err == unix.EAGAIN {
			continue
		}
		if err != nil {
			w.sendError(err)
			return
		}
		if n == 0 {
			continue
		}
		if !w.parseBuffer(buf[:n]) {
			return
		}
	}
}

func (w *fanotifyRaw) parseBuffer(buf []byte) bool {
	for off := 0; off < len(buf); {
		if len(buf)-off < unix.FAN_EVENT_METADATA_LEN {
			return true
		}
		eventLen := binary.LittleEndian.Uint32(buf[off : off+4])
		vers := buf[off+4]
		metadataLen := int(binary.LittleEndian.Uint16(buf[off+6 : off+8]))
		if vers != unix.FANOTIFY_METADATA_VERSION {
			w.sendError(fmt.Errorf("fanotify metadata version %d", vers))
			return false
		}
		if metadataLen < unix.FAN_EVENT_METADATA_LEN || eventLen < uint32(metadataLen) {
			w.sendError(errors.New("fanotify invalid metadata length"))
			return false
		}
		if eventLen > uint32(len(buf)-off) {
			w.sendError(errors.New("fanotify truncated event"))
			return false
		}
		eventEnd := off + int(eventLen)
		mask := binary.LittleEndian.Uint64(buf[off+8 : off+16])
		fd := int32(binary.LittleEndian.Uint32(buf[off+16 : off+20]))
		if fd >= 0 {
			_ = unix.Close(int(fd))
		}
		if mask&unix.FAN_Q_OVERFLOW != 0 {
			if !w.sendError(fsnotify.ErrEventOverflow) {
				return false
			}
		}

		for infoOff := off + metadataLen; infoOff < eventEnd; {
			if eventEnd-infoOff < 4 {
				break
			}
			typeID := buf[infoOff]
			hdrLen := int(binary.LittleEndian.Uint16(buf[infoOff+2 : infoOff+4]))
			if hdrLen < 4 || infoOff+hdrLen > eventEnd {
				break
			}
			infoEnd := infoOff + hdrLen
			if typeID == fanotifyInfoTypeDfidName || typeID == fanotifyInfoTypeDfid {
				if hdrLen >= 20 {
					var fsid [8]byte
					copy(fsid[:], buf[infoOff+4:infoOff+12])
					handleSize := binary.LittleEndian.Uint32(buf[infoOff+12 : infoOff+16])
					handleType := int32(binary.LittleEndian.Uint32(buf[infoOff+16 : infoOff+20]))
					handleEnd := infoOff + 20 + int(handleSize)
					if handleEnd <= infoEnd {
						keyBytes := make([]byte, 16+int(handleSize))
						copy(keyBytes[:8], fsid[:])
						binary.LittleEndian.PutUint32(keyBytes[8:12], handleSize)
						binary.LittleEndian.PutUint32(keyBytes[12:16], uint32(handleType))
						copy(keyBytes[16:], buf[infoOff+20:handleEnd])
						name := "."
						if typeID == fanotifyInfoTypeDfidName {
							nameBytes := buf[handleEnd:infoEnd]
							if nul := bytes.IndexByte(nameBytes, 0); nul >= 0 {
								nameBytes = nameBytes[:nul]
							}
							name = string(nameBytes)
						}
						if !w.emitInfo(string(keyBytes), name, mask) {
							return false
						}
					}
				}
			}
			infoOff += hdrLen
		}
		off = eventEnd
	}
	return true
}

func (w *fanotifyRaw) emitInfo(key, name string, mask uint64) bool {
	w.mu.RLock()
	dirPath, ok := w.byHandle[key]
	isRoot := ok && w.rootSet[dirPath]
	w.mu.RUnlock()
	if !ok {
		if w.orphanCount.Add(1) >= 4096 && w.orphanReported.CompareAndSwap(false, true) {
			if !w.sendError(fmt.Errorf("%w: fanotify orphan threshold", fs.ErrNotExist)) {
				return false
			}
		}
		return true
	}
	if name == "." || name == "" {
		if !isRoot {
			return true
		}
	}
	fullPath := dirPath
	if name != "." && name != "" {
		fullPath = filepath.Join(dirPath, name)
	}

	var op fsnotify.Op
	if mask&(unix.FAN_CREATE|unix.FAN_MOVED_TO) != 0 {
		op |= fsnotify.Create
	}
	if mask&(unix.FAN_DELETE|unix.FAN_DELETE_SELF) != 0 {
		op |= fsnotify.Remove
	}
	if mask&(unix.FAN_MOVED_FROM|unix.FAN_MOVE_SELF) != 0 {
		op |= fsnotify.Rename
	}
	if mask&(unix.FAN_MODIFY|unix.FAN_CLOSE_WRITE) != 0 {
		op |= fsnotify.Write
	}
	if mask&unix.FAN_ATTRIB != 0 {
		op |= fsnotify.Chmod
	}
	if op == 0 {
		return true
	}

	if mask&unix.FAN_ONDIR != 0 && name != "." && name != "" && op&fsnotify.Create != 0 && op&(fsnotify.Remove|fsnotify.Rename) != 0 {
		if _, err := os.Lstat(fullPath); err == nil {
			if !w.sendEvent(fsnotify.Event{Name: fullPath, Op: fsnotify.Remove}) {
				return false
			}
			return w.sendEvent(fsnotify.Event{Name: fullPath, Op: fsnotify.Create})
		} else if os.IsNotExist(err) {
			return w.sendEvent(fsnotify.Event{Name: fullPath, Op: fsnotify.Remove})
		}
	}
	return w.sendEvent(fsnotify.Event{Name: fullPath, Op: op})
}

func (w *fanotifyRaw) Close() error {
	var err error
	w.closeOnce.Do(func() {
		w.mu.Lock()
		w.mu.Unlock()
		close(w.stopCh)
		_, err = unix.Write(w.stopW, []byte{1})
		if err == unix.EPIPE || err == unix.EBADF {
			err = nil
		}
		<-w.readerDone
	})
	return err
}

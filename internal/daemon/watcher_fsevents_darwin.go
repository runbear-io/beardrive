//go:build darwin && (arm64 || amd64)

package daemon

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"syscall"
	"unsafe"

	"github.com/fsnotify/fsnotify"
)

// The FSEvents API is not exposed by a pure-Go package. These declarations use
// the same dynamic-linking mechanism as syscall on Darwin, while the assembly
// trampolines preserve the platform ABI for callbacks and floating-point
// arguments.
//
//go:linkname syscall_syscall syscall.syscall
func syscall_syscall(fn, a1, a2, a3 uintptr) (r1, r2 uintptr, err syscall.Errno)

//go:linkname syscall_syscall6 syscall.syscall6
func syscall_syscall6(fn, a1, a2, a3, a4, a5, a6 uintptr) (r1, r2 uintptr, err syscall.Errno)

//go:cgo_import_dynamic libc_CFStringCreateWithBytes CFStringCreateWithBytes "/System/Library/Frameworks/CoreFoundation.framework/Versions/A/CoreFoundation"
//go:cgo_import_dynamic libc_CFArrayCreate CFArrayCreate "/System/Library/Frameworks/CoreFoundation.framework/Versions/A/CoreFoundation"
//go:cgo_import_dynamic libc_CFArrayGetValueAtIndex CFArrayGetValueAtIndex "/System/Library/Frameworks/CoreFoundation.framework/Versions/A/CoreFoundation"
//go:cgo_import_dynamic libc_CFStringCreateExternalRepresentation CFStringCreateExternalRepresentation "/System/Library/Frameworks/CoreFoundation.framework/Versions/A/CoreFoundation"
//go:cgo_import_dynamic libc_CFDataGetLength CFDataGetLength "/System/Library/Frameworks/CoreFoundation.framework/Versions/A/CoreFoundation"
//go:cgo_import_dynamic libc_CFDataGetBytes CFDataGetBytes "/System/Library/Frameworks/CoreFoundation.framework/Versions/A/CoreFoundation"
//go:cgo_import_dynamic libc_CFRelease CFRelease "/System/Library/Frameworks/CoreFoundation.framework/Versions/A/CoreFoundation"
//go:cgo_import_dynamic libc_CFRetain CFRetain "/System/Library/Frameworks/CoreFoundation.framework/Versions/A/CoreFoundation"
//go:cgo_import_dynamic libc_CFRunLoopGetCurrent CFRunLoopGetCurrent "/System/Library/Frameworks/CoreFoundation.framework/Versions/A/CoreFoundation"
//go:cgo_import_dynamic libc_CFRunLoopRunInMode CFRunLoopRunInMode "/System/Library/Frameworks/CoreFoundation.framework/Versions/A/CoreFoundation"
//go:cgo_import_dynamic libc_CFRunLoopStop CFRunLoopStop "/System/Library/Frameworks/CoreFoundation.framework/Versions/A/CoreFoundation"
//go:cgo_import_dynamic libc_CFRunLoopSourceCreate CFRunLoopSourceCreate "/System/Library/Frameworks/CoreFoundation.framework/Versions/A/CoreFoundation"
//go:cgo_import_dynamic libc_CFRunLoopAddSource CFRunLoopAddSource "/System/Library/Frameworks/CoreFoundation.framework/Versions/A/CoreFoundation"
//go:cgo_import_dynamic libc_CFRunLoopRemoveSource CFRunLoopRemoveSource "/System/Library/Frameworks/CoreFoundation.framework/Versions/A/CoreFoundation"
//go:cgo_import_dynamic libc_CFRunLoopSourceSignal CFRunLoopSourceSignal "/System/Library/Frameworks/CoreFoundation.framework/Versions/A/CoreFoundation"
//go:cgo_import_dynamic libc_CFRunLoopWakeUp CFRunLoopWakeUp "/System/Library/Frameworks/CoreFoundation.framework/Versions/A/CoreFoundation"
//go:cgo_import_dynamic libc_FSEventsGetCurrentEventId FSEventsGetCurrentEventId "/System/Library/Frameworks/CoreServices.framework/Versions/A/CoreServices"
//go:cgo_import_dynamic libc_FSEventStreamCreate FSEventStreamCreate "/System/Library/Frameworks/CoreServices.framework/Versions/A/CoreServices"
//go:cgo_import_dynamic libc_FSEventStreamScheduleWithRunLoop FSEventStreamScheduleWithRunLoop "/System/Library/Frameworks/CoreServices.framework/Versions/A/CoreServices"
//go:cgo_import_dynamic libc_FSEventStreamUnscheduleFromRunLoop FSEventStreamUnscheduleFromRunLoop "/System/Library/Frameworks/CoreServices.framework/Versions/A/CoreServices"
//go:cgo_import_dynamic libc_FSEventStreamStart FSEventStreamStart "/System/Library/Frameworks/CoreServices.framework/Versions/A/CoreServices"
//go:cgo_import_dynamic libc_FSEventStreamStop FSEventStreamStop "/System/Library/Frameworks/CoreServices.framework/Versions/A/CoreServices"
//go:cgo_import_dynamic libc_FSEventStreamInvalidate FSEventStreamInvalidate "/System/Library/Frameworks/CoreServices.framework/Versions/A/CoreServices"
//go:cgo_import_dynamic libc_FSEventStreamRelease FSEventStreamRelease "/System/Library/Frameworks/CoreServices.framework/Versions/A/CoreServices"

type rootPair struct {
	real   string
	caller string
}

type fsEventStreamContext struct {
	version         int64
	info            uintptr
	retain          uintptr
	release         uintptr
	copyDescription uintptr
}

type cbSlot struct {
	count    uintptr
	paths    uintptr
	flags    [256]uint32
	overflow uint32
}

type createArgs struct {
	alloc     uintptr
	cb        uintptr
	ctx       uintptr
	paths     uintptr
	sinceWhen uint64
	latency   float64
	flags     uint32
	_pad      uint32
}

type runInModeArgs struct {
	mode                     uintptr
	seconds                  float64
	returnAfterSourceHandled uint32
	_pad                     uint32
}

// CFRunLoopSourceContext is laid out according to CoreFoundation's C ABI. The
// source only needs its perform callback; all other callbacks and info are nil.
type runLoopSourceContext struct {
	version         int64
	info            uintptr
	retain          uintptr
	release         uintptr
	copyDescription uintptr
	equal           uintptr
	hash            uintptr
	schedule        uintptr
	cancel          uintptr
	perform         uintptr
}

type trampTable struct {
	cfStrCreate, cfArrCreate, cfArrGet, cfStrExtRep, cfDataLen, cfDataPtr, cfRelease uintptr
	rlGetCurrent, rlRunInMode, rlStop                                                uintptr
	rlSourceCreate, rlAddSource, rlRemoveSource, rlSourceSignal, rlWakeUp            uintptr
	fsCurrentID, fsCreate, fsScheduleRL, fsUnscheduleRL, fsStart, fsStop, fsInval    uintptr
	fsRelease                                                                        uintptr
}

func trampolineAddrs(out *trampTable)
func callbackAddr() uintptr
func sourceNoopAddr() uintptr

var tramps = func() (t trampTable) { trampolineAddrs(&t); return }()

const (
	kCFStringEncodingUTF8 = 0x08000100
	fseventsLatency       = 0.015
	fseventsFlags         = 0x00000001 | 0x00000002 | 0x00000004 | 0x00000010
)

const (
	kFSEventStreamEventFlagMustScanSubDirs   = 0x00000001
	kFSEventStreamEventFlagUserDropped       = 0x00000002
	kFSEventStreamEventFlagKernelDropped     = 0x00000004
	kFSEventStreamEventFlagEventIdsWrapped   = 0x00000008
	kFSEventStreamEventFlagHistoryDone       = 0x00000010
	kFSEventStreamEventFlagRootChanged       = 0x00000020
	kFSEventStreamEventFlagItemCreated       = 0x00000100
	kFSEventStreamEventFlagItemRemoved       = 0x00000200
	kFSEventStreamEventFlagItemInodeMetaMod  = 0x00000400
	kFSEventStreamEventFlagItemRenamed       = 0x00000800
	kFSEventStreamEventFlagItemModified      = 0x00001000
	kFSEventStreamEventFlagItemFinderInfoMod = 0x00002000
	kFSEventStreamEventFlagItemChangeOwner   = 0x00004000
	kFSEventStreamEventFlagItemXattrMod      = 0x00008000
)

var fseventsDropFlags = uint32(kFSEventStreamEventFlagUserDropped | kFSEventStreamEventFlagKernelDropped | kFSEventStreamEventFlagEventIdsWrapped)
var fseventsMetaFlags = uint32(kFSEventStreamEventFlagItemInodeMetaMod | kFSEventStreamEventFlagItemFinderInfoMod | kFSEventStreamEventFlagItemChangeOwner | kFSEventStreamEventFlagItemXattrMod)

type fseventsRaw struct {
	roots  []rootPair
	events chan fsnotify.Event
	errors chan error

	stopCh     chan struct{}
	readerDone chan struct{}
	closeOnce  sync.Once

	rl         uintptr
	stopSource uintptr
	cfArr      uintptr
	modeStr    uintptr
	stream     uintptr
	slot       cbSlot
}

func (w *fseventsRaw) Add(string) error              { return nil }
func (w *fseventsRaw) ForgetPrefix(string)           {}
func (w *fseventsRaw) Recursive() bool               { return true }
func (w *fseventsRaw) Events() <-chan fsnotify.Event { return w.events }
func (w *fseventsRaw) Errors() <-chan error          { return w.errors }

func (w *fseventsRaw) Close() error {
	w.closeOnce.Do(func() {
		close(w.stopCh)
		if w.stopSource != 0 {
			syscall_syscall(tramps.rlSourceSignal, w.stopSource, 0, 0)
		}
		if w.rl != 0 {
			syscall_syscall(tramps.rlWakeUp, w.rl, 0, 0)
			syscall_syscall(tramps.rlStop, w.rl, 0, 0)
		}
		<-w.readerDone
	})
	return nil
}

func cfStringCreate(s string) uintptr {
	b := []byte(s)
	var p unsafe.Pointer
	if len(b) != 0 {
		p = unsafe.Pointer(unsafe.SliceData(b))
	}
	ref, _, _ := syscall_syscall6(tramps.cfStrCreate, 0, uintptr(p), uintptr(len(b)), kCFStringEncodingUTF8, 0, 0)
	runtime.KeepAlive(b)
	return ref
}

func cfStringToGo(cfStr uintptr) string {
	data, _, _ := syscall_syscall6(tramps.cfStrExtRep, 0, cfStr, kCFStringEncodingUTF8, 0, 0, 0)
	if data == 0 {
		return ""
	}
	defer syscall_syscall(tramps.cfRelease, data, 0, 0)
	length, _, _ := syscall_syscall(tramps.cfDataLen, data, 0, 0)
	if length == 0 {
		return ""
	}
	buf := make([]byte, int(length))
	p := unsafe.Pointer(unsafe.SliceData(buf))
	syscall_syscall6(tramps.cfDataPtr, data, 0, length, uintptr(p), 0, 0)
	runtime.KeepAlive(buf)
	return string(buf)
}

func mapToCaller(roots []rootPair, rawPath string) string {
	rawPath = filepath.Clean(rawPath)
	for _, pair := range roots {
		if rawPath != pair.real && !strings.HasPrefix(rawPath, pair.real+string(filepath.Separator)) {
			continue
		}
		suffix := strings.TrimPrefix(rawPath, pair.real)
		if suffix == "" {
			return pair.caller
		}
		return filepath.Join(pair.caller, strings.TrimPrefix(suffix, string(filepath.Separator)))
	}
	return rawPath
}

func newFSEventsRawWatcher(roots []string) (rawWatcher, error) {
	if len(roots) == 0 {
		return nil, errors.New("fsevents: no roots")
	}
	pairs := make([]rootPair, 0, len(roots))
	for _, root := range roots {
		fi, err := os.Stat(root)
		if err != nil {
			return nil, err
		}
		if !fi.IsDir() {
			return nil, syscall.ENOTDIR
		}
		callerRoot := filepath.Clean(root)
		realRoot, err := filepath.EvalSymlinks(root)
		if err != nil {
			realRoot = callerRoot
		}
		pairs = append(pairs, rootPair{real: filepath.Clean(realRoot), caller: callerRoot})
	}
	sort.SliceStable(pairs, func(i, j int) bool { return len(pairs[i].real) > len(pairs[j].real) })

	w := &fseventsRaw{
		roots:      pairs,
		events:     make(chan fsnotify.Event, 256),
		errors:     make(chan error, 1),
		stopCh:     make(chan struct{}),
		readerDone: make(chan struct{}),
	}
	readyCh := make(chan error, 1)
	go w.run(readyCh)
	if err := <-readyCh; err != nil {
		return nil, err
	}
	return w, nil
}

func (w *fseventsRaw) run(readyCh chan<- error) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	defer close(w.readerDone)
	defer close(w.events)
	defer close(w.errors)

	var streamStarted, streamScheduled bool
	defer func() {
		if w.stream != 0 {
			if streamStarted {
				syscall_syscall(tramps.fsStop, w.stream, 0, 0)
			}
			if streamScheduled {
				syscall_syscall(tramps.fsUnscheduleRL, w.stream, w.rl, w.modeStr)
			}
			syscall_syscall(tramps.fsInval, w.stream, 0, 0)
			syscall_syscall(tramps.fsRelease, w.stream, 0, 0)
		}
		if w.stopSource != 0 && w.rl != 0 {
			syscall_syscall(tramps.rlRemoveSource, w.rl, w.stopSource, w.modeStr)
			syscall_syscall(tramps.cfRelease, w.stopSource, 0, 0)
		}
		if w.slot.paths != 0 {
			syscall_syscall(tramps.cfRelease, w.slot.paths, 0, 0)
			w.slot.paths = 0
			w.slot.count = 0
		}
		if w.cfArr != 0 {
			syscall_syscall(tramps.cfRelease, w.cfArr, 0, 0)
		}
		if w.modeStr != 0 {
			syscall_syscall(tramps.cfRelease, w.modeStr, 0, 0)
		}
	}()

	w.rl, _, _ = syscall_syscall(tramps.rlGetCurrent, 0, 0, 0)
	w.modeStr = cfStringCreate("kCFRunLoopDefaultMode")
	if w.rl == 0 || w.modeStr == 0 {
		readyCh <- errors.New("fsevents: create run loop failed")
		return
	}

	sourceCtx := runLoopSourceContext{version: 0, perform: sourceNoopAddr()}
	w.stopSource, _, _ = syscall_syscall6(tramps.rlSourceCreate, 0, 0, uintptr(unsafe.Pointer(&sourceCtx)), 0, 0, 0)
	if w.stopSource == 0 {
		readyCh <- errors.New("fsevents: create stop source failed")
		return
	}
	syscall_syscall(tramps.rlAddSource, w.rl, w.stopSource, w.modeStr)
	runtime.KeepAlive(&sourceCtx)

	cfRoots := make([]uintptr, len(w.roots))
	for i, pair := range w.roots {
		cfRoots[i] = cfStringCreate(pair.real)
		if cfRoots[i] == 0 {
			readyCh <- fmt.Errorf("fsevents: create root string %q", pair.real)
			return
		}
	}
	values := unsafe.Pointer(nil)
	if len(cfRoots) > 0 {
		values = unsafe.Pointer(unsafe.SliceData(cfRoots))
	}
	w.cfArr, _, _ = syscall_syscall6(tramps.cfArrCreate, 0, uintptr(values), uintptr(len(cfRoots)), 0, 0, 0)
	runtime.KeepAlive(cfRoots)
	defer func() {
		for _, root := range cfRoots {
			if root != 0 {
				syscall_syscall(tramps.cfRelease, root, 0, 0)
			}
		}
	}()
	if w.cfArr == 0 {
		readyCh <- errors.New("fsevents: create root array failed")
		return
	}

	ctx := fsEventStreamContext{version: 0, info: uintptr(unsafe.Pointer(&w.slot))}
	sinceWhen, _, _ := syscall_syscall(tramps.fsCurrentID, 0, 0, 0)
	create := createArgs{
		cb:        callbackAddr(),
		ctx:       uintptr(unsafe.Pointer(&ctx)),
		paths:     w.cfArr,
		sinceWhen: uint64(sinceWhen),
		latency:   fseventsLatency,
		flags:     fseventsFlags,
	}
	w.stream, _, _ = syscall_syscall(tramps.fsCreate, uintptr(unsafe.Pointer(&create)), 0, 0)
	runtime.KeepAlive(&create)
	runtime.KeepAlive(&ctx)
	if w.stream == 0 {
		readyCh <- errors.New("fsevents: create stream failed")
		return
	}
	syscall_syscall(tramps.fsScheduleRL, w.stream, w.rl, w.modeStr)
	streamScheduled = true
	ok, _, _ := syscall_syscall(tramps.fsStart, w.stream, 0, 0)
	if ok == 0 {
		readyCh <- errors.New("fsevents: start stream failed")
		return
	}
	streamStarted = true
	readyCh <- nil

	runArgs := runInModeArgs{mode: w.modeStr, seconds: 60.0, returnAfterSourceHandled: 1}
	for {
		select {
		case <-w.stopCh:
			return
		default:
		}
		syscall_syscall(tramps.rlRunInMode, uintptr(unsafe.Pointer(&runArgs)), 0, 0)
		runtime.KeepAlive(&runArgs)
		select {
		case <-w.stopCh:
			return
		default:
		}

		if w.slot.overflow != 0 {
			w.slot.overflow = 0
			if w.slot.paths != 0 {
				syscall_syscall(tramps.cfRelease, w.slot.paths, 0, 0)
				w.slot.paths = 0
			}
			w.slot.count = 0
			w.emit(fsnotify.Event{Name: "", Op: opRescanRoot})
		} else if w.slot.count > 0 {
			type pendingEvent struct {
				path  string
				flags uint32
			}
			pending := make([]pendingEvent, 0, w.slot.count)
			for i := range w.slot.count {
				ref, _, _ := syscall_syscall(tramps.cfArrGet, w.slot.paths, i, 0)
				pending = append(pending, pendingEvent{path: cfStringToGo(ref), flags: w.slot.flags[i]})
			}
			if w.slot.paths != 0 {
				syscall_syscall(tramps.cfRelease, w.slot.paths, 0, 0)
			}
			w.slot.count = 0
			w.slot.paths = 0
			for _, item := range pending {
				w.emitFSEvent(item.path, item.flags)
			}
		} else if w.slot.paths != 0 {
			syscall_syscall(tramps.cfRelease, w.slot.paths, 0, 0)
			w.slot.paths = 0
		}
		select {
		case <-w.stopCh:
			return
		default:
		}
	}
}

func (w *fseventsRaw) emit(event fsnotify.Event) {
	select {
	case w.events <- event:
	case <-w.stopCh:
	}
}

func (w *fseventsRaw) emitFSEvent(rawPath string, flags uint32) {
	mappedPath := mapToCaller(w.roots, rawPath)
	if flags&fseventsDropFlags != 0 {
		w.emit(fsnotify.Event{Name: "", Op: opRescanRoot})
	}
	if flags&kFSEventStreamEventFlagRootChanged != 0 {
		matched := false
		for _, pair := range w.roots {
			if mappedPath == pair.caller || strings.HasPrefix(pair.caller, mappedPath+string(filepath.Separator)) {
				w.emit(fsnotify.Event{Name: pair.caller, Op: fsnotify.Remove | fsnotify.Rename})
				matched = true
			}
		}
		if !matched {
			for _, pair := range w.roots {
				w.emit(fsnotify.Event{Name: pair.caller, Op: fsnotify.Remove | fsnotify.Rename})
			}
		}
	}
	if flags&kFSEventStreamEventFlagMustScanSubDirs != 0 {
		w.emit(fsnotify.Event{Name: mappedPath, Op: opRescanRoot})
	}
	if flags&kFSEventStreamEventFlagHistoryDone != 0 {
		return
	}
	_, err := os.Lstat(mappedPath)
	var op fsnotify.Op
	if err == nil {
		op = fsnotify.Create | fsnotify.Write
		if flags&kFSEventStreamEventFlagItemRemoved != 0 {
			op |= fsnotify.Remove
		}
		if flags&kFSEventStreamEventFlagItemRenamed != 0 {
			op |= fsnotify.Rename
		}
		if flags&fseventsMetaFlags != 0 {
			op |= fsnotify.Chmod
		}
	} else {
		if flags&kFSEventStreamEventFlagItemRenamed != 0 && flags&kFSEventStreamEventFlagItemRemoved == 0 {
			op = fsnotify.Rename
		} else {
			op = fsnotify.Remove
		}
	}
	w.emit(fsnotify.Event{Name: mappedPath, Op: op})
}

func libc_CFStringCreateWithBytes_trampoline()
func libc_CFArrayCreate_trampoline()
func libc_CFArrayGetValueAtIndex_trampoline()
func libc_CFStringCreateExternalRepresentation_trampoline()
func libc_CFDataGetLength_trampoline()
func libc_CFDataGetBytes_trampoline()
func libc_CFRelease_trampoline()
func libc_CFRunLoopGetCurrent_trampoline()
func libc_CFRunLoopRunInMode_trampoline()
func libc_CFRunLoopStop_trampoline()
func libc_CFRunLoopSourceCreate_trampoline()
func libc_CFRunLoopAddSource_trampoline()
func libc_CFRunLoopRemoveSource_trampoline()
func libc_CFRunLoopSourceSignal_trampoline()
func libc_CFRunLoopWakeUp_trampoline()
func libc_FSEventsGetCurrentEventId_trampoline()
func libc_FSEventStreamScheduleWithRunLoop_trampoline()
func libc_FSEventStreamUnscheduleFromRunLoop_trampoline()
func libc_FSEventStreamStart_trampoline()
func libc_FSEventStreamStop_trampoline()
func libc_FSEventStreamInvalidate_trampoline()
func libc_FSEventStreamRelease_trampoline()
func libc_FSEventStreamCreate_trampoline()
func fseventsCallbackTrampoline()
func fseventsSourceNoopTrampoline()

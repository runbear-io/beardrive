//go:build darwin && arm64

#include "textflag.h"

TEXT ·libc_CFStringCreateWithBytes_trampoline(SB),NOSPLIT,$0-0
	JMP libc_CFStringCreateWithBytes(SB)
TEXT ·libc_CFArrayCreate_trampoline(SB),NOSPLIT,$0-0
	JMP libc_CFArrayCreate(SB)
TEXT ·libc_CFArrayGetValueAtIndex_trampoline(SB),NOSPLIT,$0-0
	JMP libc_CFArrayGetValueAtIndex(SB)
TEXT ·libc_CFStringCreateExternalRepresentation_trampoline(SB),NOSPLIT,$0-0
	JMP libc_CFStringCreateExternalRepresentation(SB)
TEXT ·libc_CFDataGetLength_trampoline(SB),NOSPLIT,$0-0
	JMP libc_CFDataGetLength(SB)
TEXT ·libc_CFDataGetBytes_trampoline(SB),NOSPLIT,$0-0
	JMP libc_CFDataGetBytes(SB)
TEXT ·libc_CFRelease_trampoline(SB),NOSPLIT,$0-0
	JMP libc_CFRelease(SB)
TEXT ·libc_CFRunLoopGetCurrent_trampoline(SB),NOSPLIT,$0-0
	JMP libc_CFRunLoopGetCurrent(SB)
TEXT ·libc_CFRunLoopRunInMode_trampoline(SB),NOSPLIT,$0-0
	// R0 points to struct { mode uintptr; seconds float64; returnAfterSourceHandled uint32 }
	FMOVD 8(R0), F0
	MOVWU 16(R0), R1
	MOVD 0(R0), R0
	JMP libc_CFRunLoopRunInMode(SB)
TEXT ·libc_CFRunLoopStop_trampoline(SB),NOSPLIT,$0-0
	JMP libc_CFRunLoopStop(SB)
TEXT ·libc_CFRunLoopSourceCreate_trampoline(SB),NOSPLIT,$0-0
	JMP libc_CFRunLoopSourceCreate(SB)
TEXT ·libc_CFRunLoopAddSource_trampoline(SB),NOSPLIT,$0-0
	JMP libc_CFRunLoopAddSource(SB)
TEXT ·libc_CFRunLoopRemoveSource_trampoline(SB),NOSPLIT,$0-0
	JMP libc_CFRunLoopRemoveSource(SB)
TEXT ·libc_CFRunLoopSourceSignal_trampoline(SB),NOSPLIT,$0-0
	JMP libc_CFRunLoopSourceSignal(SB)
TEXT ·libc_CFRunLoopWakeUp_trampoline(SB),NOSPLIT,$0-0
	JMP libc_CFRunLoopWakeUp(SB)
TEXT ·libc_FSEventsGetCurrentEventId_trampoline(SB),NOSPLIT,$0-0
	JMP libc_FSEventsGetCurrentEventId(SB)
TEXT ·libc_FSEventStreamScheduleWithRunLoop_trampoline(SB),NOSPLIT,$0-0
	JMP libc_FSEventStreamScheduleWithRunLoop(SB)
TEXT ·libc_FSEventStreamUnscheduleFromRunLoop_trampoline(SB),NOSPLIT,$0-0
	JMP libc_FSEventStreamUnscheduleFromRunLoop(SB)
TEXT ·libc_FSEventStreamStart_trampoline(SB),NOSPLIT,$0-0
	JMP libc_FSEventStreamStart(SB)
TEXT ·libc_FSEventStreamStop_trampoline(SB),NOSPLIT,$0-0
	JMP libc_FSEventStreamStop(SB)
TEXT ·libc_FSEventStreamInvalidate_trampoline(SB),NOSPLIT,$0-0
	JMP libc_FSEventStreamInvalidate(SB)
TEXT ·libc_FSEventStreamRelease_trampoline(SB),NOSPLIT,$0-0
	JMP libc_FSEventStreamRelease(SB)

TEXT ·libc_FSEventStreamCreate_trampoline(SB),NOSPLIT,$0-0
	// R0 points to struct { alloc, callback, context, paths, sinceWhen, latency, flags }
	MOVD 8(R0), R1
	MOVD 16(R0), R2
	MOVD 24(R0), R3
	MOVD 32(R0), R4
	FMOVD 40(R0), F0
	MOVWU 48(R0), R5
	MOVD 0(R0), R0
	JMP libc_FSEventStreamCreate(SB)

// Runs synchronously inside CFRunLoopRunInMode on our own LockOSThread Go M.
// R1 = info = pointer to cbSlot { count uintptr; paths uintptr; flags [256]uint32; overflow uint32 }
// R2 = numEvents, R3 = eventPaths (CFArrayRef), R4 = eventFlags (uint32*)
TEXT ·fseventsCallbackTrampoline(SB),NOSPLIT|NOFRAME,$0-0
	SUB $32, RSP
	MOVD R29, 16(RSP)
	MOVD LR, 24(RSP)
	CBZ R2, done
	MOVD 0(R1), R5
	CBNZ R5, mark_overflow
	CMP $256, R2
	BHI mark_overflow
	MOVD R2, 0(R1)
	MOVD R3, 8(R1)
	ADD $16, R1, R6
	MOVD $0, R7
copy_loop:
	CMP R2, R7
	BHS copy_done
	MOVWU (R4)(R7<<2), R8
	MOVWU R8, (R6)(R7<<2)
	ADD $1, R7
	B copy_loop
copy_done:
	MOVD R3, R0
	BL libc_CFRetain(SB)
	B done
mark_overflow:
	MOVW $1, R5
	MOVWU R5, 1040(R1)
done:
	MOVD 16(RSP), R29
	MOVD 24(RSP), LR
	ADD $32, RSP
	RET

// The source perform callback is intentionally empty: signaling this source
// wakes CFRunLoopRunInMode without entering Go from CoreFoundation.
TEXT ·fseventsSourceNoopTrampoline(SB),NOSPLIT|NOFRAME,$0-0
	RET

TEXT ·trampolineAddrs(SB),NOSPLIT,$0-8
	MOVD out+0(FP), R1
	MOVD $·libc_CFStringCreateWithBytes_trampoline(SB), R0; MOVD R0, 0(R1)
	MOVD $·libc_CFArrayCreate_trampoline(SB), R0; MOVD R0, 8(R1)
	MOVD $·libc_CFArrayGetValueAtIndex_trampoline(SB), R0; MOVD R0, 16(R1)
	MOVD $·libc_CFStringCreateExternalRepresentation_trampoline(SB), R0; MOVD R0, 24(R1)
	MOVD $·libc_CFDataGetLength_trampoline(SB), R0; MOVD R0, 32(R1)
	MOVD $·libc_CFDataGetBytes_trampoline(SB), R0; MOVD R0, 40(R1)
	MOVD $·libc_CFRelease_trampoline(SB), R0; MOVD R0, 48(R1)
	MOVD $·libc_CFRunLoopGetCurrent_trampoline(SB), R0; MOVD R0, 56(R1)
	MOVD $·libc_CFRunLoopRunInMode_trampoline(SB), R0; MOVD R0, 64(R1)
	MOVD $·libc_CFRunLoopStop_trampoline(SB), R0; MOVD R0, 72(R1)
	MOVD $·libc_CFRunLoopSourceCreate_trampoline(SB), R0; MOVD R0, 80(R1)
	MOVD $·libc_CFRunLoopAddSource_trampoline(SB), R0; MOVD R0, 88(R1)
	MOVD $·libc_CFRunLoopRemoveSource_trampoline(SB), R0; MOVD R0, 96(R1)
	MOVD $·libc_CFRunLoopSourceSignal_trampoline(SB), R0; MOVD R0, 104(R1)
	MOVD $·libc_CFRunLoopWakeUp_trampoline(SB), R0; MOVD R0, 112(R1)
	MOVD $·libc_FSEventsGetCurrentEventId_trampoline(SB), R0; MOVD R0, 120(R1)
	MOVD $·libc_FSEventStreamCreate_trampoline(SB), R0; MOVD R0, 128(R1)
	MOVD $·libc_FSEventStreamScheduleWithRunLoop_trampoline(SB), R0; MOVD R0, 136(R1)
	MOVD $·libc_FSEventStreamUnscheduleFromRunLoop_trampoline(SB), R0; MOVD R0, 144(R1)
	MOVD $·libc_FSEventStreamStart_trampoline(SB), R0; MOVD R0, 152(R1)
	MOVD $·libc_FSEventStreamStop_trampoline(SB), R0; MOVD R0, 160(R1)
	MOVD $·libc_FSEventStreamInvalidate_trampoline(SB), R0; MOVD R0, 168(R1)
	MOVD $·libc_FSEventStreamRelease_trampoline(SB), R0; MOVD R0, 176(R1)
	RET

TEXT ·callbackAddr(SB),NOSPLIT,$0-8
	MOVD $·fseventsCallbackTrampoline(SB), R0
	MOVD R0, ret+0(FP)
	RET

TEXT ·sourceNoopAddr(SB),NOSPLIT,$0-8
	MOVD $·fseventsSourceNoopTrampoline(SB), R0
	MOVD R0, ret+0(FP)
	RET

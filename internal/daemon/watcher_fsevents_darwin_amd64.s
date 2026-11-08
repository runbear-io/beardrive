//go:build darwin && amd64

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
	// DI points to struct { mode uintptr; seconds float64; returnAfterSourceHandled uint32 }
	MOVSD 8(DI), X0
	MOVL 16(DI), SI
	MOVQ 0(DI), DI
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
	// DI points to struct { alloc, callback, context, paths, sinceWhen, latency, flags }
	MOVQ 8(DI), SI
	MOVQ 16(DI), DX
	MOVQ 24(DI), CX
	MOVQ 32(DI), R8
	MOVSD 40(DI), X0
	MOVL 48(DI), R9
	MOVQ 0(DI), DI
	JMP libc_FSEventStreamCreate(SB)

// System V AMD64 ABI callback: DI=streamRef, SI=info, DX=numEvents, CX=eventPaths, R8=eventFlags, R9=eventIds
TEXT ·fseventsCallbackTrampoline(SB),NOSPLIT|NOFRAME,$0-0
	PUSHQ BP
	MOVQ SP, BP
	TESTQ DX, DX
	JZ done
	MOVQ 0(SI), AX
	TESTQ AX, AX
	JNZ mark_overflow
	CMPQ DX, $256
	JA mark_overflow
	MOVQ DX, 0(SI)
	MOVQ CX, 8(SI)
	LEAQ 16(SI), R10
	XORQ R11, R11
copy_loop:
	CMPQ R11, DX
	JAE copy_done
	MOVL (R8)(R11*4), AX
	MOVL AX, (R10)(R11*4)
	INCQ R11
	JMP copy_loop
copy_done:
	MOVQ CX, DI
	CALL libc_CFRetain(SB)
	JMP done
mark_overflow:
	MOVL $1, 1040(SI)
done:
	MOVQ BP, SP
	POPQ BP
	RET

TEXT ·fseventsSourceNoopTrampoline(SB),NOSPLIT|NOFRAME,$0-0
	RET

TEXT ·trampolineAddrs(SB),NOSPLIT,$0-8
	MOVQ out+0(FP), DI
	LEAQ ·libc_CFStringCreateWithBytes_trampoline(SB), AX; MOVQ AX, 0(DI)
	LEAQ ·libc_CFArrayCreate_trampoline(SB), AX; MOVQ AX, 8(DI)
	LEAQ ·libc_CFArrayGetValueAtIndex_trampoline(SB), AX; MOVQ AX, 16(DI)
	LEAQ ·libc_CFStringCreateExternalRepresentation_trampoline(SB), AX; MOVQ AX, 24(DI)
	LEAQ ·libc_CFDataGetLength_trampoline(SB), AX; MOVQ AX, 32(DI)
	LEAQ ·libc_CFDataGetBytes_trampoline(SB), AX; MOVQ AX, 40(DI)
	LEAQ ·libc_CFRelease_trampoline(SB), AX; MOVQ AX, 48(DI)
	LEAQ ·libc_CFRunLoopGetCurrent_trampoline(SB), AX; MOVQ AX, 56(DI)
	LEAQ ·libc_CFRunLoopRunInMode_trampoline(SB), AX; MOVQ AX, 64(DI)
	LEAQ ·libc_CFRunLoopStop_trampoline(SB), AX; MOVQ AX, 72(DI)
	LEAQ ·libc_CFRunLoopSourceCreate_trampoline(SB), AX; MOVQ AX, 80(DI)
	LEAQ ·libc_CFRunLoopAddSource_trampoline(SB), AX; MOVQ AX, 88(DI)
	LEAQ ·libc_CFRunLoopRemoveSource_trampoline(SB), AX; MOVQ AX, 96(DI)
	LEAQ ·libc_CFRunLoopSourceSignal_trampoline(SB), AX; MOVQ AX, 104(DI)
	LEAQ ·libc_CFRunLoopWakeUp_trampoline(SB), AX; MOVQ AX, 112(DI)
	LEAQ ·libc_FSEventsGetCurrentEventId_trampoline(SB), AX; MOVQ AX, 120(DI)
	LEAQ ·libc_FSEventStreamCreate_trampoline(SB), AX; MOVQ AX, 128(DI)
	LEAQ ·libc_FSEventStreamScheduleWithRunLoop_trampoline(SB), AX; MOVQ AX, 136(DI)
	LEAQ ·libc_FSEventStreamUnscheduleFromRunLoop_trampoline(SB), AX; MOVQ AX, 144(DI)
	LEAQ ·libc_FSEventStreamStart_trampoline(SB), AX; MOVQ AX, 152(DI)
	LEAQ ·libc_FSEventStreamStop_trampoline(SB), AX; MOVQ AX, 160(DI)
	LEAQ ·libc_FSEventStreamInvalidate_trampoline(SB), AX; MOVQ AX, 168(DI)
	LEAQ ·libc_FSEventStreamRelease_trampoline(SB), AX; MOVQ AX, 176(DI)
	RET

TEXT ·callbackAddr(SB),NOSPLIT,$0-8
	LEAQ ·fseventsCallbackTrampoline(SB), AX
	MOVQ AX, ret+0(FP)
	RET

TEXT ·sourceNoopAddr(SB),NOSPLIT,$0-8
	LEAQ ·fseventsSourceNoopTrampoline(SB), AX
	MOVQ AX, ret+0(FP)
	RET

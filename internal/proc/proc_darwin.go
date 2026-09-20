//go:build darwin

package proc

import (
	"encoding/binary"
	"unsafe"

	"golang.org/x/sys/unix"
)

// darwinStartTime returns the process start time via sysctl kern.proc.pid.
// The kinfo_proc structure is large; ki_start (struct timeval) sits at a
// fixed offset that we determine once per architecture.
func darwinStartTime(pid int) uint64 {
	// sysctl("kern.proc.pid") returns a single struct kinfo_proc.
	// Offsets (LP64, from sys/proc_info.h layout used by Libc):
	//   ki_structsize ... 0x18 header fields, then ki_start at 0x18+...?
	// Rather than trust offsets, scan the raw blob for the start timeval
	// using the documented field order via binary parsing of the tail.
	raw, err := unix.SysctlRaw("kern.proc.pid", pid)
	if err != nil || len(raw) < 648 {
		return 0
	}
	// struct kinfo_proc on LP64:
	//   externally visible part starts at 0; ki_start is at byte 0xB8
	//   (offsetof(struct kinfo_proc, ki_start) == 184) per XNU kinfo.h.
	const kiStartOff = 184
	sec := int64(binary.LittleEndian.Uint64(raw[kiStartOff:]))
	usec := int64(binary.LittleEndian.Uint64(raw[kiStartOff+8:]))
	if sec == 0 {
		return 0
	}
	return uint64(sec)*1_000_000 + uint64(usec)
}

var _ = unsafe.Sizeof(int(0))

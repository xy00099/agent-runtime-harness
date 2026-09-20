//go:build linux

package proc

// darwinStartTime is a no-op off darwin; unixStartTime reads /proc first.
func darwinStartTime(pid int) uint64 { return 0 }

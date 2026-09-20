//go:build unix

package proc

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

// processIdentity returns the process start time. Linux: field 22 of
// /proc/<pid>/stat. macOS: kern.proc.pid sysctl (see proc_darwin.go).
func ProcessIdentity(pid int) uint64 {
	if pid <= 0 {
		return 0
	}
	return unixStartTime(pid)
}

// processExists checks for a live process.
func processExists(pid int) bool {
	if pid <= 0 {
		return false
	}
	// Signal 0 probes existence without delivering anything.
	return unix.Kill(pid, 0) == nil
}

// KillTree SIGKILLs the whole process group. The supervisor always launches
// children with setpgid, so group id == root pid. ESRCH means already gone.
func KillTree(pid int) error {
	if pid <= 0 {
		return fmt.Errorf("kill: invalid pid %d", pid)
	}
	if !processExists(pid) {
		return nil
	}
	if err := unix.Kill(-pid, unix.SIGKILL); err != nil && err != unix.ESRCH {
		// Group may be gone; try a direct kill before failing.
		if err2 := unix.Kill(pid, unix.SIGKILL); err2 != nil && err2 != unix.ESRCH {
			return fmt.Errorf("kill tree %d: %w", pid, err2)
		}
	}
	return nil
}

// Start launches cmd with its own process group. The caller has already
// wired Stdout/Stderr/Env/Dir on the cmd.
func Start(cmd *exec.Cmd) error {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
	return cmd.Start()
}

// PlatformSync reports the isolation mechanisms used.
func PlatformSync() SyncStatus { return SyncStatus{ProcGroup: true} }

// unixStartTime reads the process start time; Linux via /proc.
func unixStartTime(pid int) uint64 {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err == nil {
		s := string(data)
		// comm may contain spaces/parens; parse after the last ')'.
		if i := strings.LastIndexByte(s, ')'); i >= 0 && i+2 < len(s) {
			fields := strings.Fields(s[i+2:])
			// fields[0] is state (field 3); starttime is field 22 => index 19.
			if len(fields) > 19 {
				if v, err := strconv.ParseUint(fields[19], 10, 64); err == nil {
					return v
				}
			}
		}
	}
	return darwinStartTime(pid)
}

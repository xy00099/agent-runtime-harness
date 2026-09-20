//go:build windows

package proc

import (
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// processIdentity returns the process creation time in 100ns units.
func ProcessIdentity(pid int) uint64 {
	if pid <= 0 {
		return 0
	}
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return 0
	}
	defer windows.CloseHandle(h)
	var create, exit, kuser, ktime windows.Filetime
	if err := windows.GetProcessTimes(h, &create, &exit, &kuser, &ktime); err == nil {
		return uint64(create.Nanoseconds()) / 100
	}
	return 0
}

// processExists checks for a live process.
func processExists(pid int) bool {
	if pid <= 0 {
		return false
	}
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return false
	}
	defer windows.CloseHandle(h)
	var code uint32
	if err := windows.GetExitCodeProcess(h, &code); err != nil {
		return false
	}
	const stillActive = 259 // NTSTATUS STATUS_PENDING as reported by GetExitCodeProcess
	return code == stillActive
}

// KillTree terminates a pid and its descendants via taskkill /T /F.
func KillTree(pid int) error {
	if pid <= 0 {
		return fmt.Errorf("kill: invalid pid %d", pid)
	}
	if !processExists(pid) {
		return nil
	}
	cmd := exec.Command("taskkill", "/T", "/F", "/PID", fmt.Sprintf("%d", pid))
	out, err := cmd.CombinedOutput()
	if err != nil {
		// Tolerate exit races: only fail if it is genuinely still alive.
		if processExists(pid) {
			return fmt.Errorf("taskkill %d: %w: %s", pid, err, strings.TrimSpace(string(out)))
		}
	}
	return nil
}

// job is a global kill-on-close job object assigned to every supervised
// child. If the daemon dies, the OS closes the handles and reaps trees.
var (
	jobOnce   sync.Once
	jobHandle windows.Handle
	jobErr    error
)

func ensureJob() (windows.Handle, error) {
	jobOnce.Do(func() {
		h, err := windows.CreateJobObject(nil, nil)
		if err != nil {
			jobErr = err
			return
		}
		info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{
			BasicLimitInformation: windows.JOBOBJECT_BASIC_LIMIT_INFORMATION{
				LimitFlags: windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE,
			},
		}
		if _, err = windows.SetInformationJobObject(
			h,
			windows.JobObjectExtendedLimitInformation,
			uintptr(unsafe.Pointer(&info)),
			uint32(unsafe.Sizeof(info)),
		); err != nil {
			windows.CloseHandle(h)
			jobErr = err
			return
		}
		jobHandle = h
	})
	return jobHandle, jobErr
}

// Start launches cmd assigned to the global kill-on-close job object.
// The caller has already wired Stdout/Stderr/Env/Dir on the cmd.
func Start(cmd *exec.Cmd) error {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_NEW_PROCESS_GROUP}
	if err := cmd.Start(); err != nil {
		return err
	}
	if job, err := ensureJob(); err == nil {
		// Best effort: if assignment fails the supervisor still tracks the
		// pid and KillTree works.
		if h, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(cmd.Process.Pid)); err == nil {
			_ = windows.AssignProcessToJobObject(job, h)
			windows.CloseHandle(h)
		}
	}
	return nil
}

// PlatformSync reports the isolation mechanisms used.
func PlatformSync() SyncStatus {
	_, err := ensureJob()
	return SyncStatus{JobObject: err == nil}
}

var _ = unsafe.Sizeof(0)

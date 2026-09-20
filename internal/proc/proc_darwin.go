//go:build darwin

package proc

import "golang.org/x/sys/unix"

// darwinStartTime returns the process start time via sysctl kern.proc.pid,
// using the official KinfoProc layout (field Start is the process start
// timeval). Units: microseconds since boot — stable per boot, which is all
// the PID-reuse guard needs.
func darwinStartTime(pid int) uint64 {
	ki, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil || ki == nil {
		return 0
	}
	st := ki.Proc.P_starttime
	if st.Sec == 0 && st.Usec == 0 {
		return 0
	}
	return uint64(st.Sec)*1_000_000 + uint64(st.Usec)
}

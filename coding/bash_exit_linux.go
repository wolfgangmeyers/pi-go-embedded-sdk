//go:build linux

package coding

import "golang.org/x/sys/unix"

// WNOWAIT reserves the child's PID until exec.Cmd.Wait reaps it.
func observeBashExit(pid int) error {
	for {
		var info unix.Siginfo
		err := unix.Waitid(unix.P_PID, pid, &info, unix.WEXITED|unix.WNOWAIT, nil)
		if err != unix.EINTR {
			return err
		}
	}
}

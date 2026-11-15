//go:build darwin

package coding

import "golang.org/x/sys/unix"

// Kqueue observes exit without reaping. ESRCH at registration means the
// unreaped child has already exited; its PID remains reserved until Wait.
func observeBashExit(pid int) error {
	kq, err := unix.Kqueue()
	if err != nil {
		return err
	}
	defer unix.Close(kq)
	var change unix.Kevent_t
	unix.SetKevent(&change, pid, unix.EVFILT_PROC, unix.EV_ADD|unix.EV_ONESHOT)
	change.Fflags = unix.NOTE_EXIT
	_, err = unix.Kevent(kq, []unix.Kevent_t{change}, nil, nil)
	if err == unix.ESRCH {
		return nil
	}
	if err != nil {
		return err
	}
	var events [1]unix.Kevent_t
	for {
		_, err = unix.Kevent(kq, nil, events[:], nil)
		if err != unix.EINTR {
			return err
		}
	}
}

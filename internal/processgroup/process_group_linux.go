package processgroup

import (
	"syscall"

	"golang.org/x/sys/unix"
)

func waitForExit(pid int) error {
	var info unix.Siginfo
	for {
		err := unix.Waitid(unix.P_PID, pid, &info, unix.WEXITED|unix.WNOWAIT, nil)
		if err != unix.EINTR {
			return err
		}
	}
}

func signalGroup(pid int, signal syscall.Signal) error {
	return syscall.Kill(-pid, signal)
}

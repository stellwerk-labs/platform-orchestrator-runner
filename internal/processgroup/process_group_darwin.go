package processgroup

import (
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

func waitForExit(pid int) error {
	queue, err := syscall.Kqueue()
	if err != nil {
		return err
	}
	defer func() { _ = syscall.Close(queue) }()
	change := []syscall.Kevent_t{{Ident: uint64(pid), Filter: syscall.EVFILT_PROC, Flags: syscall.EV_ADD | syscall.EV_ONESHOT, Fflags: syscall.NOTE_EXIT}}
	for {
		_, err = syscall.Kevent(queue, change, nil, nil)
		if err == syscall.EINTR {
			continue
		}
		// This child remains unreaped even if it exited before registration.
		if err == syscall.ESRCH {
			return nil
		}
		if err != nil {
			return err
		}
		break
	}
	events := make([]syscall.Kevent_t, 1)
	for {
		n, err := syscall.Kevent(queue, nil, events, nil)
		if err == syscall.EINTR {
			continue
		}
		if err != nil {
			return err
		}
		if n == 0 {
			continue
		}
		event := events[0]
		if event.Flags&syscall.EV_ERROR != 0 && event.Data != 0 {
			if syscall.Errno(event.Data) == syscall.ESRCH {
				return nil
			}
			return syscall.Errno(event.Data)
		}
		if event.Filter == syscall.EVFILT_PROC && event.Fflags&syscall.NOTE_EXIT != 0 {
			return nil
		}
	}
}

// Apple's sys/proc.h defines SZOMB as a process awaiting collection by its parent.
const zombieProcessState = 5

func signalGroup(pid int, signal syscall.Signal) error {
	err := syscall.Kill(-pid, signal)
	if err != syscall.EPERM {
		return err
	}
	group, queryErr := unix.SysctlKinfoProcSlice("kern.proc.pgrp", pid)
	if queryErr != nil || len(group) == 0 {
		return err
	}
	ownedLeader := false
	for _, member := range group {
		if int(member.Eproc.Pgid) != pid || member.Proc.P_stat != zombieProcessState {
			return err
		}
		if int(member.Proc.P_pid) == pid {
			if int(member.Eproc.Ppid) != os.Getpid() {
				return err
			}
			ownedLeader = true
		}
	}
	if !ownedLeader {
		return err
	}
	return nil
}

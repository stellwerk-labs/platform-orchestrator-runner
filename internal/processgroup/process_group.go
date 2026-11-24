// Package processgroup owns a command's Unix process group until it is reaped.
package processgroup

import (
	"context"
	"errors"
	"os/exec"
	"syscall"
	"time"
)

// Run kills ordinary descendants before Wait releases the leader's PID.
// Commands must come from exec.Command, with no independent waiter or Cancel.
func Run(ctx context.Context, cmd *exec.Cmd) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	exited := make(chan error, 1)
	go func() { exited <- waitForExit(cmd.Process.Pid) }()
	var readinessErr, interruptErr error
	ready, canceled := false, false
	select {
	case readinessErr = <-exited:
		ready = true
	case <-ctx.Done():
		canceled = true
		select {
		case readinessErr = <-exited:
			ready = true
		default:
		}
	}
	if canceled && !ready {
		interruptErr = signalGroup(cmd.Process.Pid, syscall.SIGINT)
		timer := time.NewTimer(500 * time.Millisecond)
		select {
		case readinessErr = <-exited:
			ready = true
		case <-timer.C:
		}
		timer.Stop()
	}
	var killErr error
	// ECHILD means another waiter broke ownership; its PID is no longer ours.
	if !errors.Is(readinessErr, syscall.ECHILD) {
		killErr = signalGroup(cmd.Process.Pid, syscall.SIGKILL)
	}
	if !ready {
		readinessErr = <-exited
	}
	waitErr := cmd.Wait()
	if canceled && waitErr == nil {
		waitErr = ctx.Err()
	}
	if errors.Is(interruptErr, syscall.ESRCH) {
		interruptErr = nil
	}
	if errors.Is(killErr, syscall.ESRCH) {
		killErr = nil
	}
	if readinessErr == nil && interruptErr == nil && killErr == nil {
		return waitErr
	}
	return errors.Join(waitErr, readinessErr, interruptErr, killErr)
}

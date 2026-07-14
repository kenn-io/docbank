//go:build !windows

package client

import (
	"errors"
	"fmt"
	"syscall"
)

func requestProcessStop(pid int) error {
	err := syscall.Kill(pid, syscall.SIGTERM)
	if errors.Is(err, syscall.ESRCH) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("sending SIGTERM: %w", err)
	}
	return nil
}

func forceTerminateProcess(pid int) error {
	err := syscall.Kill(pid, syscall.SIGKILL)
	if errors.Is(err, syscall.ESRCH) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("sending SIGKILL: %w", err)
	}
	return nil
}

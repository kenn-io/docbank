//go:build windows

package client

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/windows"
)

func requestProcessStop(pid int) error {
	return killProcess(pid)
}

func forceTerminateProcess(pid int) error {
	return killProcess(pid)
}

func killProcess(pid int) error {
	process, err := os.FindProcess(pid)
	if err != nil {
		if errors.Is(err, windows.ERROR_INVALID_PARAMETER) {
			return nil
		}
		return fmt.Errorf("finding process: %w", err)
	}
	defer func() { _ = process.Release() }()
	err = process.Kill()
	if errors.Is(err, os.ErrProcessDone) ||
		errors.Is(err, windows.ERROR_INVALID_PARAMETER) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("terminating process: %w", err)
	}
	return nil
}

//go:build windows

package client

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/windows"
)

func terminateProcess(pid int) error {
	process, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	err = process.Kill()
	if errors.Is(err, windows.ERROR_INVALID_PARAMETER) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("terminating process: %w", err)
	}
	return nil
}

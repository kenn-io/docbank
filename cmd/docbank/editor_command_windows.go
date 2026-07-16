//go:build windows

package main

import (
	"fmt"

	"golang.org/x/sys/windows"
)

func defaultEditorCommand() string { return "notepad.exe" }

func splitEditorCommand(command string) ([]string, error) {
	parts, err := windows.DecomposeCommandLine(command)
	if err != nil {
		return nil, fmt.Errorf("splitting Windows editor command: %w", err)
	}
	return parts, nil
}

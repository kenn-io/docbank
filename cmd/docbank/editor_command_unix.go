//go:build !windows

package main

import (
	"fmt"

	"github.com/google/shlex"
)

func defaultEditorCommand() string { return "vi" }

func splitEditorCommand(command string) ([]string, error) {
	parts, err := shlex.Split(command)
	if err != nil {
		return nil, fmt.Errorf("splitting Unix editor command: %w", err)
	}
	return parts, nil
}

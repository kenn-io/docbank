//go:build windows

package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWindowsEditorCommandPreservesQuotedPathAndBackslashes(t *testing.T) {
	command, err := splitEditorCommand(
		`"C:\Program Files\Microsoft VS Code\Code.exe" --wait`,
	)
	require.NoError(t, err)
	assert.Equal(t, []string{
		`C:\Program Files\Microsoft VS Code\Code.exe`, "--wait",
	}, command)
}

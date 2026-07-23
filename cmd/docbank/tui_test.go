package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTUIHelpDefinesReadOnlyInteractiveBoundary(t *testing.T) {
	out, err := runCLI(t, "tui", "--help")
	require.NoError(t, err)
	assert.Contains(t, out, "Open a read-only terminal interface")
	assert.Contains(t, out, "authenticated daemon API")
	assert.Contains(t, out, "initial TUI is deliberately read-only")
	assert.Contains(t, out, "/                    Search names and extracted text")
}

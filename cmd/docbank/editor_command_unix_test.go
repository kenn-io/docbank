//go:build !windows

package main

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestUnixEditorCommandRejectsUnterminatedQuote(t *testing.T) {
	_, err := splitEditorCommand(`"unterminated`)
	require.Error(t, err)
}

func TestExplicitMalformedEditorIsUsageError(t *testing.T) {
	resetFlags(rootCmd)
	t.Cleanup(func() { resetFlags(rootCmd) })
	var stdout, stderr bytes.Buffer
	code := runProcess(
		[]string{"edit", "/document", "--editor", `"unterminated`},
		&stdout, &stderr,
	)
	assert.Equal(t, exitUsage, code)
	assert.Contains(t, stderr.String(), "parsing editor command")
}

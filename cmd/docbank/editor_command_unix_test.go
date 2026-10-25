//go:build !windows

package main

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/assert"
)

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

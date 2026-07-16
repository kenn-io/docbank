//go:build !windows

package main

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestUnixEditorCommandRejectsUnterminatedQuote(t *testing.T) {
	_, err := splitEditorCommand(`"unterminated`)
	require.Error(t, err)
}

//go:build windows

package winsecurity

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestExtendedLengthPathDOS(t *testing.T) {
	got, err := ExtendedLengthPath(`C:\documents\source.txt`)
	require.NoError(t, err)
	require.Equal(t, `\\?\C:\documents\source.txt`, got)
}

func TestExtendedLengthPathUNC(t *testing.T) {
	got, err := ExtendedLengthPath(`\\server\share\documents\source.txt`)
	require.NoError(t, err)
	require.Equal(t, `\\?\UNC\server\share\documents\source.txt`, got)
}

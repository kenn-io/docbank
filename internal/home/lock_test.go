//go:build unix

package home

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestTryLockExclusive(t *testing.T) {
	l := Layout{Root: t.TempDir()}
	require.NoError(t, l.Ensure())

	lk, err := l.TryLockExclusive()
	require.NoError(t, err)
	_, err = l.TryLockExclusive()
	require.ErrorIs(t, err, ErrVaultLocked)
	require.NoError(t, lk.Release())

	lk2, err := l.TryLockExclusive()
	require.NoError(t, err)
	require.NoError(t, lk2.Release())
}

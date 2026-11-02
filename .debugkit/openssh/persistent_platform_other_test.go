//go:build !unix

package openssh

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPersistentManagerIsUnsupported(t *testing.T) {
	require := require.New(t)
	manager, err := NewPersistentManager(t.TempDir(), PersistentConfig{})
	require.NoError(err)

	_, err = manager.Connect(t.Context(), "studio", testTarget("wes@studio"))
	require.ErrorIs(err, ErrPersistentUnsupported)
	_, err = manager.IsAlive(t.Context(), "studio", Generation(1))
	require.ErrorIs(err, ErrPersistentUnsupported)
	arguments, err := ClientArguments("")
	require.NoError(err)
	assert.Equal(t, []string{
		"-o", "ControlMaster=no",
		"-o", "ControlPersist=no",
		"-S", "none",
	}, arguments)
}

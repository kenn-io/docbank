package safefileio

import (
	"os"
	"syscall"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestValidateDarwinExtendedACLRejectsFailedInspection(t *testing.T) {
	file, err := os.CreateTemp(t.TempDir(), "record-*.json")
	require.NoError(t, err)
	require.NoError(t, file.Close())

	require.ErrorIs(t, validateDarwinExtendedACL(file), syscall.EBADF)
}

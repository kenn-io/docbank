//go:build !windows

package mistral

import (
	"encoding/hex"
	"io"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestPrepareClassifiesUnixDiskExhaustion(t *testing.T) {
	for _, platformError := range []error{syscall.ENOSPC, syscall.EDQUOT} {
		directory := filepath.Join(t.TempDir(), "spool")
		makePrivateDirectory(t, directory)
		policy := testPolicy(t, 1024, 10)
		_, err := Prepare(t.Context(), io.NopCloser(errorReader{err: platformError}), policy, PrepareOptions{
			Directory: directory, DeclaredMediaType: mediaTypePDF,
			ExpectedSize: 1, ExpectedSHA256: hex.EncodeToString(make([]byte, 32)),
			MaxSpoolBytes: 2048, MinFreeBytes: 1,
		})
		require.ErrorIs(t, err, ErrSpoolCapacity)
		require.ErrorIs(t, err, platformError)
	}
}

//go:build windows

package mistral

import (
	"encoding/hex"
	"io"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/windows"
)

func TestPrepareClassifiesWindowsDiskExhaustion(t *testing.T) {
	platformErrors := []error{
		windows.ERROR_DISK_FULL,
		windows.ERROR_HANDLE_DISK_FULL,
		windows.ERROR_DISK_QUOTA_EXCEEDED,
	}
	for _, platformError := range platformErrors {
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

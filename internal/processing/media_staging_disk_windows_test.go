package processing

import (
	"os"
	"testing"

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/windows"
)

func TestMediaDiskExhaustionIsRetryableWindows(t *testing.T) {
	t.Parallel()
	for _, err := range []error{windows.ERROR_DISK_FULL, windows.ERROR_HANDLE_DISK_FULL, windows.ERROR_DISK_QUOTA_EXCEEDED} {
		require.ErrorIs(t, mediaStagingError(&os.PathError{Op: "write", Path: "synthetic", Err: err}), ErrMediaStagingUnavailable)
	}
}

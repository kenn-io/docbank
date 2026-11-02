package gitlock

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestUnlockRejectsDoubleRelease(t *testing.T) {
	locker, err := New("").Acquire(t.Context(), t.TempDir())
	if err != nil {
		require.FailNow(t, err.Error())
	}
	if err := locker.Unlock(); err != nil {
		require.FailNow(t, err.Error())
	}
	if err := locker.Unlock(); err == nil {
		require.FailNow(t, "second Unlock succeeded, want error")
	}
}

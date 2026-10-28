package ingest

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func observedNativeFingerprint(t *testing.T, path string) localFileFingerprint {
	t.Helper()
	file, err := os.Open(path)
	require.NoError(t, err)
	info, err := file.Stat()
	require.NoError(t, err)
	fingerprint := fingerprintOpenFile(file, info, true)
	require.NoError(t, file.Close())
	if fingerprint.change == 0 {
		t.Skip("test filesystem does not report native change time")
	}
	return fingerprint
}

func TestSourceChangeTimeOrdinaryWrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "source")
	require.NoError(t, os.WriteFile(path, []byte("alpha"), 0o600))
	before := observedNativeFingerprint(t, path)
	base := fingerprintFileInfo(before.info)
	require.False(t, before.matches(base), "required native evidence cannot be omitted")

	rewriteSourceWithRestoredMtime(t, path, []byte("bravo"))
	after := observedNativeFingerprint(t, path)
	require.False(t, before.matches(after))
	require.True(t, base.matches(fingerprintFileInfo(after.info)))
	require.True(t, after.matches(observedNativeFingerprint(t, path)))
}

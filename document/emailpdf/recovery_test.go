package emailpdf

import (
	"github.com/stretchr/testify/require"
	"os"
	"path/filepath"
	"testing"
)

func TestRecoveryDoesNotDeleteUnownedStaging(t *testing.T) {
	spool := t.TempDir()
	dir := filepath.Join(spool, "email-pdf-unowned")
	require.NoError(t, os.Mkdir(dir, 0700))
	require.NoError(t, RecoverStale(t.Context(), spool))
	_, err := os.Stat(dir)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".docbank-email-pdf-unit"), []byte("unrelated.service"), 0600))
	require.Error(t, RecoverStale(t.Context(), spool))
	_, err = os.Stat(dir)
	require.NoError(t, err)
}

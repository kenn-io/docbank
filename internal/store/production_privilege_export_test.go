package store_test

import (
	"crypto/sha256"
	"encoding/hex"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	productionservice "go.kenn.io/docbank/internal/production"
	"go.kenn.io/docbank/internal/productiontest"
	"go.kenn.io/docbank/internal/store"
)

func TestProductionPrivilegeExportReadsFrozenStoredAuthority(t *testing.T) {
	s, err := store.Open(filepath.Join(t.TempDir(), "docbank.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	productiontest.SeedFrozenPrivilegeLog(t, s)
	const logID = "13131313-1313-4313-8313-131313131313"
	receipt, err := s.PrivilegeLogReceipt(t.Context(), logID, 1)
	require.NoError(t, err)
	for _, test := range []struct{ format, mediaType, prefix string }{
		{"json", productionservice.PrivilegeLogJSONMediaType, "["},
		{"csv", productionservice.PrivilegeLogCSVMediaType, "id,"},
		{"xlsx", productionservice.PrivilegeLogXLSXMediaType, "PK\x03\x04"},
		{"pdf", productionservice.PrivilegeLogPDFMediaType, "%PDF-"},
	} {
		t.Run(test.format, func(t *testing.T) {
			exported, err := s.ExportProductionPrivilegeLog(t.Context(), logID, 1, test.format)
			require.NoError(t, err)
			require.Equal(t, test.mediaType, exported.MediaType)
			require.Equal(t, receipt.SHA256, exported.ReceiptSHA256)
			require.Equal(t, receipt.RowsSHA256, exported.RowsSHA256)
			require.True(t, strings.HasPrefix(string(exported.Content), test.prefix))
			contentSHA := sha256.Sum256(exported.Content)
			require.Equal(t, hex.EncodeToString(contentSHA[:]), exported.ContentSHA256)
			require.NotContains(t, string(exported.Content), "Synthetic private rationale.")
			require.NotContains(t, string(exported.Content), "synthetic@example.test")
		})
	}
	_, err = s.ExportProductionPrivilegeLog(t.Context(), logID, 2, "json")
	require.ErrorIs(t, err, store.ErrNotFound)
	_, err = s.ExportProductionPrivilegeLog(t.Context(), logID, 1, "html")
	require.Error(t, err)
}

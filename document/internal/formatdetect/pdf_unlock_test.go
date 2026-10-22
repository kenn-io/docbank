package formatdetect_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/document/internal/formatdetect"
	"go.kenn.io/docbank/document/media/mediatest"
)

func TestUnlockPDF(t *testing.T) {
	for _, user := range []string{"open", ""} {
		source, err := mediatest.EncryptedPDF(user, "owner", 256)
		require.NoError(t, err)
		plain, _, err := formatdetect.UnlockPDF(t.Context(), source, []string{"wrong space", "open"}, 1<<20)
		require.NoError(t, err)
		pages, err := formatdetect.CountPDFPages(plain)
		require.NoError(t, err)
		require.Equal(t, int64(1), pages)
		_, err = formatdetect.InspectPDF(source, formatdetect.PDFLimits{MaxExpandedBytes: 1 << 20, MaxEntryBytes: 1 << 20, MaxEntries: 1000})
		require.ErrorIs(t, err, formatdetect.ErrPDFEncrypted)
	}
	source, err := mediatest.EncryptedPDF("open", "owner", 128)
	require.NoError(t, err)
	_, _, err = formatdetect.UnlockPDF(t.Context(), source, []string{"wrong"}, 1<<20)
	require.ErrorIs(t, err, formatdetect.ErrPDFEncrypted)
	_, _, err = formatdetect.UnlockPDF(t.Context(), source, []string{"open"}, 1)
	require.ErrorIs(t, err, formatdetect.ErrPDFExpandedBytes)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, _, err = formatdetect.UnlockPDF(ctx, source, []string{"open"}, 1<<20)
	require.ErrorIs(t, err, context.Canceled)
	_, _, err = formatdetect.UnlockPDF(t.Context(), []byte("broken PDF"), []string{"wrong", "open"}, 1<<20)
	require.Error(t, err)
	require.NotErrorIs(t, err, formatdetect.ErrPDFEncrypted)
}

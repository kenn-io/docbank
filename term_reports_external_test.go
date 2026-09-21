package docbank_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"testing"

	"github.com/stretchr/testify/require"
	docbank "go.kenn.io/docbank"
	"go.kenn.io/docbank/report"
)

func TestEmbeddedTermReportOwnsPreparationArtifactAndReaders(t *testing.T) {
	vault, err := docbank.New(t.Context(), docbank.Config{Root: t.TempDir()})
	require.NoError(t, err)
	content := []byte("synthetic alpha")
	digest := sha256.Sum256(content)
	_, err = vault.Create(t.Context(), "/synthetic-alpha.txt", bytes.NewBufferString("synthetic alpha"),
		docbank.CreateOptions{MediaType: "text/plain", Expected: docbank.ContentIdentity{
			SHA256: hex.EncodeToString(digest[:]), Size: int64(len(content))}})
	require.NoError(t, err)
	request := report.Request{Version: 1, AllDocuments: true, Timezone: "UTC",
		CoverageMode: "available_only", Terms: []report.Term{{Number: 1, Expression: "alpha",
			Syntax: "simple", Dates: report.DateRange{Start: "2026-01-01", End: "2026-12-31"}}}}
	prepared, err := vault.PrepareTermReport(t.Context(), request)
	require.NoError(t, err)
	page, err := prepared.Dates(t.Context(), report.DatePageRequest{})
	require.NoError(t, err)
	require.Len(t, page.Members, 1)
	artifact, err := prepared.Finalize(t.Context(), nil)
	require.NoError(t, err)
	require.Equal(t, int64(1), artifact.Summary().Counts[0].Hits)
	require.NoError(t, prepared.Close())
	_, err = prepared.Finalize(t.Context(), nil)
	require.ErrorIs(t, err, docbank.ErrClosed)
	reader, err := artifact.OpenBundle(t.Context())
	require.NoError(t, err)
	packet, err := io.ReadAll(reader)
	require.NoError(t, err)
	require.NotEmpty(t, packet)
	require.NoError(t, artifact.Close())
	_, err = artifact.OpenCSV(t.Context())
	require.ErrorIs(t, err, docbank.ErrClosed)
	require.NoError(t, reader.Close())
	require.NoError(t, vault.Close())
}

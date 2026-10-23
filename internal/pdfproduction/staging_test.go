package pdfproduction

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestPDFStagingOwnershipAndLimit(t *testing.T) {
	dir := t.TempDir()
	artifact := unicodeArtifact(t)
	options := PDFWriteOptions{Recipe: qualificationRecipe(), StagingDirectory: dir, MaxBytes: 32 << 20}
	var output bytes.Buffer
	observed := false
	out := stagingObserver{Writer: &output, observe: func() {
		files, err := filepath.Glob(filepath.Join(dir, "docbank-fresh-*.pdf"))
		require.NoError(t, err)
		require.Len(t, files, 1, "writer must stage under its supplied owner before copying output")
		observed = true
	}}
	require.NoError(t, WriteRendition(t.Context(), out, &artifactSequence{pages: []PageArtifact{artifact}}, options))
	require.True(t, observed)
	require.Greater(t, output.Len(), 1)
	options.MaxBytes = int64(output.Len() - 1)
	output.Reset()
	require.ErrorIs(t, WriteRendition(t.Context(), &output,
		&artifactSequence{pages: []PageArtifact{artifact}}, options), ErrOutputLimit)
	require.Zero(t, output.Len(), "oversized private file must never reach output")
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	require.Empty(t, entries)
}

type stagingObserver struct {
	io.Writer

	observe func()
}

func (w stagingObserver) Write(p []byte) (int, error) { w.observe(); return w.Writer.Write(p) }

func TestSourceStagingCanceledRead(t *testing.T) {
	dir := t.TempDir()
	ctx, cancel := context.WithCancel(t.Context())
	source := Source{Reader: cancelStagingReader{cancel}, Size: 10,
		SHA256: digest([]byte("0123456789")), StagingDirectory: dir}
	_, err := stageSource(ctx, source, 100)
	require.ErrorIs(t, err, context.Canceled)
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	require.Empty(t, entries)
}

type cancelStagingReader struct{ cancel context.CancelFunc }

func (r cancelStagingReader) ReadAt(p []byte, _ int64) (int, error) {
	r.cancel()
	p[0] = '0'
	return 1, nil
}

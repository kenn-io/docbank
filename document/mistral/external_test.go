package mistral_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/mistral"
)

func TestPublicLocalWorkflow(t *testing.T) {
	normalization, err := document.NewNormalizePolicy(100_000)
	require.NoError(t, err)
	policy, err := mistral.NewPolicy(mistral.PolicyConfig{
		Region: "eu", Model: "mistral-ocr-4-0", Retention: "zdr", Training: "opted-out",
		MaxDocumentBytes: 1024, MaxResponseBytes: 1 << 20, MaxUnits: 10,
		ExtractHeader: true, ExtractFooter: true, NormalizePolicy: normalization,
	})
	require.NoError(t, err)
	assert.Equal(t, "https://api.mistral.ai/v1/ocr", policy.Values().Endpoint)
	assert.Equal(t, normalization.Identity(), policy.NormalizePolicy().Identity())
	require.Len(t, mistral.CandidateFormats(), 26)

	content := []byte("%PDF-1.7\nsynthetic")
	digest := sha256.Sum256(content)
	directory := filepath.Join(t.TempDir(), "spool")
	require.NoError(t, os.Mkdir(directory, 0o700))
	prepared, err := mistral.Prepare(t.Context(), io.NopCloser(bytes.NewReader(content)), policy, mistral.PrepareOptions{
		Directory: directory, DeclaredMediaType: "application/pdf",
		ExpectedSize: int64(len(content)), ExpectedSHA256: hex.EncodeToString(digest[:]),
		MaxSpoolBytes: 1024, MinFreeBytes: 1,
	})
	require.NoError(t, err)
	assert.Equal(t, "pdf", prepared.Format().ID)
	require.NoError(t, prepared.Release())
}

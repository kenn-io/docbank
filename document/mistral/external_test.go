package mistral_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/mistral"
	"go.kenn.io/kit/safefileio"
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

	content := externalTestPDF()
	digest := sha256.Sum256(content)
	directory := filepath.Join(t.TempDir(), "spool")
	require.NoError(t, safefileio.EnsurePrivateDir(directory))
	prepared, err := mistral.Prepare(t.Context(), io.NopCloser(bytes.NewReader(content)), policy, mistral.PrepareOptions{
		Directory: directory, DeclaredMediaType: "application/pdf",
		ExpectedSize: int64(len(content)), ExpectedSHA256: hex.EncodeToString(digest[:]),
		MaxSpoolBytes: 1024, MinFreeBytes: 1,
	})
	require.NoError(t, err)
	assert.Equal(t, "pdf", prepared.Format().ID)
	require.NoError(t, prepared.Release())
}

func externalTestPDF() []byte {
	objects := []string{
		"<< /Type /Catalog /Pages 2 0 R >>",
		"<< /Type /Pages /Kids [3 0 R] /Count 1 >>",
		"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] >>",
	}
	var output bytes.Buffer
	output.WriteString("%PDF-1.4\n")
	offsets := make([]int, len(objects))
	for index, object := range objects {
		offsets[index] = output.Len()
		_, _ = fmt.Fprintf(&output, "%d 0 obj\n%s\nendobj\n", index+1, object)
	}
	xref := output.Len()
	_, _ = fmt.Fprintf(&output, "xref\n0 %d\n0000000000 65535 f \n", len(objects)+1)
	for _, offset := range offsets {
		_, _ = fmt.Fprintf(&output, "%010d 00000 n \n", offset)
	}
	_, _ = fmt.Fprintf(&output, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(objects)+1, xref)
	return output.Bytes()
}

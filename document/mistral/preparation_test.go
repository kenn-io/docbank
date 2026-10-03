package mistral

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document/ocr"
)

func TestPrepareDescribesSourceFailures(t *testing.T) {
	t.Parallel()
	pdf := testPDF("synthetic document")
	pptx := pptxArchiveWithSlideXML(t, strings.Replace(validPPTXPresentation(), `r:id="rId1"`, `r:id="private-synthetic-value"`, 1), validPPTXRelationships(), validPPTXContentTypes())
	for _, test := range []struct {
		name, mediaType, description string
		content                      []byte
		change                       func(*PrepareOptions)
	}{
		{name: "size bound", content: pdf, mediaType: mediaTypePDF, change: func(o *PrepareOptions) { o.ExpectedSize = 2 << 20 }, description: "source size is outside policy bounds"},
		{name: "size mismatch", content: pdf, mediaType: mediaTypePDF, change: func(o *PrepareOptions) { o.ExpectedSize++ }, description: "source size mismatch"},
		{name: "hash mismatch", content: pdf, mediaType: mediaTypePDF, change: func(o *PrepareOptions) { o.ExpectedSHA256 = zeroSHA256() }, description: "source hash mismatch"},
		{name: "declared type", content: pdf, mediaType: "application/x-private-synthetic-value", description: "document format is invalid or does not match its declared media type"},
		{name: "malformed PDF", content: []byte("%PDF-1.4\nprivate-synthetic-value\n"), mediaType: mediaTypePDF, description: "PDF structure could not be validated"},
		{name: "unit count", content: pptx, mediaType: "application/vnd.openxmlformats-officedocument.presentationml.presentation", description: "document page or unit count could not be determined"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			directory := filepath.Join(t.TempDir(), "spool")
			makePrivateDirectory(t, directory)
			digest := sha256.Sum256(test.content)
			options := PrepareOptions{
				Directory: directory, DeclaredMediaType: test.mediaType,
				ExpectedSize: int64(len(test.content)), ExpectedSHA256: hex.EncodeToString(digest[:]),
				MaxSpoolBytes: 1 << 20, MinFreeBytes: 1,
			}
			if test.change != nil {
				test.change(&options)
			}
			_, err := Prepare(t.Context(), io.NopCloser(bytes.NewReader(test.content)), testPolicy(t, 1<<20, 10), options)
			require.ErrorIs(t, err, ErrInvalidSource)
			diagnostic, ok := errors.AsType[*ocr.PreparationError](err)
			require.True(t, ok)
			require.EqualError(t, diagnostic, test.description)
			assert.NotContains(t, diagnostic.Error(), "private-synthetic-value")
		})
	}
}

func TestPrepareRetainsPrivateIOCause(t *testing.T) {
	t.Parallel()
	directory := filepath.Join(t.TempDir(), "spool")
	makePrivateDirectory(t, directory)
	cause := &os.PathError{Op: "read", Path: "private-synthetic-source", Err: os.ErrPermission}
	_, err := Prepare(t.Context(), io.NopCloser(errorReader{err: cause}), testPolicy(t, 1024, 10), PrepareOptions{
		Directory: directory, DeclaredMediaType: mediaTypePDF,
		ExpectedSize: 1, ExpectedSHA256: zeroSHA256(), MaxSpoolBytes: 1024, MinFreeBytes: 1,
	})
	require.ErrorIs(t, err, ErrSpoolUnavailable)
	require.ErrorIs(t, err, cause)
	diagnostic, ok := errors.AsType[*ocr.PreparationError](err)
	require.True(t, ok)
	assert.EqualError(t, diagnostic, "temporary document storage or source I/O failed")
}

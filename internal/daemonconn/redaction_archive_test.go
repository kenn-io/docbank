package daemonconn

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/json/v2"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document/redaction"
	"go.kenn.io/docbank/internal/processing"
)

func TestRedactionClientArchive(t *testing.T) {
	prep := redactionClientPreparation(t)
	masks := redactionClientMasks(prep)
	review, err := processing.BuildRedactionReview(prep, masks, prep.Selection.Revision)
	require.NoError(t, err)
	request := processing.RedactionRenderRequest{Preparation: prep, Masks: masks, ReviewSHA256: review.SHA256}
	for _, scenario := range []string{"valid", "review", "changed text", "duplicate", "missing", "extra", "deflate", "digest", "interrupted"} {
		t.Run(scenario, func(t *testing.T) {
			data := redactionArchiveFixture(t, review, scenario)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/zip")
				w.Header().Set("Content-Length", strconv.Itoa(len(data)))
				digest := fmt.Sprintf("%x", sha256.Sum256(data))
				if scenario == "digest" {
					digest = strings.Repeat("0", 64)
				}
				w.Header().Set("X-Content-Sha256", digest)
				w.Header().Set("X-Redaction-Review-Sha256", review.SHA256)
				if scenario == "interrupted" {
					data = data[:len(data)/2]
				}
				_, _ = w.Write(data)
			}))
			defer server.Close()
			client := New(server.URL, "synthetic-key")
			defer func() { _ = client.Close() }()
			file, err := os.Create(filepath.Join(t.TempDir(), "archive.zip"))
			require.NoError(t, err)
			defer func() { _ = file.Close() }()
			got, err := client.RenderNativePDFTo(t.Context(), request, file)
			switch scenario {
			case "valid":
				require.NoError(t, err)
				require.Equal(t, review.SHA256, got.Receipt.ReviewSHA256)
			case "interrupted":
				require.ErrorIs(t, err, io.ErrUnexpectedEOF)
				require.NotErrorIs(t, err, ErrIntegrity)
			default:
				require.ErrorIs(t, err, ErrIntegrity)
			}
		})
	}
}

// PDF semantics belong to the real render qualification, not this wire-protocol fixture.
func redactionArchiveFixture(t *testing.T, review processing.RedactionReview, scenario string) []byte {
	t.Helper()
	pdf := []byte("synthetic verified PDF payload")
	text, err := redaction.Text(review.Resolved)
	require.NoError(t, err)
	if scenario == "changed text" {
		text = []byte("changed but self-consistently hashed\f")
	}
	digest := func(b []byte) processing.RedactionFileDigest {
		return processing.RedactionFileDigest{Size: int64(len(b)), SHA256: fmt.Sprintf("%x", sha256.Sum256(b))}
	}
	receipt := processing.RedactionReceipt{Contract: "native-pdf-redaction-result/v1", Selection: review.Selection, AdmittedNodeRevision: 5, PreparationSHA256: review.PreparationSHA256, DecisionsSHA256: review.DecisionsSHA256, ReviewSHA256: review.SHA256, ResolvedSHA256: review.Resolved.SHA256, Recipe: review.Recipe, PageCount: review.Resolved.PageCount, PDF: digest(pdf), Text: digest(text)}
	if scenario == "review" {
		receipt.ReviewSHA256 = strings.Repeat("0", 64)
	}
	encoded, err := json.Marshal(receipt)
	require.NoError(t, err)
	entries := []struct {
		name string
		data []byte
	}{{"redacted.pdf", pdf}, {"redacted.txt", text}, {"receipt.json", encoded}}
	switch scenario {
	case "duplicate":
		entries[1].name = "redacted.pdf"
	case "missing":
		entries = entries[:2]
	case "extra":
		entries = append(entries, entries[0])
	}
	var output bytes.Buffer
	writer := zip.NewWriter(&output)
	for _, entry := range entries {
		// DOS time avoids the optional extended-timestamp field emitted by Modified.
		header := &zip.FileHeader{Name: entry.name, Method: zip.Store, ModifiedDate: 33} //nolint:staticcheck
		header.SetMode(0600)
		if scenario == "deflate" {
			header.Method = zip.Deflate
		}
		out, err := writer.CreateHeader(header)
		require.NoError(t, err)
		_, err = out.Write(entry.data)
		require.NoError(t, err)
	}
	require.NoError(t, writer.Close())
	return output.Bytes()
}

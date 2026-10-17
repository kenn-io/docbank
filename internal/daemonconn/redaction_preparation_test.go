package daemonconn

import (
	"bytes"
	"crypto/sha256"
	"encoding/json/v2"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/canonical"
	"go.kenn.io/docbank/internal/pdfproduction"
	"go.kenn.io/docbank/internal/processing"
	"go.kenn.io/docbank/internal/production"
	"go.kenn.io/docbank/internal/redactiontest"
	"go.kenn.io/docbank/internal/store"
)

func preparationRequest() processing.RedactionPreparationRequest {
	return processing.RedactionPreparationRequest{Selection: store.PageBinding{NodeID: 7, Revision: 1,
		Source: document.PageSource{VersionID: "00000000-0000-4000-8000-000000000001",
			SHA256: strings.Repeat("a", 64), Size: 20}}}
}

func TestRedactionPreparationClientBindings(t *testing.T) {
	runtime := redactiontest.PageRuntime(t)
	data := redactiontest.PDF(t, []string{"SECRET KEEP SECRET"}, "")
	request := preparationRequest()
	request.Selection.Source.SHA256 = fmt.Sprintf("%x", sha256.Sum256(data))
	request.Selection.Source.Size = int64(len(data))
	inspection, err := runtime.Inspect(t.Context(), data, request.Selection.Source, "application/pdf")
	require.NoError(t, err)
	prepared, err := production.PrepareNativePDF(t.Context(), production.NativePreparationInput{
		PDF: pdfproduction.Source{Reader: bytes.NewReader(data), SHA256: request.Selection.Source.SHA256,
			Size: int64(len(data))}, Inspection: inspection, PageRuntimeFingerprint: runtime.Fingerprint()})
	require.NoError(t, err)
	expected := processing.RedactionPreparation{Contract: "native-pdf-preparation/v1",
		Selection: request.Selection, NativePreparation: prepared}
	encoded, err := json.Marshal(expected)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(encoded, &expected))
	for _, test := range []struct {
		name      string
		change    func(*processing.RedactionPreparation)
		oversized bool
	}{
		{name: "valid"},
		{name: "oversized", oversized: true},
		{name: "selection", change: func(r *processing.RedactionPreparation) { r.Selection.NodeID++ }},
		{name: "frame", change: func(r *processing.RedactionPreparation) {
			frame, err := document.NewPDFPageFrame(r.Evidence.Source, 1, [4]float64{0, 0, 612, 792}, [4]float64{0, 0, 612, 792}, 180)
			require.NoError(t, err)
			r.Frames[0] = frame
			_, _, err = document.MarshalPageFrameV1(r.Frames[0])
			require.NoError(t, err)
		}},
		{name: "evidence", change: func(r *processing.RedactionPreparation) {
			r.Evidence.Pages[0].Glyphs[0].Text = "X"
			encodedEvidence, err := canonical.Marshal(r.Evidence)
			require.NoError(t, err)
			r.EvidenceSHA256 = fmt.Sprintf("%x", sha256.Sum256(encodedEvidence))
		}},
		{name: "map", change: func(r *processing.RedactionPreparation) { r.Map.SHA256 = strings.Repeat("0", 64) }},
	} {
		t.Run(test.name, func(t *testing.T) {
			var result processing.RedactionPreparation
			require.NoError(t, json.Unmarshal(encoded, &result))
			if test.change != nil {
				test.change(&result)
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assert.Equal(t, "/api/v1/redaction/prepare", r.URL.Path)
				assert.Equal(t, "synthetic-key", r.Header.Get("X-Api-Key"))
				w.Header().Set("Content-Type", "application/json")
				assert.NoError(t, json.MarshalWrite(w, struct {
					processing.RedactionPreparation

					Schema string `json:"$schema"`
				}{result, "synthetic"}))
				if test.oversized {
					_, _ = io.WriteString(w, strings.Repeat(" ", 16<<20))
				}
			}))
			defer server.Close()
			client := New(server.URL, "synthetic-key")
			defer func() { require.NoError(t, client.Close()) }()
			got, err := client.PrepareNativePDF(t.Context(), request)
			if test.change == nil && !test.oversized {
				require.NoError(t, err)
				require.Equal(t, expected, got)
			} else {
				require.ErrorIs(t, err, ErrIntegrity)
				require.Empty(t, got.Map.Text)
			}
		})
	}
}

func TestRedactionPreparationClientTransport(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Length", "500")
		_, _ = io.WriteString(w, `{"contract":`)
	}))
	defer server.Close()
	client := New(server.URL, "synthetic-key")
	defer func() { require.NoError(t, client.Close()) }()
	_, err := client.PrepareNativePDF(t.Context(), preparationRequest())
	require.ErrorIs(t, err, io.ErrUnexpectedEOF)
	require.NotErrorIs(t, err, ErrIntegrity)
}

package daemonconn

import (
	"encoding/json/v2"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"uuid"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document/redaction"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/pdfproduction"
	"go.kenn.io/docbank/internal/processing"
	"go.kenn.io/docbank/internal/redactiontest"
)

func redactionClientMasks(prep processing.RedactionPreparation) []processing.RedactionMask {
	return []processing.RedactionMask{{ID: uuid.New(), Selector: redaction.Selector{Kind: "text", MapSHA256: prep.Map.SHA256, Span: &redaction.Span{End: 6}}}}
}

func TestRedactionClientExpansion(t *testing.T) {
	prep := redactionClientPreparation(t)
	request := processing.RedactionResolveRequest{Preparation: prep, Masks: redactionClientMasks(prep)}
	expansion := redactiontest.Expansion(t, pdfproduction.QualifiedRecipe())
	for _, oversized := range []bool{false, true} {
		t.Run(map[bool]string{false: "full expansion", true: "error limit"}[oversized], func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusUnprocessableEntity)
				_, _ = io.WriteString(w, strings.Repeat(" ", (1<<20)+1))
				_ = json.MarshalWrite(w, api.Error{Status: 422, Code: expansion.Code, Redaction: expansion})
				if oversized {
					_, _ = io.WriteString(w, strings.Repeat(" ", 16<<20))
				}
			}))
			defer server.Close()
			client := New(server.URL, "synthetic-key")
			defer func() { _ = client.Close() }()
			_, err := client.ResolveNativePDF(t.Context(), request)
			require.Error(t, err)
			var got *redaction.Problem
			if oversized {
				require.NotErrorAs(t, err, &got)
				return
			}
			require.ErrorAs(t, err, &got)
			require.Equal(t, expansion, got)
			code, ok := ProblemCode(err)
			require.True(t, ok)
			require.Equal(t, expansion.Code, code)
		})
	}
}

func TestRedactionClientReview(t *testing.T) {
	prep := redactionClientPreparation(t)
	masks := redactionClientMasks(prep)
	review, err := processing.BuildRedactionReview(prep, masks, prep.Selection.Revision)
	require.NoError(t, err)
	for _, changed := range []bool{false, true} {
		t.Run(map[bool]string{false: "valid", true: "changed plan"}[changed], func(t *testing.T) {
			result := review
			if changed {
				result.Resolved.RedactBoxes = nil
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _ = json.MarshalWrite(w, result) }))
			defer server.Close()
			client := New(server.URL, "synthetic-key")
			defer func() { _ = client.Close() }()
			got, err := client.ResolveNativePDF(t.Context(), processing.RedactionResolveRequest{Preparation: prep, Masks: masks})
			if changed {
				require.ErrorIs(t, err, ErrIntegrity)
			} else {
				require.NoError(t, err)
				require.Equal(t, review, got)
			}
		})
	}
}

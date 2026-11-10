package api_test

import (
	"bytes"
	"encoding/json/v2"
	"net/http"
	"os"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/processing"
)

//nolint:paralleltest // PDFium uses process-wide resources.
func TestSearchTextCoverage(t *testing.T) {
	var runner *processing.PDFTextRunner
	ts, s := newTestServer(t, func(d *api.Deps) { runner = configureAutomaticPDFText(t, d) })
	_, err := s.CreateFile(t.Context(), s.RootID(), "contract.pdf", testHash("pdf"), 3, "application/pdf")
	require.NoError(t, err)
	_, err = s.CreateFile(t.Context(), s.RootID(), "report.docx", testHash("office"), 6, "application/vnd.openxmlformats-officedocument.wordprocessingml.document")
	require.NoError(t, err)
	resp, body := get(t, ts, "/api/v1/search/text-coverage?mime_type=text/plain", nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	var got api.SearchTextCoverage
	require.NoError(t, json.Unmarshal([]byte(body), &got))
	require.Equal(t, api.SearchTextCoverage{Families: []string{}}, got)
	for _, tc := range []struct {
		query          string
		status, noText int
	}{
		{"", http.StatusOK, 1},
		{"?mime_type=invalid/type;bad", http.StatusUnprocessableEntity, 0},
		{"?modified_since=invalid", http.StatusUnprocessableEntity, 0},
	} {
		resp, body := get(t, ts, "/api/v1/search/text-coverage"+tc.query, nil)
		require.Equal(t, tc.status, resp.StatusCode, body)
		if tc.status == http.StatusOK {
			var got api.SearchTextCoverage
			require.NoError(t, json.Unmarshal([]byte(body), &got))
			require.Equal(t, tc.noText, got.NoText)
		}
	}
	pdf, err := os.ReadFile("../../document/testdata/scanassessment/mixed-below-ratio.pdf")
	require.NoError(t, err)
	hash, size, err := s.Blobs.WriteContext(t.Context(), bytes.NewReader(pdf))
	require.NoError(t, err)
	node, err := s.CreateFile(t.Context(), s.RootID(), "mixed.pdf", hash, size, "application/pdf")
	require.NoError(t, err)
	selector := processing.Selector{NodeID: node.ID, ContentVersionID: node.CurrentVersionID, Profile: processing.PDFTextProfileName}
	plan, err := runner.Service.Plan(t.Context(), selector)
	require.NoError(t, err)
	_, err = runner.Service.Start(t.Context(), processing.StartRequest{Selector: selector, PlanFingerprint: plan.Fingerprint})
	require.NoError(t, err)
	resp, body = get(t, ts, "/api/v1/search/text-coverage", nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	require.Contains(t, body, `"pages_without_text":1`)
}

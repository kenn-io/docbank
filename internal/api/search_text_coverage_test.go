package api_test

import (
	"encoding/json/v2"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/api"
)

func TestSearchTextCoverage(t *testing.T) {
	t.Parallel()
	ts, s := newTestServer(t, nil)
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
		{"", http.StatusOK, 2},
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
}

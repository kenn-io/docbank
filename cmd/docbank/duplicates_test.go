package main

import (
	"encoding/json/v2"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/api"
)

func TestDOCXDuplicatesCLI(t *testing.T) {
	setupFakeDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/api/v1/duplicates/docx", r.URL.Path)
		assert.Equal(t, "7", r.URL.Query().Get("limit"))
		assert.Equal(t, "3", r.URL.Query().Get("offset"))
		w.Header().Set("Content-Type", "application/json")
		_, err := w.Write([]byte(`{"items":[{"fingerprint":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","reference_count":2,"references":[{"id":12,"name":"first.docx","path":"/first.docx","kind":"file","blob_hash":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","size":123},{"id":13,"name":"second.docx","path":"/second.docx","kind":"file","blob_hash":"cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc","size":124}],"references_truncated":false}],"total":4,"total_references":8,"unfingerprinted_references":2,"limit":7,"offset":3}`))
		assert.NoError(t, err)
	})
	out, err := runCLI(t, "duplicates", "docx", "--limit", "7", "--offset", "3", "--json")
	require.NoError(t, err)
	var page api.DOCXDuplicatePage
	require.NoError(t, json.Unmarshal([]byte(out), &page))
	require.Equal(t, 4, page.Total)
	require.Equal(t, 2, page.UnfingerprintedReferences)
	require.Equal(t, int64(12), page.Items[0].References[0].ID)
	out, err = runCLI(t, "duplicates", "docx", "--limit", "7", "--offset", "3")
	require.NoError(t, err)
	require.Contains(t, out, "/first.docx")
	require.Contains(t, out, "id:13")
	require.Contains(t, out, "2 DOCX documents without fingerprints")
}

func TestDOCXDuplicatesCLIRejectsInvalidPage(t *testing.T) {
	for _, args := range [][]string{{"--limit", "0"}, {"--limit", "101"}, {"--offset", "-1"}} {
		_, err := runCLI(t, append([]string{"duplicates", "docx"}, args...)...)
		require.Error(t, err)
	}
}

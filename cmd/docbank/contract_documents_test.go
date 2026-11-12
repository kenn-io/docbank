package main

import (
	"bytes"
	"encoding/json/v2"
	"net/http"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/daemonconn"
)

func TestContractShowIncludesDocumentEvidence(t *testing.T) {
	var out bytes.Buffer
	cmd := newContractShowCommand()
	cmd.SetOut(&out)
	c := api.Contract{Title: "Agreement", Documents: []api.ContractDocument{{NodeID: 42, Role: "signed", ContentVersionID: "11111111-1111-4111-8111-111111111111", BlobSHA256: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Current: false, Available: true}}}
	require.NoError(t, printContractShow(cmd, c, false))
	require.Contains(t, out.String(), "  signed\tid:42\t11111111-1111-4111-8111-111111111111\taaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\tcurrent=false\tavailable=true\n")
}

func TestContractDocumentCLI(t *testing.T) {
	const id = "11111111-1111-4111-8111-111111111111"
	const version = "22222222-2222-4222-8222-222222222222"
	calls := 0
	var malformed atomic.Bool
	var pathMode atomic.Bool
	events := make(chan string, 6)
	setupFakeDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		if pathMode.Load() {
			switch r.URL.Path {
			case "/api/v1/path":
				events <- "resolve"
				assert.Equal(t, http.MethodGet, r.Method)
				assert.Equal(t, "/agreements/signed.pdf", r.URL.Query().Get("path"))
				w.Header().Set("Content-Type", "application/json")
				assert.NoError(t, json.MarshalWrite(w, api.Node{ID: 42, Name: "signed.pdf", Kind: "file", Path: "/agreements/signed.pdf", Revision: 1, CurrentVersionID: version, BlobHash: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Size: 12, MimeType: "application/pdf", CreatedAt: "2024-01-01T00:00:00Z", ModifiedAt: "2024-01-01T00:00:00Z"}))
				return
			case "/api/v1/contracts/" + id:
				events <- "contract"
				assert.Equal(t, http.MethodGet, r.Method)
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("ETag", `"7"`)
				assert.NoError(t, json.MarshalWrite(w, api.Contract{ContractID: id, Revision: 7, Parties: []api.ContractParty{}, Documents: []api.ContractDocument{}}))
				return
			}
			events <- r.Method
		}
		calls++
		assert.Equal(t, "/api/v1/contracts/"+id+"/documents/42", r.URL.Path)
		assert.Equal(t, "signed", r.URL.Query().Get("role"))
		if pathMode.Load() {
			assert.Equal(t, `"7"`, r.Header.Get("If-Match"))
		} else {
			assert.Equal(t, `"1"`, r.Header.Get("If-Match"))
		}
		if r.Method == http.MethodPut {
			assert.False(t, r.URL.Query().Has("content_version_id"))
		} else {
			assert.Equal(t, http.MethodDelete, r.Method)
			assert.Equal(t, version, r.URL.Query().Get("content_version_id"))
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("ETag", `"2"`)
		if malformed.Load() {
			_, _ = w.Write([]byte("{"))
			return
		}
		assert.NoError(t, json.MarshalWrite(w, api.Contract{ContractID: id, Revision: 2, Parties: []api.ContractParty{}, Documents: []api.ContractDocument{}}))
	})
	for _, action := range []string{"link", "unlink"} {
		_, err := runCLI(t, "contract", action, id, "id:42", "--role", "signed", "--revision", "1", "--version", "")
		require.ErrorContains(t, err, "version ID must not be empty")
	}
	require.Zero(t, calls)
	_, err := runCLI(t, "contract", "link", id, "id:42", "--role", "signed", "--revision", "1")
	require.NoError(t, err)
	_, err = runCLI(t, "contract", "unlink", id, "id:42", "--role", "signed", "--revision", "1", "--version", version)
	require.NoError(t, err)
	require.Equal(t, 2, calls)
	malformed.Store(true)
	for _, action := range []string{"link", "unlink"} {
		args := []string{"contract", action, id, "id:42", "--role", "signed", "--revision", "1"}
		if action == "unlink" {
			args = append(args, "--version", version)
		}
		_, err := runCLI(t, args...)
		assert.ErrorContains(t, err, "outcome unknown")
		assert.ErrorContains(t, err, "inspect contracts before retrying")
		assert.True(t, daemonconn.IsRetryableMutationError(err), "underlying mutation error must remain classifiable")
	}
	require.Equal(t, 4, calls)
	malformed.Store(false)
	pathMode.Store(true)
	for _, tc := range []struct{ action, method string }{{"link", http.MethodPut}, {"unlink", http.MethodDelete}} {
		args := []string{"contract", tc.action, id, "/agreements/signed.pdf", "--role", "signed"}
		if tc.action == "unlink" {
			args = append(args, "--version", version)
		}
		_, err := runCLI(t, args...)
		require.NoError(t, err)
		require.Len(t, events, 3)
		assert.Equal(t, "resolve", <-events)
		assert.Equal(t, "contract", <-events)
		assert.Equal(t, tc.method, <-events)
	}
	require.Equal(t, 6, calls)
}

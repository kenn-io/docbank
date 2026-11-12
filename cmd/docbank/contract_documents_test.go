package main

import (
	"bytes"
	"encoding/json/v2"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/api"
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
	setupFakeDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		require.Equal(t, "/api/v1/contracts/"+id+"/documents/42", r.URL.Path)
		require.Equal(t, "signed", r.URL.Query().Get("role"))
		require.Equal(t, `"1"`, r.Header.Get("If-Match"))
		if r.Method == http.MethodPut {
			require.False(t, r.URL.Query().Has("content_version_id"))
		} else {
			require.Equal(t, http.MethodDelete, r.Method)
			require.Equal(t, version, r.URL.Query().Get("content_version_id"))
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("ETag", `"2"`)
		require.NoError(t, json.MarshalWrite(w, api.Contract{ContractID: id, Revision: 2, Parties: []api.ContractParty{}, Documents: []api.ContractDocument{}}))
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
}

package main

import (
	"encoding/json/v2"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/api"
)

func TestTaxCLIStaleAndUsageErrors(t *testing.T) {
	for _, args := range [][]string{{"account", "edit", "id", "--note", "x", "--revision=0"}, {"account", "retire", "id", "--revision=-1"}, {"account", "edit", "id"}, {"account", "edit", "id", "--effective-start", "2025-01-01", "--clear-effective-start"}} {
		cmd := newTaxCommand()
		cmd.SetArgs(args)
		require.Error(t, cmd.Execute())
	}
}

func TestTaxCLIAccountWorkflow(t *testing.T) {
	setupVaultHome(t)
	out, err := runCLI(t, "party", "create", "Example Holdings Ltd.", "--own", "--json")
	require.NoError(t, err, out)
	var party api.Party
	require.NoError(t, json.Unmarshal([]byte(out), &party))
	out, err = runCLI(t, "tax", "account", "add", "--filer", party.PartyID, "--country", "ZZ", "--tax-kind", "Example", "--cadence", "annual", "--currency", "USD", "--identifier", "synthetic12345", "--json")
	require.NoError(t, err, out)
	assert.NotContains(t, out, "synthetic12345")
	var account api.TaxAccount
	require.NoError(t, json.Unmarshal([]byte(out), &account))
	assert.Equal(t, "****2345", account.Identifier)
	out, err = runCLI(t, "tax", "account", "show", account.AccountID, "--reveal", "--json")
	require.NoError(t, err, out)
	assert.Contains(t, out, "synthetic12345")
	out, err = runCLI(t, "tax", "account", "edit", account.AccountID, "--lead-days", "0", "--json")
	require.NoError(t, err, out)
	assert.NotContains(t, out, "synthetic12345")
	out, err = runCLI(t, "tax", "account", "edit", account.AccountID, "--note", "private-synthetic-note", "--revision", "1", "--json")
	require.Error(t, err)
	assert.NotContains(t, out, "private-synthetic-note")
	assert.NotContains(t, err.Error(), "private-synthetic-note")
	out, err = runCLI(t, "tax", "account", "retire", account.AccountID, "--json")
	require.NoError(t, err, out)
	out, err = runCLI(t, "tax", "account", "list", "--include-retired", "--json")
	require.NoError(t, err, out)
	assert.Contains(t, out, account.AccountID)
	assert.NotContains(t, out, "synthetic12345")
}

func TestTaxCLIFlagErrorsArePrivate(t *testing.T) {
	for _, args := range [][]string{{"account", "list", "--limit", "private-synthetic"}, {"account", "edit", "id", "--revision", "private-synthetic"}, {"account", "add", "--private-synthetic"}} {
		cmd := newTaxCommand()
		cmd.SilenceErrors = true
		cmd.SilenceUsage = true
		cmd.SetArgs(args)
		err := cmd.Execute()
		require.Error(t, err)
		assert.NotContains(t, err.Error(), "private-synthetic")
	}
}

func TestTaxCLIRevisionInspectAndNoRetry(t *testing.T) {
	const id = "11111111-1111-4111-8111-111111111111"
	var reads, writes int
	setupFakeDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/tax/accounts/"+id {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.Method {
		case http.MethodGet:
			reads++
			assert.NotEqual(t, "true", r.URL.Query().Get("reveal"))
			w.Header().Set("ETag", `"1"`)
			_, _ = w.Write([]byte(`{"account_id":"` + id + `","revision":1}`))
		case http.MethodPatch:
			writes++
			assert.Equal(t, `"1"`, r.Header.Get("If-Match"))
			w.WriteHeader(http.StatusPreconditionFailed)
			_, _ = w.Write([]byte(`{"status":412,"title":"Precondition Failed","code":"stale_revision"}`))
		}
	})
	_, err := runCLI(t, "tax", "account", "edit", id, "--note", "example")
	require.Error(t, err)
	assert.Equal(t, 1, reads)
	assert.Equal(t, 1, writes)
}

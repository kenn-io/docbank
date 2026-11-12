package main

import (
	"encoding/json/v2"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/api"
)

func TestTaxCLIUsageErrors(t *testing.T) {
	requests := 0
	setupFakeDaemon(t, func(w http.ResponseWriter, _ *http.Request) {
		requests++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = w.Write([]byte(`{"status":422,"code":"invalid_tax","detail":"cadence: unknown cadence"}`))
	})
	for _, args := range [][]string{{"account", "list", "--limit=0"}, {"account", "list", "--offset=-1"}, {"account", "show", "private-id"}, {"account", "edit", "private-id", "--note=x"}, {"account", "retire", "private-id"}, {"account", "edit", "11111111-1111-4111-8111-111111111111", "--note", "x", "--revision=0"}, {"account", "retire", "11111111-1111-4111-8111-111111111111", "--revision=-1"}, {"account", "edit", "11111111-1111-4111-8111-111111111111"}, {"account", "edit", "11111111-1111-4111-8111-111111111111", "--effective-start", "2025-01-01", "--clear-effective-start"}} {
		cmd := newTaxCommand()
		cmd.SetArgs(args)
		err := cmd.Execute()
		require.Error(t, err)
		assert.Equal(t, exitUsage, commandExitCode(err, true))
	}
	assert.Zero(t, requests)
	_, err := runCLI(t, "tax", "account", "add", "--filer", "11111111-1111-4111-8111-111111111111", "--country", "ZZ", "--tax-kind", "Example", "--cadence", "invalid", "--currency", "USD")
	require.Error(t, err)
	assert.Equal(t, 1, requests)
}

func TestTaxCLIAccountWorkflow(t *testing.T) {
	setupVaultHome(t)
	out, err := runCLI(t, "party", "create", "Example Holdings Ltd.", "--own", "--json")
	require.NoError(t, err, out)
	var party api.Party
	require.NoError(t, json.Unmarshal([]byte(out), &party))
	out, err = runCLI(t, "party", "create", "Example Authority", "--json")
	require.NoError(t, err, out)
	var authority api.Party
	require.NoError(t, json.Unmarshal([]byte(out), &authority))
	out, err = runCLI(t, "tax", "account", "add", "--filer", party.PartyID, "--authority", authority.PartyID, "--country", "ZZ", "--tax-kind", "Example", "--cadence", "annual", "--currency", "USD", "--identifier", "synthetic12345", "--effective-start", "2025-01-01", "--note", "line one\nline two", "--json")
	require.NoError(t, err, out)
	var account api.TaxAccount
	require.NoError(t, json.Unmarshal([]byte(out), &account))
	require.NotNil(t, account.AuthorityPartyID)
	assert.Equal(t, authority.PartyID, *account.AuthorityPartyID)
	out, err = runCLI(t, "tax", "account", "show", account.AccountID, "--reveal", "--json")
	require.NoError(t, err, out)
	assert.Contains(t, out, "synthetic12345")
	out, err = runCLI(t, "tax", "account", "list", "--filer", party.PartyID, "--country", "zz", "--taxkind", "Example", "--limit", "1")
	require.NoError(t, err, out)
	assert.Contains(t, out, account.AccountID)
	assert.Contains(t, out, "--offset 1")
	out, err = runCLI(t, "tax", "account", "show", account.AccountID)
	require.NoError(t, err, out)
	assert.Contains(t, out, `note: "line one\nline two"`)
	out, err = runCLI(t, "tax", "account", "show", account.AccountID, "--json")
	require.NoError(t, err, out)
	out, err = runCLI(t, "tax", "account", "edit", account.AccountID, "--lead-days", "0", "--json")
	require.NoError(t, err, out)
	require.NoError(t, json.Unmarshal([]byte(out), &account))
	assert.Zero(t, account.LeadDays)
	out, err = runCLI(t, "tax", "account", "edit", account.AccountID, "--clear-effective-start", "--json")
	require.NoError(t, err, out)
	require.NoError(t, json.Unmarshal([]byte(out), &account))
	assert.Nil(t, account.EffectiveStart)
	out, err = runCLI(t, "tax", "account", "retire", account.AccountID, "--json")
	require.NoError(t, err, out)
}

func TestTaxCLIFlagErrorsArePrivate(t *testing.T) {
	for _, args := range [][]string{{"private-synthetic"}, {"account", "private-synthetic"}, {"account", "add", "private-synthetic"}, {"account", "list", "private-synthetic"}} {
		cmd := newTaxCommand()
		cmd.SilenceErrors, cmd.SilenceUsage = true, true
		cmd.SetArgs(args)
		err := cmd.Execute()
		require.Error(t, err)
		assert.NotContains(t, err.Error(), "private-synthetic")
		if len(args) < 3 {
			assert.Contains(t, err.Error(), "unknown command")
		}
	}
	missing := newTaxCommand()
	missing.SilenceErrors, missing.SilenceUsage = true, true
	missing.SetArgs([]string{"account", "add"})
	missingErr := missing.Execute()
	require.Error(t, missingErr)
	assert.Contains(t, missingErr.Error(), "filer")
	assert.Contains(t, missingErr.Error(), "country")
	for _, args := range [][]string{{"account", "list", "--limit", "private-synthetic"}, {"account", "edit", "11111111-1111-4111-8111-111111111111", "--revision", "private-synthetic"}, {"account", "add", "--private-synthetic"}} {
		cmd := newTaxCommand()
		cmd.SilenceErrors = true
		cmd.SilenceUsage = true
		cmd.SetArgs(args)
		err := cmd.Execute()
		require.Error(t, err)
		assert.NotContains(t, err.Error(), "private-synthetic")
		if args[1] == "list" {
			assert.Contains(t, err.Error(), "limit")
		}
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
			_, _ = w.Write([]byte(`{"account_id":"` + id + `","filer_party_id":"22222222-2222-4222-8222-222222222222","authority_party_id":null,"country":"ZZ","subdivision":"","tax_kind":"Example","identifier":"","cadence":"annual","period_note":"","due_note":"","lead_days":0,"currency":"USD","effective_start":null,"effective_end":null,"note":"","retired_at":null,"created_at":"2025-01-01T00:00:00Z","updated_at":"2025-01-01T00:00:00Z","revision":1}`))
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

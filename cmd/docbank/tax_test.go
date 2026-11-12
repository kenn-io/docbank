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
	assert.Equal(t, exitUsage, commandExitCode(err, true))
	assert.Contains(t, err.Error(), "cadence: unknown cadence")
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
	assert.NotContains(t, out, "synthetic12345")
	var account api.TaxAccount
	require.NoError(t, json.Unmarshal([]byte(out), &account))
	assert.Equal(t, "****2345", account.Identifier)
	require.NotNil(t, account.AuthorityPartyID)
	assert.Equal(t, authority.PartyID, *account.AuthorityPartyID)
	out, err = runCLI(t, "tax", "account", "show", account.AccountID, "--reveal", "--json")
	require.NoError(t, err, out)
	assert.Contains(t, out, "synthetic12345")
	out, err = runCLI(t, "tax", "account", "list", "--filer", party.PartyID, "--country", "zz", "--taxkind", "Example", "--limit", "1")
	require.NoError(t, err, out)
	assert.Contains(t, out, account.AccountID)
	assert.Contains(t, out, "--offset 1")
	assert.NotContains(t, out, "synthetic12345")
	out, err = runCLI(t, "tax", "account", "show", account.AccountID)
	require.NoError(t, err, out)
	assert.Contains(t, out, `note: "line one\nline two"`)
	out, err = runCLI(t, "tax", "account", "show", account.AccountID, "--json")
	require.NoError(t, err, out)
	assert.NotContains(t, out, "synthetic12345")
	out, err = runCLI(t, "tax", "account", "edit", account.AccountID, "--lead-days", "0", "--json")
	require.NoError(t, err, out)
	assert.NotContains(t, out, "synthetic12345")
	require.NoError(t, json.Unmarshal([]byte(out), &account))
	assert.Zero(t, account.LeadDays)
	out, err = runCLI(t, "tax", "account", "edit", account.AccountID, "--note", "private-synthetic-note", "--revision", "1", "--json")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "stale_revision")
	assert.NotContains(t, out, "private-synthetic-note")
	assert.NotContains(t, err.Error(), "private-synthetic-note")
	out, err = runCLI(t, "tax", "account", "edit", account.AccountID, "--clear-effective-start", "--json")
	require.NoError(t, err, out)
	require.NoError(t, json.Unmarshal([]byte(out), &account))
	assert.Nil(t, account.EffectiveStart)
	out, err = runCLI(t, "tax", "account", "retire", account.AccountID, "--json")
	require.NoError(t, err, out)
	assert.NotContains(t, out, "synthetic12345")
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
	cmd := newTaxCommand()
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	cmd.SetArgs([]string{"account", "list", "--limit", "private-synthetic"})
	err := cmd.Execute()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "limit")
	for _, args := range [][]string{{"account", "list", "--limit", "private-synthetic"}, {"account", "edit", "11111111-1111-4111-8111-111111111111", "--revision", "private-synthetic"}, {"account", "add", "--private-synthetic"}} {
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
			_, _ = w.Write([]byte(`{"account_id":"` + id + `","filer_party_id":"22222222-2222-4222-8222-222222222222","country":"ZZ","tax_kind":"Example","cadence":"annual","currency":"USD","created_at":"2025-01-01T00:00:00Z","updated_at":"2025-01-01T00:00:00Z","revision":1}`))
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

func TestTaxCLICommittedOutputFailure(t *testing.T) {
	const id = "11111111-1111-4111-8111-111111111111"
	setupFakeDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("ETag", `"1"`)
		if r.Method == http.MethodPost && r.URL.Path == "/api/v1/tax/accounts" {
			w.WriteHeader(http.StatusCreated)
		}
		_, _ = w.Write([]byte(`{"account_id":"` + id + `","filer_party_id":"22222222-2222-4222-8222-222222222222","country":"ZZ","tax_kind":"Example","cadence":"annual","currency":"USD","created_at":"2025-01-01T00:00:00Z","updated_at":"2025-01-01T00:00:00Z","revision":1}`))
	})
	for _, args := range [][]string{{"account", "add", "--filer", id, "--country", "ZZ", "--tax-kind", "Example", "--cadence", "annual", "--currency", "USD"}, {"account", "edit", id, "--note", "example", "--revision=1"}, {"account", "retire", id, "--revision=1"}} {
		cmd := newTaxCommand()
		cmd.SetOut(searchFailingWriter{})
		cmd.SilenceErrors = true
		cmd.SilenceUsage = true
		cmd.SetArgs(args)
		err := cmd.Execute()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "committed")
		assert.Contains(t, err.Error(), "inspect")
		assert.NotContains(t, err.Error(), "output closed")
	}
}

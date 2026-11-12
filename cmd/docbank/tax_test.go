package main

import (
	"encoding/json/v2"
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

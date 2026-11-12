package main

import (
	"encoding/json/v2"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/api"
)

func TestTaxCLIRecordWorkflow(t *testing.T) {
	setupVaultHome(t)
	out, err := runCLI(t, "party", "create", "Example Holdings Ltd.", "--own", "--json")
	require.NoError(t, err, out)
	var party api.Party
	require.NoError(t, json.Unmarshal([]byte(out), &party))
	out, err = runCLI(t, "tax", "account", "add", "--filer", party.PartyID, "--country", "ZZ", "--tax-kind", "Example", "--cadence", "annual", "--currency", "USD", "--json")
	require.NoError(t, err, out)
	var account api.TaxAccount
	require.NoError(t, json.Unmarshal([]byte(out), &account))
	out, err = runCLI(t, "tax", "record", "add", "--account", account.AccountID, "--kind", "filing", "--title", "Example return", "--tax-year", "2026", "--direction", "payable", "--amount", "1234.00", "--note", "line one\nline two", "--json")
	require.NoError(t, err, out)
	var record api.TaxRecord
	require.NoError(t, json.Unmarshal([]byte(out), &record))
	out, err = runCLI(t, "tax", "record", "list", "--filer", party.PartyID, "--country", "zz", "--taxkind", "example", "--kind", "filing", "--state", "expected", "--year", "2026", "--limit", "1")
	require.NoError(t, err, out)
	assert.Contains(t, out, record.RecordID)
	assert.Contains(t, out, "--offset 1")
	assert.Contains(t, out, `line one\nline two`)
	out, err = runCLI(t, "tax", "record", "show", record.RecordID)
	require.NoError(t, err, out)
	assert.Contains(t, out, `title: "Example return"`)
	out, err = runCLI(t, "tax", "record", "edit", record.RecordID, "--note", "text example")
	require.NoError(t, err, out)
	assert.Contains(t, out, `note: "text example"`)
	record.Revision++
	_, err = runCLI(t, "tax", "record", "edit", record.RecordID, "--direction", "")
	require.Error(t, err)
	out, err = runCLI(t, "tax", "record", "show", record.RecordID, "--json")
	require.NoError(t, err, out)
	var unchanged api.TaxRecord
	require.NoError(t, json.Unmarshal([]byte(out), &unchanged))
	assert.Equal(t, record.Revision, unchanged.Revision)
	out, err = runCLI(t, "tax", "record", "edit", record.RecordID, "--currency", "JPY", "--amount", "1234", "--json")
	require.NoError(t, err, out)
	require.NoError(t, json.Unmarshal([]byte(out), &record))
	_, err = runCLI(t, "tax", "record", "edit", record.RecordID, "--note", "private-synthetic", "--revision", "1")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "stale_revision")
	assert.NotContains(t, err.Error(), "private-synthetic")
	out, err = runCLI(t, "tax", "record", "edit", record.RecordID, "--clear-amount", "--direction", "unknown", "--json")
	require.NoError(t, err, out)
	require.NoError(t, json.Unmarshal([]byte(out), &record))
	assert.Nil(t, record.AmountMinor)
	out, err = runCLI(t, "tax", "record", "add", "--filer", party.PartyID, "--country", "ZZ", "--tax-kind", "Example", "--kind", "audit", "--title", "Example audit", "--json")
	require.NoError(t, err, out)
	var one api.TaxRecord
	require.NoError(t, json.Unmarshal([]byte(out), &one))
	assert.Equal(t, "audit", one.Kind)
	assert.Equal(t, party.PartyID, one.FilerPartyID)
	out, err = runCLI(t, "tax", "record", "retire", record.RecordID, "--json")
	require.NoError(t, err, out)
	out, err = runCLI(t, "tax", "record", "show", record.RecordID, "--json")
	require.NoError(t, err, out)
	require.NoError(t, json.Unmarshal([]byte(out), &record))
	assert.NotNil(t, record.RetiredAt)
}
func TestTaxRecordUsageErrors(t *testing.T) {
	requests := 0
	setupFakeDaemon(t, func(w http.ResponseWriter, _ *http.Request) {
		requests++
		w.WriteHeader(http.StatusUnprocessableEntity)
	})
	const id = "11111111-1111-4111-8111-111111111111"
	for _, args := range [][]string{{"record", "add", "--kind=filing", "--title=Example", "--direction="}, {"record", "edit", id}, {"record", "edit", id, "--revision=0", "--note=x"}, {"record", "edit", id, "--amount=1", "--clear-amount"}, {"record", "list", "--year=private-synthetic"}, {"record", "private-synthetic"}} {
		cmd := newTaxCommand()
		cmd.SilenceErrors = true
		cmd.SilenceUsage = true
		cmd.SetArgs(args)
		err := cmd.Execute()
		require.Error(t, err)
		assert.Equal(t, exitUsage, commandExitCode(err, true))
		assert.NotContains(t, err.Error(), "private-synthetic")
	}
	assert.Zero(t, requests)
	cmd := newTaxCommand()
	cmd.SetArgs([]string{"record", "private-synthetic"})
	cmd.SilenceErrors = true
	cmd.SilenceUsage = true
	err := cmd.Execute()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown command")
}

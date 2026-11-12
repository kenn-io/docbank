package main

import (
	"encoding/json/v2"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/api"
)

const taxRecordTestReceipt = `{"record_id":"11111111-1111-4111-8111-111111111111","filer_party_id":"22222222-2222-4222-8222-222222222222","account_id":null,"authority_party_id":null,"subdivision":"","form_code":"","tax_year":null,"period_start":null,"period_end":null,"due_date":null,"lead_days":null,"amount_minor":null,"currency":"","verified_on":null,"verified_revision":null,"verification_note":"","note":"","retired_at":null,"country":"ZZ","tax_kind":"Example","kind":"filing","title":"Example","state":"expected","direction":"unknown","verification":"unverified","created_at":"2025-01-01T00:00:00Z","updated_at":"2025-01-01T00:00:00Z","revision":1}`

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
	for _, args := range [][]string{{"--filer", "33333333-3333-4333-8333-333333333333"}, {"--year", "2025"}, {"--state", "opened"}, {"--kind", "audit"}, {"--taxkind", "different"}} {
		out, err = runCLI(t, append([]string{"tax", "record", "list"}, args...)...)
		require.NoError(t, err, out)
		assert.NotContains(t, out, record.RecordID)
	}
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
	out, err = runCLI(t, "tax", "record", "retire", record.RecordID, "--json")
	require.NoError(t, err, out)
	out, err = runCLI(t, "tax", "record", "show", record.RecordID, "--json")
	require.NoError(t, err, out)
	require.NoError(t, json.Unmarshal([]byte(out), &record))
	assert.NotNil(t, record.RetiredAt)
}
func TestTaxRecordUsageErrors(t *testing.T) {
	const id = "11111111-1111-4111-8111-111111111111"
	for _, args := range [][]string{{"record", "add", "--kind=filing", "--title=Example", "--direction="}, {"record", "edit", id}, {"record", "edit", id, "--revision=0", "--note=x"}, {"record", "edit", id, "--amount=1", "--clear-amount"}, {"record", "show", id, "--reveal"}, {"record", "list", "--year=private-synthetic"}, {"record", "private-synthetic"}} {
		cmd := newTaxCommand()
		cmd.SilenceErrors = true
		cmd.SilenceUsage = true
		cmd.SetArgs(args)
		err := cmd.Execute()
		require.Error(t, err)
		assert.NotContains(t, err.Error(), "private-synthetic")
	}
	cmd := newTaxCommand()
	cmd.SetArgs([]string{"record", "private-synthetic"})
	cmd.SilenceErrors = true
	cmd.SilenceUsage = true
	err := cmd.Execute()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown command")
}

func TestTaxRecordRevisionInspectNoRetry(t *testing.T) {
	const id = "11111111-1111-4111-8111-111111111111"
	var reads, writes int
	setupFakeDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet {
			reads++
			w.Header().Set("ETag", `"1"`)
			_, _ = w.Write([]byte(taxRecordTestReceipt))
			return
		}
		writes++
		assert.Equal(t, `"1"`, r.Header.Get("If-Match"))
		w.WriteHeader(http.StatusPreconditionFailed)
		_, _ = w.Write([]byte(`{"status":412,"code":"stale_revision"}`))
	})
	_, err := runCLI(t, "tax", "record", "edit", id, "--note", "example")
	require.Error(t, err)
	assert.Equal(t, 1, reads)
	assert.Equal(t, 1, writes)
}

func TestTaxRecordHelp(t *testing.T) {
	cmd := newTaxCommand()
	group, _, err := cmd.Find([]string{"record"})
	require.NoError(t, err)
	assert.Equal(t, helpOnlyMarker, group.Annotations[helpOnlyAnnotation])
}

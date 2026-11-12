package main

import (
	"bytes"
	"encoding/json/v2"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/api"
)

func TestContractEditRejectsConflictingDatesBeforeConnecting(t *testing.T) {
	cmd := newContractEditCommand()
	cmd.SetArgs([]string{"example", "--starts", "2024-01-01", "--clear-starts"})
	require.ErrorContains(t, cmd.Execute(), "cannot set and clear")
}

func TestContractShowQuotesTermsAndResolvesLinkedNames(t *testing.T) {
	c := api.Contract{Title: "Agreement", Summary: "line\n\x1b[31m", Parties: []api.ContractParty{{PartyID: "first", Role: "counterparty", DisplayName: "Supplier"}, {PartyID: "second", Role: "counterparty", DisplayName: "supplier"}}}
	cmd := newContractShowCommand()
	var out bytes.Buffer
	cmd.SetOut(&out)
	require.NoError(t, printContractShow(cmd, c, false))
	require.Contains(t, out.String(), `line\n\x1b[31m`)
	_, err := resolveLinkedContractParty(c, "counterparty", "supplier")
	require.ErrorContains(t, err, "ambiguous")
}
func TestContractEditRejectsEmptyBeforeConnecting(t *testing.T) {
	cmd := newContractEditCommand()
	cmd.SetArgs([]string{"example"})
	require.ErrorContains(t, cmd.Execute(), "no changes requested")
}

// Exercise the actual CLI and daemon together, including failures known before a write.
func TestContractCLIWalkAndPreflight(t *testing.T) {
	setupVaultHome(t)
	out, err := runCLI(t, "party", "create", "Synthetic Owner", "--own", "--json")
	require.NoError(t, err, out)
	var owner api.Party
	require.NoError(t, json.Unmarshal([]byte(out), &owner))
	out, err = runCLI(t, "party", "create", "Synthetic Supplier", "--json")
	require.NoError(t, err, out)
	var supplier api.Party
	require.NoError(t, json.Unmarshal([]byte(out), &supplier))
	out, err = runCLI(t, "contract", "create", "Storage agreement", "--type", "lease", "--status", "active", "--starts", "2024-01-01", "--expires", "2028-01-01", "--signed", "2023-12-01", "--own", "Synthetic Owner", "--counterparty", "Synthetic Supplier", "--summary", "first\nsecond", "--purpose", "Storage", "--consideration", "Annual fee", "--termination", "Notice", "--json")
	require.NoError(t, err, out)
	var contract api.Contract
	require.NoError(t, json.Unmarshal([]byte(out), &contract))
	require.Equal(t, int64(1), contract.Revision)
	require.Len(t, contract.Parties, 2)
	out, err = runCLI(t, "contract", "show", contract.ContractID)
	require.NoError(t, err, out)
	for _, want := range []string{"Storage agreement", "lease", "active", "2024-01-01", "2028-01-01", "2023-12-01", "Synthetic Owner", owner.PartyID, "Synthetic Supplier", supplier.PartyID, `first\nsecond`, "Storage", "Annual fee", "Notice", contract.CreatedAt, contract.UpdatedAt} {
		require.Contains(t, out, want)
	}
	out, err = runCLI(t, "contract", "list", "--as-of", "2025-01-01")
	require.NoError(t, err, out)
	for _, want := range []string{"lease", "Synthetic Supplier", "2024-01-01", "2028-01-01"} {
		require.Contains(t, out, want)
	}
	read := func() api.Contract {
		t.Helper()
		out, err := runCLI(t, "contract", "show", contract.ContractID, "--json")
		require.NoError(t, err, out)
		var got api.Contract
		require.NoError(t, json.Unmarshal([]byte(out), &got))
		return got
	}
	// An unknown later addition must not apply fields or an earlier valid link.
	_, err = runCLI(t, "contract", "edit", contract.ContractID, "--title", "Should not apply", "--own", owner.PartyID, "--counterparty", "Missing supplier")
	require.ErrorContains(t, err, "not found")
	require.Equal(t, contract, read())
	_, err = runCLI(t, "contract", "edit", contract.ContractID, "--counterparty", "Missing supplier")
	require.ErrorContains(t, err, "not found")
	require.Equal(t, contract, read())
	_, err = runCLI(t, "contract", "edit", contract.ContractID)
	require.ErrorContains(t, err, "no changes")
	require.Equal(t, contract, read())
	// A party-only idempotent link does not issue an empty PATCH.
	out, err = runCLI(t, "contract", "edit", contract.ContractID, "--counterparty", supplier.PartyID, "--json")
	require.NoError(t, err, out)
	require.Equal(t, contract, read())
	out, err = runCLI(t, "contract", "create", "Second agreement", "--counterparty", supplier.PartyID, "--json")
	require.NoError(t, err, out)
	var second api.Contract
	require.NoError(t, json.Unmarshal([]byte(out), &second))
	require.Len(t, second.Parties, 1)
	// A retired linked name can be reused by another active party. Both links
	// must be disambiguated by ID before detaching either of them.
	out, err = runCLI(t, "party", "retire", supplier.PartyID)
	require.NoError(t, err, out)
	out, err = runCLI(t, "contract", "edit", second.ContractID, "--remove-counterparty", "Synthetic Supplier", "--json")
	require.NoError(t, err, out)
	var detached api.Contract
	require.NoError(t, json.Unmarshal([]byte(out), &detached))
	require.Empty(t, detached.Parties)
	require.Equal(t, second.Revision+1, detached.Revision)
	out, err = runCLI(t, "party", "create", "Synthetic Supplier", "--json")
	require.NoError(t, err, out)
	var replacement api.Party
	require.NoError(t, json.Unmarshal([]byte(out), &replacement))
	out, err = runCLI(t, "contract", "edit", contract.ContractID, "--counterparty", replacement.PartyID)
	require.NoError(t, err, out)
	before := read()
	_, err = runCLI(t, "contract", "edit", contract.ContractID, "--title", "Should not apply", "--remove-counterparty", "Synthetic Supplier")
	require.ErrorContains(t, err, "ambiguous")
	require.Equal(t, before, read())
	out, err = runCLI(t, "contract", "edit", contract.ContractID, "--remove-counterparty", supplier.PartyID)
	require.NoError(t, err, out)
	after := read()
	require.Equal(t, before.Revision+1, after.Revision)
	require.Len(t, after.Parties, 2)
	out, err = runCLI(t, "contract", "edit", contract.ContractID, "--type", "", "--summary", "", "--purpose", "", "--consideration", "", "--termination", "", "--json")
	require.NoError(t, err, out)
	after = read()
	require.Empty(t, after.ContractType)
	require.Empty(t, after.Summary)
	require.Empty(t, after.Purpose)
	require.Empty(t, after.Consideration)
	require.Empty(t, after.Termination)
	for _, flag := range []string{"--title", "--status"} {
		_, err = runCLI(t, "contract", "edit", contract.ContractID, flag, "")
		require.Error(t, err)
		require.Equal(t, after, read())
	}
	// A syntactically valid unknown ID can only fail at the server; report the
	// prior field mutation and retain its new revision for the caller.
	_, err = runCLI(t, "contract", "edit", contract.ContractID, "--title", "Updated agreement", "--counterparty", "11111111-1111-4111-8111-111111111111")
	require.ErrorContains(t, err, "applied [contract fields]")
	after = read()
	require.Equal(t, "Updated agreement", after.Title)
	out, err = runCLI(t, "contract", "delete", contract.ContractID)
	require.NoError(t, err, out)
	_, err = runCLI(t, "contract", "show", contract.ContractID)
	require.Error(t, err)
}

func TestContractCreateRejectsEmptyStatusBeforeConnecting(t *testing.T) {
	cmd := newContractCreateCommand()
	cmd.SetArgs([]string{"Agreement", "--status", ""})
	require.ErrorContains(t, cmd.Execute(), "status cannot be empty")
}

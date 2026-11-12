package main

import (
	"bytes"
	"go.kenn.io/docbank/internal/api"
	"testing"

	"github.com/stretchr/testify/require"
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

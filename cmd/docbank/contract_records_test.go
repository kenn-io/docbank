package main

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestContractEditRejectsConflictingDatesBeforeConnecting(t *testing.T) {
	cmd := newContractEditCommand()
	cmd.SetArgs([]string{"example", "--starts", "2024-01-01", "--clear-starts"})
	require.ErrorContains(t, cmd.Execute(), "cannot set and clear")
}

package main

import (
	"bytes"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/api"
)

func TestLocalPlanPrintsScope(t *testing.T) {
	t.Parallel()
	var output bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&output)
	plan := api.LocalTextPlan{Profile: "synthetic-profile", Server: "local", Scope: "Selected synthetic documents"}
	require.NoError(t, writeLocalTextPlan(cmd, plan))
	require.Contains(t, output.String(), "scope:     Selected synthetic documents")
}

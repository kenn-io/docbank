package main

import (
	"bytes"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/api"
)

func TestFaceSetupPrintsDisclosedScope(t *testing.T) {
	t.Parallel()
	var output bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&output)
	plan := api.LocalTextPlan{Profile: "local-faces", Server: "local", Scope: "Granting consent automatically queues faces in eligible photos throughout this vault, including Hidden photos."}
	require.NoError(t, writeLocalTextPlan(cmd, plan))
	require.Contains(t, output.String(), "including Hidden photos.")
}

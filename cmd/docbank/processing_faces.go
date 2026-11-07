package main

import (
	"io"

	"github.com/spf13/cobra"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/apiclient"
	"go.kenn.io/docbank/internal/daemonconn"
)

func newProcessingFacesCommand() *cobra.Command {
	return &cobra.Command{Use: "faces", Short: "Turn on local face suggestions", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		c, err := daemonconn.Ensure(cmd.Context())
		if err != nil {
			return err
		}
		defer func() { _ = c.Close() }()
		plan, err := c.API().PlanFaces(cmd.Context())
		if err != nil {
			return err
		}
		if err := writeLocalTextPlan(cmd, *plan); err != nil {
			return err
		}
		_, err = c.API().ApplyFaces(cmd.Context(), &apiclient.ApplyFacesRequestOptions{Body: &api.LocalTextApplyRequest{Server: "local", PlanFingerprint: plan.Fingerprint, Consent: true}})
		if err != nil {
			return err
		}
		_, err = io.WriteString(cmd.OutOrStdout(), "Face suggestions enabled.\n")
		return err
	}}
}

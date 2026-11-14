package main

import (
	"io"
	"sync"

	"github.com/spf13/cobra"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/apiclient"
	"go.kenn.io/docbank/internal/daemonconn"
	"go.kenn.io/kit/embedlocal"
)

func newProcessingFacesCommand() *cobra.Command {
	return &cobra.Command{Use: "faces", Short: "Turn on local face suggestions", Example: "  docbank processing faces", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		c, err := daemonconn.Ensure(cmd.Context())
		if err != nil {
			return err
		}
		closeClient := sync.OnceValue(c.Close)
		defer func() { _ = closeClient() }()
		plan, err := c.API().PlanFaces(cmd.Context())
		if err != nil {
			return err
		}
		if err := writeLocalTextPlan(cmd, *plan); err != nil {
			return err
		}
		if err := closeClient(); err != nil {
			return err
		}
		if err := installLocalTextWithRuntime(cmd, true, embedlocal.FaceModel, embedlocal.Endpoint, embedlocal.Install); err != nil {
			return err
		}
		c, err = daemonconn.Ensure(cmd.Context())
		if err != nil {
			return err
		}
		defer func() { _ = c.Close() }()
		_, err = c.API().ApplyFaces(cmd.Context(), &apiclient.ApplyFacesRequestOptions{Body: &api.FaceSetupApplyRequest{PlanFingerprint: plan.Fingerprint, Consent: true}})
		if err != nil {
			return err
		}
		_, err = io.WriteString(cmd.OutOrStdout(), "Face suggestions enabled.\n")
		return err
	}}
}

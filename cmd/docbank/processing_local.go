package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"text/tabwriter"

	"github.com/spf13/cobra"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/apiclient"
	"go.kenn.io/docbank/internal/daemonconn"
	"go.kenn.io/docbank/internal/home"
	"go.kenn.io/docbank/internal/processing"
	"go.kenn.io/kit/embedlocal"
)

func newProcessingLocalCommand() *cobra.Command {
	var server, embeddingServer, embeddingModel, embeddingAPIKeyEnv string
	var embeddingDimensions int
	cmd := &cobra.Command{Use: "local", Short: "Set up vault-wide text indexing", Args: cobra.NoArgs,
		Example: "  docbank processing local --server local",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if server != "auto" && server != "local" {
				return usageError(errors.New("--server must be auto or local"))
			}
			target := processing.LocalTextTarget{EmbeddingServer: embeddingServer, EmbeddingModel: embeddingModel, EmbeddingDimensions: embeddingDimensions, EmbeddingAPIKeyEnv: embeddingAPIKeyEnv}
			if cmd.Flags().Changed("embedding-server") || cmd.Flags().Changed("embedding-model") || cmd.Flags().Changed("embedding-dimensions") || cmd.Flags().Changed("embedding-api-key-env") {
				if err := target.ValidateEmbeddingServer(); err != nil {
					return usageError(err)
				}
				if cmd.Flags().Changed("server") {
					return usageError(errors.New("--server cannot be combined with --embedding-server"))
				}
			}
			c, err := daemonconn.Ensure(cmd.Context())
			if err != nil {
				return err
			}
			closeClient := sync.OnceValue(c.Close)
			defer func() { _ = closeClient() }()
			request := &api.LocalTextPlanRequest{Server: server}
			if embeddingServer != "" {
				request.Target = &target
			}
			plan, err := c.API().PlanLocalText(cmd.Context(), &apiclient.PlanLocalTextRequestOptions{Body: request})
			if err != nil {
				return err
			}
			if err := writeLocalTextPlan(cmd, *plan); err != nil {
				return err
			}
			if err := closeClient(); err != nil {
				return err
			}
			if err := installLocalText(cmd, plan.Managed); err != nil {
				return err
			}
			if err := applyLocalText(cmd.Context(), *plan); err != nil {
				return err
			}
			layout, err := home.Resolve()
			if err != nil {
				return err
			}
			_, _, err = daemonconn.Restart(cmd.Context(), layout.Root, false)
			if err != nil {
				return fmt.Errorf("text indexing choice saved; run docbank daemon start to activate it: %w", err)
			}
			_, err = fmt.Fprintln(cmd.OutOrStdout(), "Text indexing enabled for this vault and new imports.")
			if err != nil {
				return fmt.Errorf("write text indexing result: %w", err)
			}
			return nil
		}}
	cmd.Flags().StringVar(&server, "server", "auto", "embedding server: auto or local")
	cmd.Flags().StringVar(&embeddingServer, "embedding-server", "", "embedding server URL")
	cmd.Flags().StringVar(&embeddingModel, "embedding-model", "", "embedding model")
	cmd.Flags().StringVar(&embeddingAPIKeyEnv, "embedding-api-key-env", "", "environment variable containing the embedding server API key")
	cmd.Flags().IntVar(&embeddingDimensions, "embedding-dimensions", 0, "embedding dimensions")
	return cmd
}

// applyLocalText acquires a fresh proven connection after any download.
func applyLocalText(ctx context.Context, plan api.LocalTextPlan) (retErr error) {
	c, err := daemonconn.Ensure(ctx)
	if err != nil {
		return err
	}
	defer func() { retErr = errors.Join(retErr, c.Close()) }()
	_, err = c.API().ApplyLocalText(ctx, &apiclient.ApplyLocalTextRequestOptions{Body: &api.LocalTextApplyRequest{Server: plan.Server, Target: plan.Target, PlanFingerprint: plan.Fingerprint, Consent: true}})
	return err
}

func writeLocalTextPlan(cmd *cobra.Command, p api.LocalTextPlan) error {
	w := tabwriter.NewWriter(cmd.OutOrStdout(), 2, 4, 2, ' ', 0)
	if p.Target != nil {
		_, _ = fmt.Fprintf(w, "profile:\t%s\n", p.Profile)
		_, _ = fmt.Fprintf(w, "server:\t%s\nmodel:\t%s\ndimensions:\t%d\n", p.Target.EmbeddingServer, p.Target.EmbeddingModel, p.Target.EmbeddingDimensions)
	} else {
		_, _ = fmt.Fprintf(w, "profile:\t%s\nserver:\t%s\n", p.Profile, p.Server)
	}
	_, _ = fmt.Fprintf(w, "index:\t%s\nscope:\t%s\n", p.VectorEffect, p.Scope)
	_, _ = fmt.Fprintf(w, "retained:\t%s\n", strings.Join(append(p.RetainedClasses, p.Retained...), ", "))
	if err := w.Flush(); err != nil {
		return fmt.Errorf("write local text plan: %w", err)
	}
	return writeProcessingFlow(cmd, p.Flow)
}

// installLocalText installs the shared embeddings server when it is missing.
// Running the command is the approval, so Kit's pre-download callback only
// reports the download size.
func installLocalText(cmd *cobra.Command, managed bool) error {
	return installLocalTextWithRuntime(cmd, managed, embedlocal.TextModel, embedlocal.Endpoint, embedlocal.Install)
}

func installLocalTextWithRuntime(cmd *cobra.Command, managed bool, model string,
	endpoint func(context.Context, embedlocal.Options) (string, error),
	install func(context.Context, embedlocal.InstallOptions) (embedlocal.Installation, error),
) error {
	if !managed {
		return nil
	}
	confirm := func(_ context.Context, bytes int64) (bool, error) {
		if _, err := fmt.Fprintf(cmd.OutOrStdout(), "Downloading local embeddings: %d bytes\n", bytes); err != nil {
			return false, fmt.Errorf("write download size: %w", err)
		}
		return true, nil
	}
	_, err := localEmbeddingOriginWithRuntime(cmd.Context(), model, confirm, endpoint, install)
	return err
}

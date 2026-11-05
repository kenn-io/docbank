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
	"go.kenn.io/kit/embedlocal"
)

func newProcessingLocalCommand() *cobra.Command {
	var server string
	cmd := &cobra.Command{Use: "local", Short: "Set up vault-wide text indexing", Args: cobra.NoArgs,
		Long:    "Move an indexed Nomic profile to the shared local server, keeping its extractor, chunks, and vectors. Existing indexes from other models require rebuilding.",
		Example: "  docbank processing local --server local",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if server != "auto" && server != "local" {
				return usageError(errors.New("--server must be auto or local"))
			}
			c, err := daemonconn.Ensure(cmd.Context())
			if err != nil {
				return err
			}
			closeClient := sync.OnceValue(c.Close)
			defer func() { _ = closeClient() }()
			plan, err := c.API().PlanLocalText(cmd.Context(), &apiclient.PlanLocalTextRequestOptions{Body: &api.LocalTextPlanRequest{Server: server}})
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
			if _, err := daemonconn.Stop(cmd.Context(), layout.Root); err != nil {
				return fmt.Errorf("text indexing choice saved; run docbank daemon start to activate it: %w", err)
			}
			reconnected, err := daemonconn.Ensure(cmd.Context())
			if err != nil {
				return fmt.Errorf("text indexing choice saved; run docbank daemon start to activate it: %w", err)
			}
			defer func() { _ = reconnected.Close() }()
			_, err = fmt.Fprintln(cmd.OutOrStdout(), "Text indexing enabled for this vault and new imports.")
			if err != nil {
				return fmt.Errorf("write text indexing result: %w", err)
			}
			return nil
		}}
	cmd.Flags().StringVar(&server, "server", "auto", "embedding server: auto or local")
	return cmd
}

// applyLocalText acquires a fresh proven connection after any download.
func applyLocalText(ctx context.Context, plan api.LocalTextPlan) (retErr error) {
	c, err := daemonconn.Ensure(ctx)
	if err != nil {
		return err
	}
	defer func() { retErr = errors.Join(retErr, c.Close()) }()
	_, err = c.API().ApplyLocalText(ctx, &apiclient.ApplyLocalTextRequestOptions{Body: &api.LocalTextApplyRequest{Server: plan.Server, PlanFingerprint: plan.Fingerprint, Consent: true}})
	return err
}

func writeLocalTextPlan(cmd *cobra.Command, p api.LocalTextPlan) error {
	w := tabwriter.NewWriter(cmd.OutOrStdout(), 2, 4, 2, ' ', 0)
	_, _ = fmt.Fprintf(w, "profile:\t%s\nserver:\t%s\nindex:\t%s\nscope:\tthis vault and new imports\n", p.Profile, p.Server, p.VectorEffect)
	_, _ = fmt.Fprintf(w, "retained:\t%s\n", strings.Join(p.RetainedClasses, ", "))
	if err := w.Flush(); err != nil {
		return fmt.Errorf("write local text plan: %w", err)
	}
	return writeProcessingFlow(cmd, p.Flow)
}

// installLocalText installs the shared embeddings server when it is missing.
// Running the command is the approval, so Kit's pre-download callback only
// reports the download size.
func installLocalText(cmd *cobra.Command, managed bool) error {
	return installLocalTextWithRuntime(cmd, managed, embedlocal.Endpoint, embedlocal.Install)
}

func installLocalTextWithRuntime(cmd *cobra.Command, managed bool,
	endpoint func(context.Context, embedlocal.Options) (string, error),
	install func(context.Context, embedlocal.InstallOptions) (embedlocal.Installation, error),
) error {
	if !managed {
		return nil
	}
	_, err := endpoint(cmd.Context(), embedlocal.Options{SkipOllama: true})
	if err == nil {
		return nil
	}
	if !errors.Is(err, embedlocal.ErrNotFound) {
		return fmt.Errorf("discover embeddings server: %w", err)
	}
	dir, err := embedlocal.DefaultDir()
	if err != nil {
		return fmt.Errorf("find embeddings directory: %w", err)
	}
	report := func(_ context.Context, bytes int64) (bool, error) {
		if _, err := fmt.Fprintf(cmd.OutOrStdout(), "Downloading local embeddings: %d bytes\n", bytes); err != nil {
			return false, fmt.Errorf("write download size: %w", err)
		}
		return true, nil
	}
	_, err = install(cmd.Context(), embedlocal.InstallOptions{DataDir: dir, Confirm: report})
	return err
}

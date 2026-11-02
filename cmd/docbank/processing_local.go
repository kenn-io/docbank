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
			if err := approveLocalText(cmd, plan.Managed); err != nil {
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

// applyLocalText acquires a fresh proven connection after approval and download.
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
	_, _ = fmt.Fprintf(w, "profile:\t%s\nserver:\t%s\nmodel:\t%s\nvector space:\t%s\nscope:\tthis vault and new imports\n", p.Profile, p.Server, strings.Join(p.Models, ","), p.VectorEffect)
	_, _ = fmt.Fprintf(w, "retained:\t%s\n", strings.Join(p.RetainedClasses, ", "))
	if err := w.Flush(); err != nil {
		return fmt.Errorf("write local text plan: %w", err)
	}
	return writeProcessingFlow(cmd, p.Flow)
}

// approveLocalText uses Kit's single pre-download callback. An offline reuse
// still needs the same explicit vault-wide approval before startup or writes.
func approveLocalText(cmd *cobra.Command, managed bool) error {
	return approveLocalTextWithRuntime(cmd, managed, embedlocal.Endpoint, embedlocal.Install)
}

func approveLocalTextWithRuntime(cmd *cobra.Command, managed bool,
	endpoint func(context.Context, embedlocal.Options) (string, error),
	install func(context.Context, embedlocal.InstallOptions) (embedlocal.Installation, error),
) error {
	prompted := false
	approve := func(_ context.Context, bytes int64) (bool, error) {
		prompted = true
		if _, err := fmt.Fprintf(cmd.OutOrStdout(), "missing download: %d bytes\n", bytes); err != nil {
			return false, fmt.Errorf("write download size: %w", err)
		}
		if err := cmd.Context().Err(); err != nil {
			return false, err
		}
		approved, err := confirmPrompt(cmd, cmd.OutOrStdout(), "Enable text indexing for this vault and new imports?")
		if err != nil {
			return false, err
		}
		if !approved {
			return false, errors.New("text indexing declined")
		}
		return true, nil
	}
	if managed {
		_, err := endpoint(cmd.Context(), embedlocal.Options{SkipOllama: true})
		if errors.Is(err, embedlocal.ErrNotFound) {
			dir, err := embedlocal.DefaultDir()
			if err != nil {
				return fmt.Errorf("find embeddings directory: %w", err)
			}
			_, err = install(cmd.Context(), embedlocal.InstallOptions{DataDir: dir, Confirm: approve})
			if err != nil {
				return err
			}
		} else if err != nil {
			return fmt.Errorf("discover embeddings server: %w", err)
		}
	}
	if !prompted {
		if _, err := approve(cmd.Context(), 0); err != nil {
			return err
		}
	}
	return nil
}

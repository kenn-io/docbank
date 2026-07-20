package main

import (
	"fmt"
	"strconv"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/client"
)

var embeddingsCmd = &cobra.Command{
	Use:   "embeddings",
	Short: "Build and inspect the derived semantic-search index",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		return cmd.Help()
	},
}

var (
	embeddingsBuildJSON     bool
	embeddingsBuildProgress string
)

var embeddingsBuildCmd = &cobra.Command{
	Use:   "build",
	Short: "Mirror verified text and build the configured vector generation",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		c, err := client.Ensure(cmd.Context())
		if err != nil {
			return err
		}
		var result api.EmbeddingBuildResult
		if embeddingsBuildJSON {
			result, err = c.BuildEmbeddings(cmd.Context(), nil)
		} else {
			mode, modeErr := progressModeFromFlag("embeddings build", embeddingsBuildProgress)
			if modeErr != nil {
				return modeErr
			}
			renderer := newBackupProgressRenderer(cmd.ErrOrStderr(), mode)
			defer renderer.finish()
			result, err = c.BuildEmbeddings(cmd.Context(), func(progress api.EmbeddingBuildProgress) {
				renderer.handle(api.BackupProgress{
					Stage: progress.Phase, Done: int64(progress.Done), Total: int64(progress.Total),
				})
			})
		}
		if err != nil {
			return err
		}
		if embeddingsBuildJSON {
			return writeCLIJSON(cmd.OutOrStdout(), result)
		}
		state := "built but not activated"
		if result.Activated {
			state = "active"
		}
		_, err = fmt.Fprintf(cmd.OutOrStdout(),
			"embedding generation %s %s; embedded %d unique content object(s) into %d chunk(s)\n",
			shortFingerprint(result.Fingerprint), state, result.Embedded, result.Chunks)
		if err != nil {
			return fmt.Errorf("writing embeddings build result: %w", err)
		}
		return nil
	},
}

var embeddingsListJSON bool

var embeddingsListCmd = &cobra.Command{
	Use:   "list",
	Short: "List embedding generations and current coverage",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		c, err := client.Ensure(cmd.Context())
		if err != nil {
			return err
		}
		report, err := c.EmbeddingGenerations(cmd.Context())
		if err != nil {
			return err
		}
		if embeddingsListJSON {
			return writeCLIJSON(cmd.OutOrStdout(), report)
		}
		if !report.Configured {
			_, err := fmt.Fprintln(cmd.OutOrStdout(),
				"embeddings are not configured; lexical search remains available")
			if err != nil {
				return fmt.Errorf("writing embeddings configuration status: %w", err)
			}
			return nil
		}
		if len(report.Items) == 0 {
			_, err := fmt.Fprintln(cmd.OutOrStdout(),
				"no embedding generations; run `docbank embeddings build`")
			if err != nil {
				return fmt.Errorf("writing embedding generations: %w", err)
			}
			return nil
		}
		writer := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
		_, _ = fmt.Fprintln(writer, "GENERATION\tSTATE\tMODEL\tDIMS\tEMBEDDED\tSKIPPED\tPENDING")
		for _, item := range report.Items {
			_, _ = fmt.Fprintf(writer, "%s\t%s\t%s\t%d\t%d\t%d\t%d\n",
				shortFingerprint(item.Fingerprint), item.State, strconv.QuoteToASCII(item.Model), item.Dimensions,
				item.Embedded, item.Skipped, item.Pending)
		}
		if err := writer.Flush(); err != nil {
			return fmt.Errorf("writing embedding generations: %w", err)
		}
		return nil
	},
}

func shortFingerprint(fingerprint string) string {
	if len(fingerprint) <= 12 {
		return fingerprint
	}
	return fingerprint[:12]
}

func init() {
	embeddingsBuildCmd.Flags().BoolVar(&embeddingsBuildJSON, "json", false,
		"emit a machine-readable terminal report (progress suppressed)")
	embeddingsBuildCmd.Flags().StringVar(&embeddingsBuildProgress, "progress", "auto",
		"progress output mode: auto, bar, or plain (suppressed by --json)")
	embeddingsListCmd.Flags().BoolVar(&embeddingsListJSON, "json", false,
		"emit machine-readable JSON")
	embeddingsCmd.AddCommand(embeddingsBuildCmd, embeddingsListCmd)
	rootCmd.AddCommand(embeddingsCmd)
}

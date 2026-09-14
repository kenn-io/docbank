package main

import (
	"errors"
	"fmt"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/client"
)

var (
	formatsFamily    string
	formatsFormat    string
	formatsExtension string
	formatsJSON      bool
)

var formatsCmd = &cobra.Command{
	Use:   "formats",
	Short: "Report per-format capability coverage",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		if formatsFormat != "" && formatsExtension != "" {
			return usageError(errors.New("exactly one of --format or --extension may be set"))
		}
		c, err := client.Ensure(cmd.Context())
		if err != nil {
			return err
		}
		response, err := c.FormatCapabilities(
			cmd.Context(), formatsFamily, formatsFormat, formatsExtension)
		if err != nil {
			return err
		}
		if formatsJSON {
			return writeCLIJSON(cmd.OutOrStdout(), response)
		}
		writer := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
		if len(response.Formats) != 0 {
			_, _ = fmt.Fprintln(writer, "FORMAT\tCAPABILITY\tSTATE\tREASON")
			for _, format := range response.Formats {
				for _, capability := range document.AllCapabilityKeys() {
					state := format.Capabilities[capability]
					reason := state.Note
					if reason == "" {
						reason = state.Evidence
					}
					if reason == "" {
						reason = "-"
					}
					_, _ = fmt.Fprintf(writer, "%s\t%s\t%s\t%s\n",
						format.ID, capability, state.State, reason)
				}
			}
		}
		if len(response.Formats) == 0 && response.Lookup != nil {
			_, _ = fmt.Fprintln(writer, "QUERY\tMATCH\tOWNER\tDETAIL")
			lookup := response.Lookup
			switch lookup.Match {
			case document.FormatLookupFormat:
				detail := fmt.Sprintf("Matched %s.", lookup.Format.ID)
				if formatsFamily != "" && lookup.Format.QueryFamily != formatsFamily {
					detail = fmt.Sprintf("Matched %s in family %s; excluded by --family %s.",
						lookup.Format.ID, lookup.Format.QueryFamily, formatsFamily)
				}
				_, _ = fmt.Fprintf(writer, "%s\t%s\t-\t%s\n", lookup.Query, lookup.Match, detail)
			case document.FormatLookupPending:
				_, _ = fmt.Fprintf(writer, "%s\t%s\t%s\t%s: %s\n",
					lookup.Query, lookup.Match, lookup.Pending.OwnerSlice,
					lookup.Pending.Label, lookup.Pending.Note)
			case document.FormatLookupUnknown:
				_, _ = fmt.Fprintf(writer, "%s\t%s\t-\tNo catalog or pending format matched.\n",
					lookup.Query, lookup.Match)
			}
		}
		return writer.Flush()
	},
}

func init() {
	formatsCmd.Flags().StringVar(&formatsFamily, "family", "", "return only one query family")
	formatsCmd.Flags().StringVar(&formatsFormat, "format", "", "look up one format id")
	formatsCmd.Flags().StringVar(&formatsExtension, "extension", "", "look up one extension")
	formatsCmd.Flags().BoolVar(&formatsJSON, "json", false, "machine-readable output")
	rootCmd.AddCommand(formatsCmd)
}

package main

import (
	"errors"
	"fmt"
	"strconv"

	"github.com/google/uuid"
	"github.com/spf13/cobra"

	"go.kenn.io/docbank/internal/apiclient"
	"go.kenn.io/docbank/internal/daemonconn"
)

var (
	labelLookupPackage    string
	labelLookupSet        string
	labelLookupProvenance string
	labelLookupCursor     string
	labelLookupLimit      int
	labelLookupJSON       bool
)

var labelsCmd = &cobra.Command{Use: "labels", Short: "Look up package-scoped received and assigned labels"}

var labelsLookupCmd = &cobra.Command{
	Use: "lookup <label>", Short: "Find every scoped match for an exact Bates label", Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if labelLookupPackage != "" {
			if _, err := uuid.Parse(labelLookupPackage); err != nil {
				return usageError(errors.New("--package must be a UUID"))
			}
		}
		if err := validatePackageReadLimit(labelLookupLimit); err != nil {
			return usageError(err)
		}
		connection, err := daemonconn.Ensure(cmd.Context())
		if err != nil {
			return err
		}
		query := &apiclient.ListPackageLabelCandidatesQuery{Label: args[0], Limit: new(int64(labelLookupLimit))}
		if labelLookupPackage != "" {
			query.PackageID = &labelLookupPackage
		}
		if labelLookupSet != "" {
			query.LabelSet = &labelLookupSet
		}
		if labelLookupCursor != "" {
			query.Cursor = &labelLookupCursor
		}
		if labelLookupProvenance != "" {
			value := apiclient.ListPackageLabelCandidatesQueryProvenance(labelLookupProvenance)
			query.Provenance = &value
		}
		page, err := connection.API().ListPackageLabelCandidates(cmd.Context(), &apiclient.ListPackageLabelCandidatesRequestOptions{Query: query})
		if err != nil {
			return err
		}
		if labelLookupJSON {
			return writeCLIJSON(cmd.OutOrStdout(), page)
		}
		for _, item := range page.Items {
			if _, err = fmt.Fprintf(cmd.OutOrStdout(), "%s\t%s\t%s\t%s\t%s\n",
				strconv.QuoteToASCII(item.Label), item.Provenance, strconv.QuoteToASCII(item.LabelSet), item.PackageID, item.OccurrenceID); err != nil {
				return fmt.Errorf("writing label matches: %w", err)
			}
		}
		return nil
	},
}

func init() {
	labelsLookupCmd.Flags().StringVar(&labelLookupPackage, "package", "", "scope to one package UUID")
	labelsLookupCmd.Flags().StringVar(&labelLookupSet, "label-set", "", "scope to one sender or assigned label set")
	labelsLookupCmd.Flags().StringVar(&labelLookupProvenance, "provenance", "", "scope to received or assigned labels")
	labelsLookupCmd.Flags().StringVar(&labelLookupCursor, "cursor", "", "opaque continuation cursor")
	labelsLookupCmd.Flags().IntVar(&labelLookupLimit, "limit", 100, "page size: 50, 100, or 250")
	labelsLookupCmd.Flags().BoolVar(&labelLookupJSON, "json", false, "emit machine-readable JSON")
	labelsCmd.AddCommand(labelsLookupCmd)
	rootCmd.AddCommand(labelsCmd)
}

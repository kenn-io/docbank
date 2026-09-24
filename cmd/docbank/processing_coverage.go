package main

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"uuid"

	"github.com/spf13/cobra"
	"go.kenn.io/docbank/internal/apiclient"
	"go.kenn.io/docbank/internal/daemonconn"
)

var (
	coverageProfile    string
	coverageVaultID    string
	coverageVersionIDs []string
	coverageJSON       bool
)

var coverageProfilePattern = regexp.MustCompile(`^[a-z][a-z0-9_-]*$`)

var processingCoverageCmd = &cobra.Command{
	Use: "coverage", Short: "Read processing coverage for exact document versions",
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		if len(coverageProfile) < 1 || len(coverageProfile) > 128 || !coverageProfilePattern.MatchString(coverageProfile) {
			return usageError(errors.New("--profile must be a 1-128 character processing profile name"))
		}
		vaultID, err := uuid.Parse(coverageVaultID)
		if err != nil || vaultID.String() != coverageVaultID {
			return usageError(errors.New("--vault-id must be a canonical UUID"))
		}
		if len(coverageVersionIDs) < 1 || len(coverageVersionIDs) > 4096 {
			return usageError(errors.New("--source-version requires 1-4096 version IDs"))
		}
		seen := make(map[string]struct{}, len(coverageVersionIDs))
		for _, version := range coverageVersionIDs {
			parsed, parseErr := uuid.Parse(version)
			if parseErr != nil || parsed.String() != version {
				return usageError(fmt.Errorf("invalid content version ID %q", version))
			}
			if _, duplicate := seen[version]; duplicate {
				return usageError(errors.New("--source-version IDs must be unique"))
			}
			seen[version] = struct{}{}
		}
		c, err := daemonconn.Ensure(cmd.Context())
		if err != nil {
			return err
		}
		report, err := c.API().GetDocumentProcessingCoverage(cmd.Context(), &apiclient.GetDocumentProcessingCoverageRequestOptions{
			Query: &apiclient.GetDocumentProcessingCoverageQuery{Profile: coverageProfile,
				VaultUID: vaultID, ContentVersionID: coverageVersionIDs},
		})
		if err != nil {
			return err
		}
		if report.VaultUID != coverageVaultID || len(report.Embeddings) > 64 {
			return errors.New("processing coverage response escaped its source fence")
		}
		if coverageJSON {
			return writeCLIJSON(cmd.OutOrStdout(), report)
		}
		_, err = fmt.Fprintf(cmd.OutOrStdout(), "vault: %s\nprofile: %s\nstate: %s\nrenditions: %s (%d/%d complete)\n",
			report.VaultUID, report.ProfileFingerprint, report.State, report.Renditions.State,
			report.Renditions.Complete, report.Renditions.Total)
		if err != nil {
			return fmt.Errorf("writing processing coverage: %w", err)
		}
		for _, class := range report.Embeddings {
			if _, err := fmt.Fprintf(cmd.OutOrStdout(), "embedding %s: %s (%d/%d complete)\n",
				strings.TrimSpace(class.Name), class.State, class.Complete, class.Total); err != nil {
				return fmt.Errorf("writing embedding coverage: %w", err)
			}
		}
		return nil
	},
}

func init() {
	processingCoverageCmd.Flags().StringVar(&coverageProfile, "profile", "", "processing profile")
	processingCoverageCmd.Flags().StringVar(&coverageVaultID, "vault-id", "", "exact vault ID")
	processingCoverageCmd.Flags().StringArrayVar(&coverageVersionIDs, "source-version", nil, "content version ID (repeatable, 1-4096)")
	processingCoverageCmd.Flags().BoolVar(&coverageJSON, "json", false, "emit machine-readable JSON")
	processingCmd.AddCommand(processingCoverageCmd)
}

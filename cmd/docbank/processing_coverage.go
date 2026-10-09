package main

import (
	"errors"
	"fmt"
	"io"
	"strings"
	"uuid"

	"github.com/spf13/cobra"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/apiclient"
	"go.kenn.io/docbank/internal/daemonconn"
)

type processingCoverageOutput struct {
	ContentVersionID uuid.UUID          `json:"content_version_id"`
	Coverage         api.CoverageReport `json:"coverage"`
}

func newProcessingCoverageCommand() *cobra.Command {
	cmd := &cobra.Command{
		Example: `  docbank processing coverage <version-uuid> --profile <profile>`,
		Use:     "coverage <version-uuid>",
		Short:   "Inspect processing coverage for one exact version",
		Args:    cobra.ExactArgs(1)}
	var profile string
	var asJSON bool
	cmd.Flags().StringVar(&profile, "profile", "", "executable processing profile (required)")
	cmd.Flags().BoolVar(&asJSON, "json", false, "print JSON to stdout")
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		if err := validateInspectionSelection(args[0], profile); err != nil {
			return err
		}
		c, err := daemonconn.Ensure(cmd.Context())
		if err != nil {
			return err
		}
		defer func() { _ = c.Close() }()
		return runProcessingCoverage(cmd, c, args[0], profile, asJSON)
	}
	return cmd
}

func runProcessingCoverage(cmd *cobra.Command, c *daemonconn.Connection,
	version, profile string, asJSON bool,
) error {
	vaultID, err := inspectionVaultID(cmd.Context(), c)
	if err != nil {
		return err
	}
	coverage, err := c.API().GetDocumentProcessingCoverage(cmd.Context(),
		&apiclient.GetDocumentProcessingCoverageRequestOptions{
			Query: &apiclient.GetDocumentProcessingCoverageQuery{
				Profile: profile, VaultUID: vaultID, ContentVersionID: []string{version},
			},
		})
	if err != nil {
		if code, _ := daemonconn.ProblemCode(err); code == "processing_profile_unavailable" {
			return inspectionProfileError(profile, err)
		}
		return err
	}
	if coverage == nil || coverage.VaultUID != vaultID.String() {
		return errors.New("coverage response does not match the connected vault")
	}
	if asJSON {
		id, err := uuid.Parse(version)
		if err != nil {
			return fmt.Errorf("parsing version ID: %w", err)
		}
		return writeCLIJSON(cmd.OutOrStdout(), processingCoverageOutput{id, *coverage})
	}
	return writeProcessingCoverage(cmd.OutOrStdout(), version, profile, *coverage)
}

func writeProcessingCoverage(w io.Writer, version, profile string, coverage api.CoverageReport) error {
	var text strings.Builder
	fmt.Fprintf(&text, "version: %s\nvault: %s\nprofile: %q\nfingerprint: %s\nstate: %q\n",
		version, coverage.VaultUID, profile, coverage.ProfileFingerprint, coverage.State)
	writeCoverageClass(&text, "rendition", coverage.Renditions)
	for _, class := range coverage.Embeddings {
		writeCoverageClass(&text, "embedding", class)
	}
	_, err := io.WriteString(w, text.String())
	return err
}

func writeCoverageClass(w *strings.Builder, kind string, class api.CoverageClass) {
	fmt.Fprintf(w, "%s %q: required=%t state=%q total=%d complete=%d unavailable=%d "+
		"stale=%d ineligible=%d rebuilding=%d previous-generation-serving=%d\n",
		kind, class.Name, class.Required, class.State, class.Total, class.Complete,
		class.Unavailable, class.Stale, class.Ineligible, class.Rebuilding,
		class.PreviousGenerationServing)
}

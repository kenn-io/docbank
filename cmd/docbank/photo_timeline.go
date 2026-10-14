package main

import (
	"github.com/spf13/cobra"

	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/daemonconn"
	"go.kenn.io/docbank/internal/query"
)

func newPhotoTimelineCommand() *cobra.Command {
	var raw, coverage, profile string
	command := &cobra.Command{
		Example: `  docbank photos timeline
  docbank photos timeline --query '{"filters":{"capture_after":"2024-01-01"}}'`,
		Use:   "timeline",
		Short: "Count photos by capture day, or file modified day",
		Long: `Counts every photo asset the query selects by its recorded calendar day,
newest day first. A photo without a capture date counts on the UTC day its
file was last modified. Scope matches photo browsing in the web app.
Output: JSON with one capture_day facet. total includes photos with neither date, which
are also counted in missing.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if _, err := query.Parse([]byte(raw)); err != nil {
				return usageError(err)
			}
			c, err := daemonconn.Ensure(cmd.Context())
			if err != nil {
				return err
			}
			timeline, err := c.PhotoTimeline(cmd.Context(), api.PhotoTimelineRequest{
				Query:    api.QueryPayload(raw),
				Coverage: api.WorkspaceQueryCoverage{Configuration: coverage, ProfileFingerprint: profile},
			})
			if err != nil {
				return err
			}
			return writeCLIJSON(cmd.OutOrStdout(), timeline)
		},
	}
	command.Flags().StringVar(&raw, "query", "{}", "strict QueryV1 JSON selecting the photos to count")
	command.Flags().StringVar(&coverage, "coverage", "", "coverage configuration: configured or unconfigured")
	command.Flags().StringVar(&profile, "profile-fingerprint", "", "configured coverage profile fingerprint")
	return command
}

func init() {
	photosCmd.AddCommand(newPhotoTimelineCommand())
}

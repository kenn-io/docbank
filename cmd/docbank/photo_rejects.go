package main

import (
	"bufio"

	"github.com/spf13/cobra"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/daemonconn"
	"go.kenn.io/docbank/internal/query"
)

func init() {
	var raw, digest, coverage, profile string
	var hidden bool
	command := &cobra.Command{Use: "rejects", Short: "Preview rejects or move a reviewed scope to trash", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		if _, err := query.Parse([]byte(raw)); err != nil {
			return usageError(err)
		}
		c, err := daemonconn.Ensure(cmd.Context())
		if err != nil {
			return err
		}
		return runPhotoRejects(cmd, c, api.PhotoRejectsRequest{Query: api.QueryPayload(raw), Hidden: hidden, Digest: digest, Coverage: api.WorkspaceQueryCoverage{Configuration: coverage, ProfileFingerprint: profile}})
	}}
	command.Flags().StringVar(&raw, "query", "{}", "QueryV1 JSON selecting the scope")
	command.Flags().StringVar(&digest, "confirm", "", "preflight digest authorizing the move")
	command.Flags().BoolVar(&hidden, "hidden", false, "use the unlocked Hidden scope")
	command.Flags().StringVar(&coverage, "coverage", "", "coverage configuration")
	command.Flags().StringVar(&profile, "profile-fingerprint", "", "configured coverage profile fingerprint")
	photosCmd.AddCommand(command)
}

func runPhotoRejects(cmd *cobra.Command, c *daemonconn.Connection, request api.PhotoRejectsRequest) error {
	var cookie string
	if request.Hidden {
		passcode, err := readPhotoPasscode(cmd, bufio.NewReaderSize(cmd.InOrStdin(), 2048), "Passcode")
		if err != nil {
			return err
		}
		_, cookie, err = c.PhotoHidden(cmd.Context(), "unlock", passcode, "")
		if err != nil {
			return err
		}
	}
	result, err := c.PhotoRejects(cmd.Context(), request, cookie)
	if err != nil {
		return err
	}
	return writeCLIJSON(cmd.OutOrStdout(), struct {
		api.PhotoRejectsPreflight
		Moved bool `json:"moved"`
	}{result, request.Digest != ""})
}

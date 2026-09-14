package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/client"
)

var (
	packagePreflightProfile  string
	packagePreflightEncoding string
	packagePreflightMap      string
	packagePreflightJSON     bool
)

var packageCmd = &cobra.Command{
	Use:   "package",
	Short: "Inspect and exchange load-file packages",
}

var packagePreflightCmd = &cobra.Command{
	Use:   "preflight <container-or-path>",
	Short: "Validate a received load-file package without importing it",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if packagePreflightProfile == "" {
			return usageError(errors.New("--profile is required"))
		}
		if packagePreflightEncoding == "" {
			return usageError(errors.New("--encoding is required"))
		}
		var mapping []byte
		if packagePreflightMap != "" {
			var err error
			mapping, err = os.ReadFile(packagePreflightMap)
			if err != nil {
				return fmt.Errorf("reading package mapping: %w", err)
			}
		}
		request := api.PackagePreflightRequest{Profile: packagePreflightProfile, Encoding: packagePreflightEncoding, Mapping: mapping}
		kind, reference, containerID, err := packagePreflightSource(args[0])
		if err != nil {
			return err
		}
		request.SourceKind, request.SourceRef = kind, reference
		c, err := client.Ensure(cmd.Context())
		if err != nil {
			return err
		}
		result, err := c.PreflightPackage(cmd.Context(), containerID, request)
		if err != nil {
			return err
		}
		if packagePreflightJSON {
			return writeCLIJSON(cmd.OutOrStdout(), result)
		}
		if _, err = fmt.Fprintf(cmd.OutOrStdout(), "preflight %s: %d records, %d pages, blocking=%t\n", result.PreflightID, result.Records, result.Pages, result.Blocking); err != nil {
			return fmt.Errorf("writing package preflight: %w", err)
		}
		return nil
	},
}

func packagePreflightSource(source string) (kind, reference, containerID string, err error) {
	if info, statErr := os.Stat(source); statErr == nil && info.IsDir() {
		absolute, absErr := filepath.Abs(source)
		if absErr != nil {
			return "", "", "", fmt.Errorf("resolve package root: %w", absErr)
		}
		return "root", absolute, "", nil
	}
	return "container", source, source, nil
}

func init() {
	packagePreflightCmd.Flags().StringVar(&packagePreflightProfile, "profile", "", "declared load-file profile")
	packagePreflightCmd.Flags().StringVar(&packagePreflightEncoding, "encoding", "", "declared source encoding")
	packagePreflightCmd.Flags().StringVar(&packagePreflightMap, "map", "", "loadfile-mapping/v1 JSON file")
	packagePreflightCmd.Flags().BoolVar(&packagePreflightJSON, "json", false, "emit machine-readable JSON")
	packageCmd.AddCommand(packagePreflightCmd)
	rootCmd.AddCommand(packageCmd)
}

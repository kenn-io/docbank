package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"uuid"

	"github.com/spf13/cobra"

	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/apiclient"
	"go.kenn.io/docbank/internal/daemonconn"
)

var (
	packagePreflightProfile        string
	packagePreflightPageMapProfile string
	packagePreflightEncoding       string
	packagePreflightMap            string
	packagePreflightJSON           bool
	packageImportInto              string
	packageImportName              string
	packageImportParty             string
	packageImportOperation         string
	packageImportPartial           bool
	packageImportSuppliedText      bool
	packageImportJSON              bool
	packageListDirection           string
	packageListAfter               string
	packageReadLimit               int
	packageReadAfterOrdinal        int
	packageReadJSON                bool
)

var packageCmd = &cobra.Command{
	Use:   "package",
	Short: "Inspect and exchange load-file packages",
}

var packagePreflightCmd = &cobra.Command{
	Use:   "preflight <path>",
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
		request := api.PackagePreflightRequest{Profile: packagePreflightProfile, PageMapProfile: packagePreflightPageMapProfile, Encoding: packagePreflightEncoding, Mapping: mapping}
		reference, err := packagePreflightSource(args[0])
		if err != nil {
			return err
		}
		request.SourceKind, request.SourceRef = "root", reference
		c, err := daemonconn.Ensure(cmd.Context())
		if err != nil {
			return err
		}
		result, err := c.API().CreatePackagePreflight(cmd.Context(), &apiclient.CreatePackagePreflightRequestOptions{Body: &request})
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

var packageImportCmd = &cobra.Command{
	Use:   "import <preflight-id>",
	Short: "Start a durable load-file import from a successful preflight",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if packageImportName == "" {
			return usageError(errors.New("--name is required"))
		}
		operation := packageImportOperation
		if operation == "" {
			operation = uuid.New().String()
		}
		client, err := daemonconn.Ensure(cmd.Context())
		if err != nil {
			return err
		}
		result, err := client.API().CreatePackageImport(cmd.Context(), &apiclient.CreatePackageImportRequestOptions{Body: &api.PackageImportRequest{
			PreflightID: args[0], Into: packageImportInto, Name: packageImportName,
			Party: packageImportParty, OperationID: operation,
			AcceptPartial: packageImportPartial, IndexSuppliedText: packageImportSuppliedText,
		}})
		if err != nil {
			return err
		}
		return writePackageImport(cmd, result)
	},
}

var packageImportStatusCmd = &cobra.Command{
	Use:   "status <operation-id>",
	Short: "Read load-file import progress",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		client, err := daemonconn.Ensure(cmd.Context())
		if err != nil {
			return err
		}
		operation, err := uuid.Parse(args[0])
		if err != nil {
			return usageError(errors.New("operation ID must be a UUID"))
		}
		result, err := client.API().ReadPackageImport(cmd.Context(), &apiclient.ReadPackageImportRequestOptions{
			PathParams: &apiclient.ReadPackageImportPath{OperationID: operation},
		})
		if err != nil {
			return err
		}
		return writePackageImport(cmd, result)
	},
}

var packageImportCancelCmd = &cobra.Command{
	Use:   "cancel <operation-id>",
	Short: "Cancel a queued or running load-file import",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		client, err := daemonconn.Ensure(cmd.Context())
		if err != nil {
			return err
		}
		operation, err := uuid.Parse(args[0])
		if err != nil {
			return usageError(errors.New("operation ID must be a UUID"))
		}
		result, err := client.API().CancelPackageImport(cmd.Context(), &apiclient.CancelPackageImportRequestOptions{
			PathParams: &apiclient.CancelPackageImportPath{OperationID: operation},
		})
		if err != nil {
			return err
		}
		return writePackageImport(cmd, result)
	},
}

var packageListCmd = &cobra.Command{
	Use: "list", Short: "List received and produced load-file packages", Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		if err := validatePackageReadLimit(packageReadLimit); err != nil {
			return usageError(err)
		}
		connection, err := daemonconn.Ensure(cmd.Context())
		if err != nil {
			return err
		}
		query := &apiclient.ListPackagesQuery{Limit: new(int64(packageReadLimit))}
		if packageListDirection != "" {
			direction := apiclient.ListPackagesQueryDirection(packageListDirection)
			query.Direction = &direction
		}
		if packageListAfter != "" {
			query.After = &packageListAfter
		}
		page, err := connection.API().ListPackages(cmd.Context(), &apiclient.ListPackagesRequestOptions{Query: query})
		if err != nil {
			return err
		}
		if packageReadJSON {
			return writeCLIJSON(cmd.OutOrStdout(), page)
		}
		for _, item := range page.Items {
			if _, err = fmt.Fprintf(cmd.OutOrStdout(), "%s\t%s\t%s\t%s\t%d documents\t%d pages\n",
				item.PackageID, item.Direction, item.State, item.PackageName, item.MemberCount, item.PageCount); err != nil {
				return fmt.Errorf("writing package list: %w", err)
			}
		}
		return nil
	},
}

var packageShowCmd = &cobra.Command{
	Use: "show <package-id>", Short: "Show one load-file package", Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if _, err := uuid.Parse(args[0]); err != nil {
			return usageError(errors.New("package ID must be a UUID"))
		}
		connection, err := daemonconn.Ensure(cmd.Context())
		if err != nil {
			return err
		}
		result, err := connection.API().GetPackage(cmd.Context(), &apiclient.GetPackageRequestOptions{
			PathParams: &apiclient.GetPackagePath{PackageID: args[0]},
		})
		if err != nil {
			return err
		}
		if packageReadJSON {
			return writeCLIJSON(cmd.OutOrStdout(), result)
		}
		_, err = fmt.Fprintf(cmd.OutOrStdout(), "%s · %s · %s · %d documents · %d pages\n",
			result.PackageName, result.Direction, result.State, result.MemberCount, result.PageCount)
		if err != nil {
			return fmt.Errorf("writing package: %w", err)
		}
		return nil
	},
}

var packageMembersCmd = &cobra.Command{
	Use: "members <package-id>", Short: "List immutable document occurrences in one package", Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if _, err := uuid.Parse(args[0]); err != nil {
			return usageError(errors.New("package ID must be a UUID"))
		}
		if err := validatePackageReadLimit(packageReadLimit); err != nil {
			return usageError(err)
		}
		connection, err := daemonconn.Ensure(cmd.Context())
		if err != nil {
			return err
		}
		page, err := connection.API().ListPackageMembers(cmd.Context(), &apiclient.ListPackageMembersRequestOptions{
			PathParams: &apiclient.ListPackageMembersPath{PackageID: args[0]},
			Query:      &apiclient.ListPackageMembersQuery{AfterOrdinal: new(int64(packageReadAfterOrdinal)), Limit: new(int64(packageReadLimit))},
		})
		if err != nil {
			return err
		}
		if packageReadJSON {
			return writeCLIJSON(cmd.OutOrStdout(), page)
		}
		for _, item := range page.Items {
			if _, err = fmt.Fprintf(cmd.OutOrStdout(), "%d\t%s\t%s\t%s\n", item.Ordinal, item.RowID, item.DocumentKind, item.DisplayName); err != nil {
				return fmt.Errorf("writing package members: %w", err)
			}
		}
		return nil
	},
}

var packageRecordCmd = &cobra.Command{
	Use: "record <package-id> <row-id>", Short: "Read one immutable sender row", Args: cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		if _, err := uuid.Parse(args[0]); err != nil {
			return usageError(errors.New("package ID must be a UUID"))
		}
		connection, err := daemonconn.Ensure(cmd.Context())
		if err != nil {
			return err
		}
		result, err := connection.API().GetPackageRecord(cmd.Context(), &apiclient.GetPackageRecordRequestOptions{
			PathParams: &apiclient.GetPackageRecordPath{PackageID: args[0], RowID: args[1]},
		})
		if err != nil {
			return err
		}
		if packageReadJSON {
			return writeCLIJSON(cmd.OutOrStdout(), result)
		}
		_, err = fmt.Fprintf(cmd.OutOrStdout(), "%s row %d · %s\n%s\n", result.LoadFile, result.RowOrdinal, result.RowID, result.RawJSON)
		if err != nil {
			return fmt.Errorf("writing package record: %w", err)
		}
		return nil
	},
}

func validatePackageReadLimit(limit int) error {
	if limit != 50 && limit != 100 && limit != 250 {
		return errors.New("--limit must be 50, 100, or 250")
	}
	return nil
}

func writePackageImport(cmd *cobra.Command, result *api.PackageImportJob) error {
	if packageImportJSON {
		return writeCLIJSON(cmd.OutOrStdout(), result)
	}
	_, err := fmt.Fprintf(cmd.OutOrStdout(), "import %s: %s (%d/%d, gaps=%d)\n",
		result.OperationID, result.State, result.Committed, result.Total, result.GapCount)
	if err != nil {
		return fmt.Errorf("writing package import status: %w", err)
	}
	return nil
}

func packagePreflightSource(source string) (string, error) {
	info, err := os.Stat(source)
	if err != nil {
		return "", fmt.Errorf("open package directory: %w", err)
	}
	if !info.IsDir() {
		return "", errors.New("package source must be a directory")
	}
	return filepath.Abs(source)
}

func init() {
	packagePreflightCmd.Flags().StringVar(&packagePreflightProfile, "profile", "", "declared load-file profile")
	packagePreflightCmd.Flags().StringVar(&packagePreflightPageMapProfile, "page-map-profile", "", "page-map profile: opt-standard-v1 (OPT default), opt-pagecount5-v1, or lfp-ipro-v1 (LFP default)")
	packagePreflightCmd.Flags().StringVar(&packagePreflightEncoding, "encoding", "", "declared source encoding")
	packagePreflightCmd.Flags().StringVar(&packagePreflightMap, "map", "", "loadfile-mapping/v1 JSON file")
	packagePreflightCmd.Flags().BoolVar(&packagePreflightJSON, "json", false, "emit machine-readable JSON")
	packageImportCmd.Flags().StringVar(&packageImportInto, "into", "/", "existing destination folder")
	packageImportCmd.Flags().StringVar(&packageImportName, "name", "", "stable package name")
	packageImportCmd.Flags().StringVar(&packageImportParty, "party", "", "sending party label")
	packageImportCmd.Flags().StringVar(&packageImportOperation, "operation-id", "", "version-4 UUID for idempotent retry")
	packageImportCmd.Flags().BoolVar(&packageImportPartial, "accept-partial", false, "retain supported records and report gaps")
	packageImportCmd.Flags().BoolVar(&packageImportSuppliedText, "index-supplied-text", false, "index package-supplied text")
	packageImportCmd.Flags().BoolVar(&packageImportJSON, "json", false, "emit machine-readable JSON")
	packageImportStatusCmd.Flags().BoolVar(&packageImportJSON, "json", false, "emit machine-readable JSON")
	packageImportCancelCmd.Flags().BoolVar(&packageImportJSON, "json", false, "emit machine-readable JSON")
	packageListCmd.Flags().StringVar(&packageListDirection, "direction", "", "filter by received or produced direction")
	packageListCmd.Flags().StringVar(&packageListAfter, "after", "", "continue after the returned package UUID")
	packageListCmd.Flags().IntVar(&packageReadLimit, "limit", 100, "page size: 50, 100, or 250")
	packageListCmd.Flags().BoolVar(&packageReadJSON, "json", false, "emit machine-readable JSON")
	packageShowCmd.Flags().BoolVar(&packageReadJSON, "json", false, "emit machine-readable JSON")
	packageMembersCmd.Flags().IntVar(&packageReadAfterOrdinal, "after-ordinal", 0, "continue after this member ordinal")
	packageMembersCmd.Flags().IntVar(&packageReadLimit, "limit", 100, "page size: 50, 100, or 250")
	packageMembersCmd.Flags().BoolVar(&packageReadJSON, "json", false, "emit machine-readable JSON")
	packageRecordCmd.Flags().BoolVar(&packageReadJSON, "json", false, "emit machine-readable JSON")
	packageCmd.AddCommand(packagePreflightCmd, packageListCmd, packageShowCmd, packageMembersCmd, packageRecordCmd)
	packageImportCmd.AddCommand(packageImportStatusCmd, packageImportCancelCmd)
	packageCmd.AddCommand(packageImportCmd)
	rootCmd.AddCommand(packageCmd)
}

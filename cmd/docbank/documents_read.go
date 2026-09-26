package main

import (
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"
	"uuid"

	"github.com/spf13/cobra"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/daemonconn"
)

var (
	documentsPathPrefix string
	documentsSort       string
	documentsDirection  string
	documentsPageSize   int
	documentsCursor     string
	documentsSourceIDs  []string
	documentsJSON       bool
)

var documentsCmd = &cobra.Command{
	Use: "documents", Short: "Read the current document catalog",
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error { return cmd.Help() },
}

var documentsListCmd = &cobra.Command{
	Use: "list", Short: "Read one bounded page of current documents",
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		if documentsPageSize < 1 || documentsPageSize > 250 {
			return usageError(errors.New("--page-size must be between 1 and 250"))
		}
		if len(documentsPathPrefix) > 16<<10 || utf8.RuneCountInString(documentsPathPrefix) > 16<<10 || !utf8.ValidString(documentsPathPrefix) {
			return usageError(errors.New("--path-prefix exceeds the 16 KiB or 16384 character limit"))
		}
		if !strings.HasPrefix(documentsPathPrefix, "/") {
			return usageError(errors.New("--path-prefix must be absolute"))
		}
		if documentsSort != "path" && documentsSort != "name" && documentsSort != "modified_at" && documentsSort != "size" && documentsSort != "media_type" {
			return usageError(errors.New("--sort must be path, name, modified_at, size, or media_type"))
		}
		if documentsDirection != "asc" && documentsDirection != "desc" {
			return usageError(errors.New("--direction must be asc or desc"))
		}
		if len(documentsCursor) > api.MaxDocumentCursorBytes {
			return usageError(errors.New("--cursor exceeds 32 KiB"))
		}
		if len(documentsSourceIDs) > 4096 {
			return usageError(errors.New("--source-version accepts at most 4096 IDs"))
		}
		seen := make(map[string]struct{}, len(documentsSourceIDs))
		for _, version := range documentsSourceIDs {
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
		query := api.DocumentQuery{
			PathPrefix: documentsPathPrefix, Sort: documentsSort, Direction: documentsDirection,
			PageSize: documentsPageSize, Cursor: documentsCursor,
		}
		var page api.DocumentPage
		if len(documentsSourceIDs) > 0 {
			page, err = c.ListScopedDocuments(cmd.Context(), api.ScopedDocumentQuery{
				DocumentQuery: query, ContentVersionIDs: documentsSourceIDs,
			})
		} else {
			page, err = c.ListDocuments(cmd.Context(), query)
		}
		if err != nil {
			return err
		}
		if documentsJSON {
			return writeCLIJSON(cmd.OutOrStdout(), page)
		}
		for _, item := range page.Items {
			if _, err := fmt.Fprintf(cmd.OutOrStdout(), "%d\t%s\t%s\n", item.NodeID, item.ContentVersionID, item.Path); err != nil {
				return fmt.Errorf("writing document list: %w", err)
			}
		}
		if page.NextCursor != "" {
			_, err = fmt.Fprintln(cmd.ErrOrStderr(), "next cursor:", page.NextCursor)
		}
		if err != nil {
			return fmt.Errorf("writing document cursor: %w", err)
		}
		return nil
	},
}

func init() {
	documentsListCmd.Flags().StringVar(&documentsPathPrefix, "path-prefix", "/", "absolute path prefix")
	documentsListCmd.Flags().StringVar(&documentsSort, "sort", "path", "path, name, modified_at, size, or media_type")
	documentsListCmd.Flags().StringVar(&documentsDirection, "direction", "asc", "asc or desc")
	documentsListCmd.Flags().IntVar(&documentsPageSize, "page-size", 50, "page size (1-250)")
	documentsListCmd.Flags().StringVar(&documentsCursor, "cursor", "", "opaque page cursor")
	documentsListCmd.Flags().StringArrayVar(&documentsSourceIDs, "source-version", nil, "exact content version ID (repeatable, at most 4096)")
	documentsListCmd.Flags().BoolVar(&documentsJSON, "json", false, "emit the complete page as JSON")
	documentsCmd.AddCommand(documentsListCmd)
	rootCmd.AddCommand(documentsCmd)
}

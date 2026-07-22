package main

import (
	"fmt"
	"strconv"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/client"
)

var statJSON bool

var statCmd = &cobra.Command{
	Use:   "stat <path-or-id>",
	Short: "Inspect one document or directory",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		selector, err := parseNodeSelector(args[0])
		if err != nil {
			return err
		}
		c, err := client.Ensure(cmd.Context())
		if err != nil {
			return err
		}
		node, err := selector.resolveIncludingTrash(cmd.Context(), c)
		if err != nil {
			return err
		}
		if statJSON {
			return writeCLIJSON(cmd.OutOrStdout(), node)
		}
		return writeNodeStat(cmd, node)
	},
}

func writeNodeStat(cmd *cobra.Command, node api.Node) error {
	createdAt, err := formatHumanTimestamp(node.CreatedAt)
	if err != nil {
		return err
	}
	modifiedAt, err := formatHumanTimestamp(node.ModifiedAt)
	if err != nil {
		return err
	}

	w := tabwriter.NewWriter(cmd.OutOrStdout(), 2, 4, 2, ' ', 0)
	_, _ = fmt.Fprintf(w, "selector:\t%s\n", formatNodeSelector(node.ID))
	_, _ = fmt.Fprintf(w, "state:\t%s\n", nodeState(node))
	if node.Path != "" {
		_, _ = fmt.Fprintf(w, "path:\t%s\n", strconv.Quote(node.Path))
	}
	_, _ = fmt.Fprintf(w, "name:\t%s\n", strconv.Quote(node.Name))
	_, _ = fmt.Fprintf(w, "kind:\t%s\n", node.Kind)
	_, _ = fmt.Fprintf(w, "revision:\t%d\n", node.Revision)
	_, _ = fmt.Fprintf(w, "created:\t%s\n", createdAt)
	_, _ = fmt.Fprintf(w, "modified:\t%s\n", modifiedAt)
	if node.TrashedAt != "" {
		trashedAt, formatErr := formatHumanTimestamp(node.TrashedAt)
		if formatErr != nil {
			return formatErr
		}
		_, _ = fmt.Fprintf(w, "trashed:\t%s\n", trashedAt)
	}
	if node.Kind == "file" {
		_, _ = fmt.Fprintf(w, "version:\t%s\n", node.CurrentVersionID)
		_, _ = fmt.Fprintf(w, "sha256:\t%s\n", node.BlobHash)
		_, _ = fmt.Fprintf(w, "size:\t%d\n", node.Size)
		mimeType := "not recorded"
		if node.MimeType != "" {
			mimeType = strconv.Quote(node.MimeType)
		}
		_, _ = fmt.Fprintf(w, "mime:\t%s\n", mimeType)
	}
	if err := w.Flush(); err != nil {
		return fmt.Errorf("writing node details: %w", err)
	}
	return nil
}

func nodeState(node api.Node) string {
	if node.TrashedAt != "" {
		return "trashed"
	}
	return "live"
}

func init() {
	statCmd.Flags().BoolVar(&statJSON, "json", false, "emit machine-readable JSON")
	rootCmd.AddCommand(statCmd)
}

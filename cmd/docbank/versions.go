package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"go.kenn.io/docbank/internal/client"
)

const maxVersionsLimit = 1000

var (
	versionsLimit  int
	versionsOffset int
	versionsJSON   bool
	versionJSON    bool
	versionContent bool
)

var versionsCmd = &cobra.Command{
	Use:   "versions <path>",
	Short: "List a file's immutable content versions",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if versionsLimit < 1 || versionsLimit > maxVersionsLimit {
			return fmt.Errorf("--limit must be between 1 and %d", maxVersionsLimit)
		}
		if versionsOffset < 0 {
			return errors.New("--offset must not be negative")
		}
		c, err := client.Ensure(cmd.Context())
		if err != nil {
			return err
		}
		node, err := c.Stat(cmd.Context(), args[0])
		if err != nil {
			return fmt.Errorf("resolving %q: %w", args[0], err)
		}
		page, err := c.Versions(cmd.Context(), node.ID, versionsLimit, versionsOffset)
		if err != nil {
			return err
		}
		if versionsJSON {
			return writeVersionJSON(cmd.OutOrStdout(), page)
		}
		w := tabwriter.NewWriter(cmd.OutOrStdout(), 2, 4, 2, ' ', 0)
		_, _ = fmt.Fprintln(w, "VERSION\tNODE REV\tSIZE\tRECORDED\tKIND\tCURRENT")
		for _, version := range page.Items {
			current := ""
			if version.ID == node.CurrentVersionID {
				current = "yes"
			}
			_, _ = fmt.Fprintf(w, "%s\t%d\t%d\t%s\t%s\t%s\n",
				version.ID, version.NodeRevision, version.Size, version.RecordedAt,
				version.TransitionKind, current)
		}
		if err := w.Flush(); err != nil {
			return fmt.Errorf("writing content versions: %w", err)
		}
		if versionsOffset+len(page.Items) < page.Total {
			_, _ = fmt.Fprintf(cmd.OutOrStdout(),
				"showing %d-%d of %d (use --offset to continue)\n",
				versionsOffset+1, versionsOffset+len(page.Items), page.Total)
		}
		return nil
	},
}

var versionCmd = &cobra.Command{
	Use:   "version <version-id>",
	Short: "Inspect or stream one immutable content version",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if versionJSON && versionContent {
			return errors.New("--json and --content cannot be combined")
		}
		c, err := client.Ensure(cmd.Context())
		if err != nil {
			return err
		}
		if versionContent {
			stream, err := c.VersionContent(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			defer func() { _ = stream.Close() }()
			if _, err := stream.CopyVerified(cmd.OutOrStdout()); err != nil {
				return fmt.Errorf("streaming content version %s: %w", args[0], err)
			}
			return nil
		}
		version, err := c.Version(cmd.Context(), args[0])
		if err != nil {
			return err
		}
		if versionJSON {
			return writeVersionJSON(cmd.OutOrStdout(), version)
		}
		w := tabwriter.NewWriter(cmd.OutOrStdout(), 2, 4, 2, ' ', 0)
		_, _ = fmt.Fprintf(w, "Version:\t%s\n", version.ID)
		_, _ = fmt.Fprintf(w, "Node:\t%d\n", version.NodeID)
		_, _ = fmt.Fprintf(w, "Node revision:\t%d\n", version.NodeRevision)
		_, _ = fmt.Fprintf(w, "Recorded:\t%s\n", version.RecordedAt)
		_, _ = fmt.Fprintf(w, "Kind:\t%s\n", version.TransitionKind)
		_, _ = fmt.Fprintf(w, "Blob:\t%s\n", version.BlobHash)
		_, _ = fmt.Fprintf(w, "Size:\t%d\n", version.Size)
		if version.MimeType != "" {
			_, _ = fmt.Fprintf(w, "Media type:\t%s\n", version.MimeType)
		}
		return w.Flush()
	},
}

func writeVersionJSON(w io.Writer, value any) error {
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(value); err != nil {
		return fmt.Errorf("writing content-version JSON: %w", err)
	}
	return nil
}

func init() {
	versionsCmd.Flags().IntVar(&versionsLimit, "limit", 100, "maximum versions to return (1-1000)")
	versionsCmd.Flags().IntVar(&versionsOffset, "offset", 0, "number of newest versions to skip")
	versionsCmd.Flags().BoolVar(&versionsJSON, "json", false, "emit machine-readable JSON")
	versionCmd.Flags().BoolVar(&versionJSON, "json", false, "emit machine-readable JSON")
	versionCmd.Flags().BoolVar(&versionContent, "content", false, "write this version's bytes to stdout")
	rootCmd.AddCommand(versionsCmd, versionCmd)
}

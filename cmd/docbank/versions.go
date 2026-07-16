package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/client"
)

const maxVersionsLimit = 1000

var (
	versionsLimit   int
	versionsOffset  int
	versionsJSON    bool
	versionJSON     bool
	versionContent  bool
	pruneVersionIDs []string
	pruneKeepNewest int
	pruneOlderThan  string
	pruneAllPrior   bool
	pruneRun        bool
	pruneJSON       bool
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
		if version.SourceVersionID != nil {
			_, _ = fmt.Fprintf(w, "Source version:\t%s\n", *version.SourceVersionID)
		}
		return w.Flush()
	},
}

var versionsPruneCmd = &cobra.Command{
	Use:   "prune <path>",
	Short: "Preview or release selected version history",
	Long: "Release selected immutable history while retaining the current content. " +
		"This changes logical reachability only: run gc for loose bytes and storage repack " +
		"for dead packed space. The default is a dry run.",
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		request := api.VersionPruneRequest{
			VersionIDs: pruneVersionIDs, KeepNewest: pruneKeepNewest,
			OlderThan: pruneOlderThan, AllPrior: pruneAllPrior, Run: pruneRun,
		}
		if err := validatePruneFlags(request); err != nil {
			return err
		}
		c, err := client.Ensure(cmd.Context())
		if err != nil {
			return err
		}
		node, err := c.Stat(cmd.Context(), args[0])
		if err != nil {
			return fmt.Errorf("resolving %q: %w", args[0], err)
		}
		report, err := c.PruneContentVersions(
			cmd.Context(), node.ID, node.Revision, request,
		)
		if err != nil {
			return fmt.Errorf("pruning versions of %q: %w", args[0], err)
		}
		if pruneJSON {
			return writeVersionJSON(cmd.OutOrStdout(), report)
		}
		writeVersionPruneReport(cmd, report)
		return nil
	},
}

func validatePruneFlags(request api.VersionPruneRequest) error {
	if request.KeepNewest < 0 {
		return errors.New("--keep-newest must be positive")
	}
	modes := 0
	if len(request.VersionIDs) > 0 {
		modes++
	}
	if request.KeepNewest > 0 {
		modes++
	}
	if request.OlderThan != "" {
		modes++
		age, err := api.ParseAge(request.OlderThan)
		if err != nil {
			return err
		}
		if age == 0 {
			return errors.New("--older-than must be greater than zero")
		}
	}
	if request.AllPrior {
		modes++
	}
	if modes != 1 {
		return errors.New("choose exactly one of --version, --keep-newest, --older-than, or --all-prior")
	}
	return nil
}

func writeVersionPruneReport(cmd *cobra.Command, report api.VersionPruneReport) {
	_, _ = fmt.Fprintf(cmd.OutOrStdout(),
		"%d version(s) selected, %d logical byte(s), %d unique blob(s)\n",
		len(report.Candidates), report.LogicalBytes, report.UniqueBlobs)
	if len(report.DependencyRetained) > 0 {
		_, _ = fmt.Fprintf(cmd.OutOrStdout(),
			"retained %d selected source version(s) required by remaining reverts\n",
			len(report.DependencyRetained))
	}
	if report.CheckpointRequired {
		_, _ = fmt.Fprintln(cmd.OutOrStdout(),
			"the current revert will be replaced by a same-byte checkpoint before pruning")
	}
	if report.ReleasableBlobs > 0 {
		_, _ = fmt.Fprintf(cmd.OutOrStdout(),
			"%d blob(s), %d byte(s) become eligible for later physical maintenance\n",
			report.ReleasableBlobs, report.ReleasableBytes)
	}
	if report.LooseBlobsPendingGC > 0 {
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "%d loose blob(s), %d byte(s) pending gc\n",
			report.LooseBlobsPendingGC, report.LooseBytesPendingGC)
	}
	if report.PackedBlobsPendingRepack > 0 {
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "%d packed blob(s), %d stored byte(s) pending gc then repack\n",
			report.PackedBlobsPendingRepack, report.PackedBytesPendingRepack)
	}
	if !report.Run {
		if len(report.Candidates) > 0 {
			_, _ = fmt.Fprintln(cmd.OutOrStdout(), "dry run — pass --run to prune")
		}
		return
	}
	if report.Changed {
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "pruned %d version(s); node revision is now %d\n",
			report.DeletedVersions, report.Node.Revision)
	}
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
	versionsPruneCmd.Flags().StringArrayVar(&pruneVersionIDs, "version", nil,
		"select one version UUID (repeatable; commas are literal)")
	versionsPruneCmd.Flags().IntVar(&pruneKeepNewest, "keep-newest", 0,
		"retain at least this many newest versions")
	versionsPruneCmd.Flags().StringVar(&pruneOlderThan, "older-than", "",
		"select versions at least this old (e.g. 90d or 12h)")
	versionsPruneCmd.Flags().BoolVar(&pruneAllPrior, "all-prior", false,
		"remove the complete prior history while retaining current content")
	versionsPruneCmd.Flags().BoolVar(&pruneRun, "run", false,
		"actually prune (default is dry-run)")
	versionsPruneCmd.Flags().BoolVar(&pruneJSON, "json", false, "emit a machine-readable report")
	versionsCmd.AddCommand(versionsPruneCmd)
	rootCmd.AddCommand(versionsCmd, versionCmd)
}

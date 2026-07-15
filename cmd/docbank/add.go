package main

import (
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"strconv"
	"unicode/utf8"

	"github.com/spf13/cobra"

	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/client"
)

var (
	addDest      string
	addExclude   []string
	addPreflight bool
	addJSON      bool
	addProgress  string
)

var addCmd = &cobra.Command{
	Use:   "add <path>...",
	Short: "Import files or directory trees into the vault",
	Args:  cobra.MinimumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		abs := make([]string, len(args))
		for i, a := range args {
			// Windows absolute-path resolution converts through UTF-16 and can
			// replace invalid source bytes with U+FFFD. Reject before any OS path
			// normalization so the CLI cannot silently target another filename.
			if !utf8.ValidString(a) {
				return fmt.Errorf("ingest source path %s is not valid UTF-8", strconv.QuoteToASCII(a))
			}
			p, err := filepath.Abs(a)
			if err != nil {
				return fmt.Errorf("resolving %q: %w", a, err)
			}
			abs[i] = p
		}
		c, err := client.Ensure(cmd.Context())
		if err != nil {
			return err
		}
		if addPreflight {
			rep, err := c.PreflightIngest(cmd.Context(), abs, addExclude)
			if err != nil {
				return err
			}
			if addJSON {
				enc := json.NewEncoder(cmd.OutOrStdout())
				enc.SetIndent("", "  ")
				if err := enc.Encode(rep); err != nil {
					return fmt.Errorf("encoding preflight report: %w", err)
				}
			} else {
				printIngestPreflight(cmd, rep)
			}
			if rep.Errors > 0 || rep.Rejected.Files > 0 {
				return fmt.Errorf("preflight found %d error(s) and %d file(s) above the ingest limit",
					rep.Errors, rep.Rejected.Files)
			}
			return nil
		}
		var rep api.IngestReport
		if addJSON {
			rep, err = c.IngestWithOptions(cmd.Context(), abs, addDest, addExclude)
		} else {
			mode, modeErr := progressModeFromFlag("add", addProgress)
			if modeErr != nil {
				return modeErr
			}
			renderer := newIngestProgressRenderer(cmd.ErrOrStderr(), mode)
			defer renderer.finish()
			rep, err = c.IngestStream(cmd.Context(), abs, addDest, addExclude, renderer.handle)
		}
		if err != nil {
			return err
		}
		if addJSON {
			enc := json.NewEncoder(cmd.OutOrStdout())
			enc.SetIndent("", "  ")
			if err := enc.Encode(rep); err != nil {
				return fmt.Errorf("encoding ingest report: %w", err)
			}
		} else {
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "added: %d  skipped: %d  excluded: %d  failed: %d\n",
				rep.Added, rep.Skipped, rep.Excluded, len(rep.Failed))
			for _, f := range rep.Failed {
				_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "failed: %s: %s\n", f.Path, f.Error)
			}
		}
		if len(rep.Failed) > 0 {
			return fmt.Errorf("%d file(s) failed to import", len(rep.Failed))
		}
		return nil
	},
}

func init() {
	addCmd.Flags().StringVar(&addDest, "dest", "/inbox", "virtual destination directory")
	addCmd.Flags().StringArrayVar(&addExclude, "exclude", nil,
		"exclude an entry name anywhere or a relative path within each source (repeatable)")
	addCmd.Flags().BoolVar(&addPreflight, "preflight", false,
		"inventory sources without opening content or changing the vault")
	addCmd.Flags().BoolVar(&addJSON, "json", false,
		"machine-readable terminal report (progress suppressed)")
	addCmd.Flags().StringVar(&addProgress, "progress", "auto",
		"progress output mode: auto, bar, or plain (suppressed by --json)")
	rootCmd.AddCommand(addCmd)
}

type ingestProgressRenderer struct {
	base *backupProgressRenderer
}

func newIngestProgressRenderer(out io.Writer, mode backupProgressMode) *ingestProgressRenderer {
	return &ingestProgressRenderer{base: newBackupProgressRenderer(out, mode)}
}

func (r *ingestProgressRenderer) handle(event api.IngestProgress) {
	r.base.handle(api.BackupProgress{
		Stage: event.Stage, Done: event.Done, Total: event.Total,
		BytesDone: event.BytesDone, BytesTotal: event.BytesTotal, Final: event.Final,
	})
}

func (r *ingestProgressRenderer) finish() {
	r.base.finish()
}

func printIngestPreflight(cmd *cobra.Command, rep api.IngestPreflightReport) {
	out := cmd.OutOrStdout()
	_, _ = fmt.Fprintf(out, "files: %d  directories: %d  logical size: %s\n",
		rep.Files, rep.Directories, formatBackupBytes(rep.LogicalBytes))
	_, _ = fmt.Fprintf(out, "pack eligible: %d file(s), %s\n",
		rep.PackEligible.Files, formatBackupBytes(rep.PackEligible.Bytes))
	_, _ = fmt.Fprintf(out, "loose only: %d file(s), %s\n",
		rep.LooseOnly.Files, formatBackupBytes(rep.LooseOnly.Bytes))
	_, _ = fmt.Fprintf(out, "rejected: %d file(s), %s\n",
		rep.Rejected.Files, formatBackupBytes(rep.Rejected.Bytes))
	_, _ = fmt.Fprintf(out, "excluded: %d  skipped non-regular: %d  errors: %d\n",
		rep.Excluded, rep.Skipped, rep.Errors)

	if len(rep.FileTypes) > 0 {
		_, _ = fmt.Fprintln(out, "largest file types:")
		limit := min(len(rep.FileTypes), 12)
		for _, fileType := range rep.FileTypes[:limit] {
			name := fileType.Extension
			if name == "" {
				name = "(no extension)"
			}
			_, _ = fmt.Fprintf(out, "  %-16s %8d file(s)  %s\n",
				name, fileType.Files, formatBackupBytes(fileType.Bytes))
		}
		if len(rep.FileTypes) > limit || rep.FileTypesTruncated {
			_, _ = fmt.Fprintln(out, "  ... use --json for the full bounded summary")
		}
	}
	for _, finding := range rep.Findings {
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "%s: %s: %s\n",
			finding.Kind, finding.Path, finding.Detail)
	}
	if rep.FindingsTruncated {
		_, _ = fmt.Fprintln(cmd.ErrOrStderr(), "additional findings omitted; counts above are complete")
	}
}

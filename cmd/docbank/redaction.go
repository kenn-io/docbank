package main

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/spf13/cobra"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/apiclient"
	"go.kenn.io/docbank/internal/daemonconn"
	"go.kenn.io/docbank/internal/processing"
	"go.kenn.io/docbank/internal/store"
)

func init() { rootCmd.AddCommand(newRedactionCommand()) }

func newRedactionCommand() *cobra.Command {
	command := &cobra.Command{Use: "redaction", Short: "Review and redact retained PDFs",
		GroupID: groupProductions,
		Long:    "Prepare an exact retained native PDF, resolve explicit masks, then render\nthe reviewed result as one verified ZIP."}
	var version string
	var jsonOutput bool
	prepare := &cobra.Command{Use: "prepare <path-or-id>", Short: "Inspect one retained native PDF",
		Long: "Requires the configured Linux amd64 page runtime. JSON contains unredacted text.\n" +
			"Preparation preserves coverage gaps; it does not approve or apply redactions.",
		Example: "  docbank redaction prepare /inbox/source.pdf\n" +
			"  docbank redaction prepare id:42 --version <version-id> --json",
		Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
			selector, err := parseNodeSelector(args[0])
			if err != nil {
				return err
			}
			if version != "" && !daemonconn.IsCanonicalUUIDv4(version) {
				return usageError(errors.New("--version must be a canonical lowercase UUIDv4"))
			}
			connection, err := daemonconn.Ensure(cmd.Context())
			if err != nil {
				return err
			}
			defer func() { _ = connection.Close() }()
			selection, err := selectPreparationSource(cmd.Context(), connection, selector, version)
			if err != nil {
				return err
			}
			result, err := connection.PrepareNativePDF(cmd.Context(),
				processing.RedactionPreparationRequest{Selection: selection})
			if err != nil {
				return redactionPreparationCLIError(err)
			}
			if jsonOutput {
				return writeCLIJSON(cmd.OutOrStdout(), result)
			}
			return writeRedactionPreparation(cmd.OutOrStdout(), result)
		}}
	prepare.Flags().StringVar(&version, "version", "", "retained content version (defaults to current)")
	prepare.Flags().BoolVar(&jsonOutput, "json", false, "print JSON including original text")
	command.AddCommand(prepare, newRedactionResolveCommand(), newRedactionRenderCommand())
	return command
}

func selectPreparationSource(ctx context.Context, connection *daemonconn.Connection,
	selector nodeSelector, versionID string,
) (store.PageBinding, error) {
	node, err := selector.resolve(ctx, connection)
	if err != nil {
		return store.PageBinding{}, err
	}
	if node.Kind == "dir" {
		return store.PageBinding{}, fmt.Errorf("%q is a directory: %w", selector.raw, store.ErrNotFound)
	}
	source := document.PageSource{VersionID: node.CurrentVersionID, SHA256: node.BlobHash, Size: node.Size}
	if versionID != "" {
		version, err := connection.API().GetContentVersion(ctx, &apiclient.GetContentVersionRequestOptions{
			PathParams: &apiclient.GetContentVersionPath{VersionID: versionID}})
		if err != nil {
			return store.PageBinding{}, err
		}
		if version.NodeID != node.ID {
			return store.PageBinding{}, fmt.Errorf("version %q does not belong to %q: %w",
				versionID, selector.raw, store.ErrNotFound)
		}
		source = document.PageSource{VersionID: version.ID, SHA256: version.BlobHash, Size: version.Size}
	}
	return store.PageBinding{NodeID: node.ID, Revision: node.Revision, Source: source}, nil
}

func redactionPreparationCLIError(err error) error {
	if errors.Is(err, processing.ErrRedactionPreparationInvalid) {
		return usageError(err)
	}
	code, _ := daemonconn.ProblemCode(err)
	switch code {
	case "invalid_redaction_preparation":
		return usageError(err)
	case "redaction_preparation_memory":
		return withHint(&exitError{code: exitBusy, err: err}, "retry when daemon memory use is lower")
	case "redaction_preparation_busy":
		return withHint(&exitError{code: exitBusy, err: err}, "wait for the active preparation to finish, then retry")
	default:
		return err
	}
}

func writeRedactionPreparation(output io.Writer, result processing.RedactionPreparation) error {
	if _, err := fmt.Fprintf(output, "Node: %d (revision %d)\nVersion: %s\nSource SHA-256: %s\n"+
		"Evidence SHA-256: %s\nMap SHA-256: %s\n", result.Selection.NodeID, result.Selection.Revision,
		result.Selection.Source.VersionID, result.Selection.Source.SHA256, result.EvidenceSHA256,
		result.Map.SHA256); err != nil {
		return fmt.Errorf("writing native PDF preparation: %w", err)
	}
	for _, page := range result.Map.Pages {
		atoms, ordered, unordered := 0, 0, 0
		for _, atom := range result.Map.Atoms {
			for _, box := range atom.Boxes {
				if box.Page == page.Number {
					atoms++
					break
				}
			}
		}
		for _, gap := range result.Map.Gaps {
			if gap.Box.Page == page.Number {
				if gap.Unordered {
					unordered++
				} else {
					ordered++
				}
			}
		}
		if _, err := fmt.Fprintf(output, "Page %d: bytes [%d,%d), %d atoms, %d ordered gaps, %d unordered gaps\n",
			page.Number, page.Span.Start, page.Span.End, atoms, ordered, unordered); err != nil {
			return fmt.Errorf("writing native PDF preparation: %w", err)
		}
	}
	_, err := fmt.Fprintln(output, "Ordered gaps need covering masks; unordered gaps need whole-page masks.\n"+
		"Zero gaps do not approve redaction.")
	if err != nil {
		return fmt.Errorf("writing native PDF preparation: %w", err)
	}
	return nil
}

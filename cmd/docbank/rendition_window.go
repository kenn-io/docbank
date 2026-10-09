package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"
	"uuid"

	"github.com/spf13/cobra"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/daemonconn"
	"go.kenn.io/docbank/internal/store"
)

type renditionWindowOptions struct {
	selector            nodeSelector
	version             uuid.UUID
	profile, attachment string
	offset, maxChars    int
	asJSON              bool
}

func newRenditionWindowCommand() *cobra.Command {
	cmd := &cobra.Command{
		Example: `  docbank stat id:12 # version: <version-uuid>
  docbank rendition window id:12 --version <version-uuid> --profile <profile>`,
		Long: `--version is the current version UUID (docbank stat); --profile names the
profile that built it. Prints a continuation command when more text remains.`,
		Use:   "window <path-or-id>",
		Short: "Read a bounded slice of a document's derived text",
		Args:  cobra.ExactArgs(1)}
	var options renditionWindowOptions
	var version string
	cmd.Flags().StringVar(&version, "version", "", "exact content-version UUID (required)")
	cmd.Flags().StringVar(&options.profile, "profile", "", "executable processing profile (required)")
	cmd.Flags().StringVar(&options.attachment, "attachment", "", "pin an active rendition attachment")
	cmd.Flags().IntVar(&options.offset, "offset", 0, "unicode character offset (0–2147483647)")
	cmd.Flags().IntVar(&options.maxChars, "max-chars", 8000, "maximum characters to read (1–16000)")
	cmd.Flags().BoolVar(&options.asJSON, "json", false, "print JSON to stdout")
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		if err := validateInspectionSelection(version, options.profile); err != nil {
			return err
		}
		if options.offset < 0 || options.offset > 1<<31-1 ||
			options.maxChars < 1 || options.maxChars > 16000 {
			return usageError(errors.New("offset must be 0–2147483647 and max-chars must be 1–16000"))
		}
		if cmd.Flags().Changed("attachment") && !canonicalSHA256(options.attachment) {
			return usageError(errors.New("attachment must be 64 lowercase hexadecimal characters"))
		}
		if options.offset > 0 && options.attachment == "" {
			return usageError(errors.New("--offset greater than zero requires --attachment"))
		}
		var err error
		options.selector, err = parseNodeSelector(args[0])
		if err != nil {
			return err
		}
		options.version, err = uuid.Parse(version)
		if err != nil {
			return usageError(err)
		}
		c, err := daemonconn.Ensure(cmd.Context())
		if err != nil {
			return err
		}
		defer func() { _ = c.Close() }()
		return runRenditionWindow(cmd, c, options)
	}
	return cmd
}

func runRenditionWindow(
	cmd *cobra.Command, c *daemonconn.Connection, options renditionWindowOptions,
) error {
	node, err := options.selector.resolve(cmd.Context(), c)
	if err != nil {
		return err
	}
	if node.Kind != "file" {
		return usageError(errors.New("rendition window requires a file"))
	}
	if node.CurrentVersionID != options.version.String() {
		return errors.New("document version changed; refresh and reselect the document")
	}
	profile, err := windowProfileFingerprint(cmd.Context(), c, options.profile)
	if err != nil {
		return err
	}
	attachment, build := options.attachment, ""
	if attachment == "" {
		selected, err := discoverWindowRendition(cmd.Context(), c, node, profile)
		if err != nil {
			return err
		}
		attachment, build = selected.AttachmentID, selected.BuildID
	}
	vaultID, err := inspectionVaultID(cmd.Context(), c)
	if err != nil {
		return err
	}
	window, err := c.RenditionTextWindow(cmd.Context(), api.RenditionWindowRequest{
		VaultID: vaultID.String(), NodeID: node.ID, ContentVersionID: options.version.String(),
		AttachmentID: attachment, Offset: options.offset, MaxChars: options.maxChars,
	})
	if err != nil {
		return err
	}
	if build != "" && (window.BuildID != build || window.ProfileFingerprint != profile) {
		return errors.New("rendition window does not match the selected build and profile")
	}
	if window.ProfileFingerprint != profile {
		return fmt.Errorf("attachment does not belong to profile %q: %w", options.profile, store.ErrNotFound)
	}
	if options.asJSON {
		return writeCLIJSON(cmd.OutOrStdout(), window)
	}
	return writeRenditionWindow(cmd.OutOrStdout(), options, window)
}

func windowProfileFingerprint(
	ctx context.Context, c *daemonconn.Connection, name string,
) (string, error) {
	profiles, err := c.API().ListDocumentProcessingProfiles(ctx)
	if err != nil {
		return "", err
	}
	if profiles == nil {
		return "", errors.New("daemon returned an invalid processing profile list")
	}
	for _, profile := range *profiles {
		if profile.Name == name {
			return profile.Fingerprint, nil
		}
	}
	return "", inspectionProfileError(name, errors.New("processing_profile_unavailable"))
}

func discoverWindowRendition(ctx context.Context, c *daemonconn.Connection,
	node api.Node, profile string,
) (api.DocumentRenditionIdentity, error) {
	var selected api.DocumentRenditionIdentity
	if err := checkWindowCatalogBounds(node); err != nil {
		return selected, err
	}
	items, err := c.ResolveDocumentSummaries(ctx, api.DocumentSummaryResolveRequest{
		Identities: []api.DocumentIdentity{{NodeID: node.ID,
			ContentVersionID: node.CurrentVersionID, Path: node.Path}},
	})
	if err != nil {
		if code, _ := daemonconn.ProblemCode(err); code == "stale_version" {
			return selected, fmt.Errorf("document selection changed; refresh and reselect: %w", err)
		}
		return selected, err
	}
	for _, rendition := range items[0].ActiveRenditions {
		if rendition.ProfileFingerprint != profile {
			continue
		}
		if selected.AttachmentID != "" {
			return selected, errors.New("document summary has multiple active renditions for one profile")
		}
		selected = rendition
	}
	if selected.AttachmentID == "" {
		return selected, fmt.Errorf("no active rendition for the selected profile: %w", store.ErrNotFound)
	}
	return selected, nil
}

func checkWindowCatalogBounds(node api.Node) error {
	if _, err := store.NormalizeDocumentCatalogQuery(store.DocumentCatalogQuery{PathPrefix: node.Path}); err != nil {
		return fmt.Errorf("rendition discovery exceeds document catalog path limits: %w", err)
	}
	if utf8.RuneCountInString(node.Name) > store.MaxDocumentCatalogNameCharacters {
		return fmt.Errorf("rendition discovery exceeds the document catalog filename limit of %d "+
			"characters; use --attachment if the active attachment is already known",
			store.MaxDocumentCatalogNameCharacters)
	}
	return nil
}

func writeRenditionWindow(
	w io.Writer, options renditionWindowOptions, window api.RenditionTextWindow,
) error {
	var text strings.Builder
	fmt.Fprintf(&text, "version: %s\nattachment: %s\nbuild: %s\nprofile: %q\nfingerprint: %s\n"+
		"checksum: %s\ncharacters: [%d, %d)\n\n", window.ContentVersionID, window.AttachmentID,
		window.BuildID, options.profile, window.ProfileFingerprint, window.Checksum,
		window.ActualStart, window.ActualEnd)
	text.WriteString(window.Text)
	if !window.EOF {
		fmt.Fprintf(&text, "\n\nNext window: docbank rendition window id:%d --version %s "+
			"--profile %s --attachment %s --offset %d --max-chars %d\n",
			window.NodeID, window.ContentVersionID, options.profile, window.AttachmentID,
			window.NextOffset, options.maxChars)
	}
	_, err := io.WriteString(w, text.String())
	return err
}

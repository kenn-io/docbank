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
	"go.kenn.io/docbank/internal/apiclient"
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
		Example: `  docbank rendition window id:12
  docbank rendition window id:12 --version <version-uuid> --profile <profile>`,
		Long: `Reads the current version and selects its readable profile by default.
Prints a continuation command when more text remains.`,
		Use:   "window <path-or-id>",
		Short: "Read a bounded slice of a document's derived text",
		Args:  cobra.ExactArgs(1)}
	var options renditionWindowOptions
	var version string
	cmd.Flags().StringVar(&version, "version", "", "current content-version UUID (default: current version)")
	cmd.Flags().StringVar(&options.profile, "profile", "", "processing profile (default: unique readable rendition)")
	cmd.Flags().StringVar(&options.attachment, "attachment", "", "pin an active rendition attachment")
	cmd.Flags().IntVar(&options.offset, "offset", 0, "unicode character offset (0–2147483647)")
	cmd.Flags().IntVar(&options.maxChars, "max-chars", 8000, "maximum characters to read (1–16000)")
	cmd.Flags().BoolVar(&options.asJSON, "json", false, "print JSON to stdout")
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		options.version = uuid.Nil()
		if cmd.Flags().Changed("version") {
			if err := validateInspectionVersion(version); err != nil {
				return err
			}
		}
		if cmd.Flags().Changed("profile") {
			if err := validateInspectionProfile(options.profile); err != nil {
				return err
			}
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
		if version != "" {
			options.version, err = uuid.Parse(version)
			if err != nil {
				return usageError(err)
			}
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
	if options.version == uuid.Nil() {
		options.version, err = uuid.Parse(node.CurrentVersionID)
		if err != nil {
			return fmt.Errorf("invalid current version: %w", err)
		}
	}
	if node.CurrentVersionID != options.version.String() {
		return errors.New("document version changed; refresh and reselect the document")
	}
	includeConfigured := true
	profiles, err := c.API().ListDocumentProcessingProfiles(cmd.Context(), &apiclient.ListDocumentProcessingProfilesRequestOptions{Query: &apiclient.ListDocumentProcessingProfilesQuery{IncludeConfigured: &includeConfigured}})
	if err != nil {
		return err
	}
	if profiles == nil {
		return errors.New("daemon returned an invalid processing profile list")
	}
	var profile string
	attachment, build := options.attachment, ""
	if options.profile == "" && attachment == "" {
		selected, err := selectWindowProfile(cmd.Context(), c, node, options.version, *profiles)
		if err != nil {
			return err
		}
		profile, attachment, build = selected.ProfileFingerprint, selected.AttachmentID, selected.BuildID
	} else if options.profile != "" {
		profile, err = windowProfileFingerprint(*profiles, options.profile)
		if err != nil {
			return err
		}
	}
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
	if build == "" && profile != "" && window.ProfileFingerprint != profile {
		return fmt.Errorf("attachment does not belong to profile %q: %w", options.profile, store.ErrNotFound)
	}
	if options.profile == "" {
		options.profile = windowProfileName(*profiles, window.ProfileFingerprint)
	}
	if options.asJSON {
		return writeCLIJSON(cmd.OutOrStdout(), window)
	}
	return writeRenditionWindow(cmd.OutOrStdout(), options, window)
}

func windowProfileFingerprint(profiles []api.ProcessingProfileSummary, name string) (string, error) {
	for _, profile := range profiles {
		if profile.Name == name {
			return profile.Fingerprint, nil
		}
	}
	return "", fmt.Errorf("profile %q is not configured or executable: %w", name, store.ErrNotFound)
}

func windowProfileName(profiles []api.ProcessingProfileSummary, fingerprint string) string {
	for _, profile := range profiles {
		if profile.Fingerprint == fingerprint {
			return profile.Name
		}
	}
	return fingerprint
}

func selectWindowProfile(ctx context.Context, c *daemonconn.Connection, node api.Node,
	version uuid.UUID, profiles []api.ProcessingProfileSummary,
) (api.DocumentRenditionIdentity, error) {
	var selected api.DocumentRenditionIdentity
	renditions, err := windowRenditions(ctx, c, node)
	if err != nil {
		return selected, err
	}
	var names, pins []string
	exact := true
	for _, rendition := range renditions {
		fingerprint := rendition.ProfileFingerprint
		receipt, err := c.API().ResolveRenditionText(ctx, &apiclient.ResolveRenditionTextRequestOptions{Body: &apiclient.RenditionTextRequest{
			NodeID: node.ID, Revision: node.Revision, VersionID: version, BlobHash: node.BlobHash, Size: node.Size, ExactRendition: &exact,
			Observed: &apiclient.RenditionTextObserved{Configuration: apiclient.Configured,
				ProfileFingerprint: &fingerprint, AttachmentID: &rendition.AttachmentID, BuildID: &rendition.BuildID},
		}})
		if err != nil {
			if code, _ := daemonconn.ProblemCode(err); code != "rendition_text_too_large" {
				return selected, err
			}
		} else if receipt == nil {
			return selected, errors.New("daemon returned an invalid rendition text receipt")
		} else if receipt.State != apiclient.Ready {
			continue
		}
		name := windowProfileName(profiles, fingerprint)
		names = append(names, name)
		if name == fingerprint {
			pins = append(pins, name+": --attachment "+rendition.AttachmentID)
		} else {
			pins = append(pins, name+": --profile "+name)
		}
		selected = rendition
	}
	if len(names) == 0 {
		return selected, fmt.Errorf("no readable rendition is currently available for this version: %w", store.ErrNotFound)
	}
	if len(names) > 1 {
		return selected, usageError(fmt.Errorf("multiple readable profiles: %s; choose %s", strings.Join(names, ", "), strings.Join(pins, "; ")))
	}
	return selected, nil
}

func discoverWindowRendition(ctx context.Context, c *daemonconn.Connection,
	node api.Node, profile string,
) (api.DocumentRenditionIdentity, error) {
	var selected api.DocumentRenditionIdentity
	renditions, err := windowRenditions(ctx, c, node)
	if err != nil {
		return selected, err
	}
	for _, rendition := range renditions {
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

func windowRenditions(ctx context.Context, c *daemonconn.Connection, node api.Node) ([]api.DocumentRenditionIdentity, error) {
	if err := checkWindowCatalogBounds(node); err != nil {
		return nil, err
	}
	items, err := c.ResolveDocumentSummaries(ctx, api.DocumentSummaryResolveRequest{
		Identities: []api.DocumentIdentity{{NodeID: node.ID, ContentVersionID: node.CurrentVersionID, Path: node.Path}},
	})
	if err != nil {
		if code, _ := daemonconn.ProblemCode(err); code == "stale_version" {
			return nil, fmt.Errorf("document selection changed; refresh and reselect: %w", err)
		}
		return nil, err
	}
	return items[0].ActiveRenditions, nil
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
		fmt.Fprintf(&text, "\n\nNext window: docbank rendition window id:%d --version %s", window.NodeID, window.ContentVersionID)
		fmt.Fprintf(&text, " --attachment %s --offset %d --max-chars %d\n", window.AttachmentID, window.NextOffset, options.maxChars)
	}
	_, err := io.WriteString(w, text.String())
	return err
}

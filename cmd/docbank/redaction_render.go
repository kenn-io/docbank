package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"uuid"

	"github.com/spf13/cobra"
	"go.kenn.io/docbank/internal/daemonconn"
	"go.kenn.io/docbank/internal/filepublish"
	"go.kenn.io/docbank/internal/home"
	"go.kenn.io/docbank/internal/processing"
)

func newRedactionRenderCommand() *cobra.Command {
	var overwrite, asJSON bool
	var allocation string
	cmd := &cobra.Command{Use: "render <preparation.json> <masks.json> <review.json> <output.zip>", Short: "Verify and save a reviewed redacted PDF, text and receipt", Args: cobra.ExactArgs(4),
		Example: "  docbank redaction render preparation.json masks.json review.json redacted.zip",
		RunE: func(cmd *cobra.Command, args []string) error {
			if allocation != "" && !daemonconn.IsCanonicalUUIDv4(allocation) {
				return usageError(errors.New("--allocation must be a canonical lowercase UUIDv4"))
			}
			inputs, err := readRedactionInputs(args[0], args[1])
			if err != nil {
				return err
			}
			review, err := readRedactionReview(args[2], inputs)
			if err != nil {
				return err
			}
			destination, err := prepareGetDestination(args[3], overwrite)
			if err != nil {
				return err
			}
			layout, err := home.Resolve()
			if err != nil {
				return err
			}
			inside, err := layout.ContainsDirectory(filepath.Dir(destination))
			if err != nil {
				return err
			}
			if inside {
				return usageError(errors.New("redaction destination must be outside the Docbank data directory"))
			}
			connection, err := daemonconn.Ensure(cmd.Context())
			if err != nil {
				return err
			}
			defer func() { _ = connection.Close() }()
			request := processing.RedactionRenderRequest{Preparation: inputs.Preparation, Masks: inputs.Masks, ReviewSHA256: review.SHA256}
			if allocation != "" {
				request.AllocationID = uuid.MustParse(allocation)
			}
			err = redactionCLIError(publishRedaction(cmd, destination, overwrite, asJSON, func(ctx context.Context, file *os.File) (processing.RedactionArchive, error) {
				return connection.RenderNativePDFTo(ctx, request, file)
			}))
			code, _ := daemonconn.ProblemCode(err)
			if err != nil && allocation != "" && code != "redaction_review_changed" && code != "redaction_verifier_version" && code != "not_found" {
				return fmt.Errorf("numbered render for allocation %s failed; retry with the same allocation and review: %w", allocation, err)
			}
			return err
		}}
	cmd.Flags().StringVar(&allocation, "allocation", "", "reserved numbered delivery UUID; retries reuse its retained result")
	cmd.Flags().BoolVar(&overwrite, "overwrite", false, "replace an existing output file")
	cmd.Flags().BoolVar(&asJSON, "json", false, "print JSON receipt and saved archive identity")
	return cmd
}

type redactionDownload func(context.Context, *os.File) (processing.RedactionArchive, error)

func publishRedaction(cmd *cobra.Command, destination string, overwrite, asJSON bool, download redactionDownload) (retErr error) {
	stage, err := filepublish.CreateStage(filepath.Dir(destination), "docbank-redaction-")
	if err != nil {
		return err
	}
	published := false
	defer func() {
		retErr = errors.Join(retErr, stage.Cleanup())
		if retErr != nil && published {
			retErr = fmt.Errorf("verified redaction archive is already saved at %q: %w", destination, retErr)
		}
	}()
	archive, err := download(cmd.Context(), stage.File)
	if err != nil {
		return err
	}
	if err := errors.Join(stage.File.Sync(), stage.File.Close()); err != nil {
		return err
	}
	if err := cmd.Context().Err(); err != nil {
		return err
	}
	published, err = filepublish.Publish(stage.Path(), destination, overwrite)
	if err != nil {
		return err
	}
	if err := stage.Cleanup(); err != nil {
		return err
	}
	if asJSON {
		return writeCLIJSON(cmd.OutOrStdout(), struct {
			processing.RedactionArchive

			Destination string `json:"destination"`
			State       string `json:"state"`
		}{archive, destination, "published"})
	}
	_, err = fmt.Fprintf(cmd.OutOrStdout(), "Verified redaction archive saved to %s\nExtract redacted.pdf to open the PDF. redacted.txt contains its text; receipt.json records the review and file hashes.\n", destination)
	if err != nil {
		return fmt.Errorf("writing redaction receipt: %w", err)
	}
	return nil
}

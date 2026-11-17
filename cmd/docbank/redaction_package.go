package main

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"
	"go.kenn.io/docbank/internal/daemonconn"
	"go.kenn.io/docbank/internal/filepublish"
	"go.kenn.io/docbank/internal/home"
	"go.kenn.io/docbank/internal/processing"
	"go.kenn.io/docbank/internal/recipientpackage"
	"go.kenn.io/docbank/internal/redactionfiles"
)

type recipientPackageResult struct {
	State         string                             `json:"state"`
	Destination   string                             `json:"destination"`
	Size          int64                              `json:"size"`
	SHA256        string                             `json:"sha256"`
	ReceiptSHA256 string                             `json:"receipt_sha256"`
	Verification  string                             `json:"verification"`
	Receipt       processing.RecipientPackageReceipt `json:"receipt"`
}

func newRedactionPackageCommand() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{Use: "package <selection.json> <recipient.zip>", Short: "Package reviewed numbered PDFs for recipient import", Args: cobra.ExactArgs(2),
		Example: "  docbank redaction package selection.json recipient.zip --json",
		RunE: func(cmd *cobra.Command, args []string) error {
			selection, err := recipientpackage.ReadSelection(cmd.Context(), args[0])
			if err != nil {
				return recipientCLIError(err)
			}
			destination, err := prepareGetDestination(args[1], false)
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
				return usageError(errors.New("recipient destination must be outside the Docbank data directory"))
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), 30*time.Minute)
			defer cancel()
			c, err := daemonconn.Ensure(ctx)
			if err != nil {
				return err
			}
			defer func() { _ = c.Close() }()
			checked, err := recipientpackage.Preflight(ctx, c, selection)
			if err != nil {
				return recipientCLIError(err)
			}
			return recipientCLIError(publishRecipientPackage(ctx, cmd, destination, asJSON, c, checked))
		}}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print JSON package receipt and saved archive identity")
	return cmd
}

func publishRecipientPackage(ctx context.Context, cmd *cobra.Command, destination string, asJSON bool, c *daemonconn.Connection, checked recipientpackage.Plan) (retErr error) {
	stage, err := filepublish.CreateStage(filepath.Dir(destination), "docbank-recipient-")
	if err != nil {
		return err
	}
	published := false
	defer func() {
		retErr = errors.Join(retErr, stage.Cleanup())
		if retErr != nil && published {
			retErr = fmt.Errorf("verified recipient package is already saved at %q: %w", destination, retErr)
		}
	}()
	archive, err := recipientpackage.Build(ctx, c, checked, stage.File)
	if err != nil {
		return err
	}
	result := recipientPackageResult{State: "published", Destination: destination, Size: archive.Size,
		SHA256: archive.SHA256, ReceiptSHA256: archive.ReceiptSHA256,
		Verification: "masks_text_and_numbers", Receipt: archive.Receipt}
	if err = errors.Join(stage.File.Sync(), stage.File.Close()); err != nil {
		return err
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	published, err = filepublish.Publish(stage.Path(), destination, false)
	if err != nil {
		return err
	}
	if err = stage.Cleanup(); err != nil {
		return err
	}
	if asJSON {
		return writeCLIJSON(cmd.OutOrStdout(), result)
	}
	_, err = fmt.Fprintf(cmd.OutOrStdout(), "Recipient package saved to %s\n%d documents, %d pages; %d bytes; SHA-256 %s\n"+
		"PDF masks, text and numbers match the supplied reviews. Unmasked pixels and producer identity are not proven.\n"+
		"Extract the ZIP before running package preflight; mapping.json supplies the field mapping.\n",
		destination, result.Receipt.DocumentCount, result.Receipt.PageCount, result.Size, result.SHA256)
	if err != nil {
		return fmt.Errorf("writing recipient receipt: %w", err)
	}
	return nil
}

func recipientCLIError(err error) error {
	if errors.Is(err, redactionfiles.ErrInvalidInput) {
		return usageError(err)
	}
	if errors.Is(err, processing.ErrRecipientPackageInvalid) || errors.Is(err, processing.ErrRecipientPackageLimit) {
		return usageError(err)
	}
	if errors.Is(err, processing.ErrRecipientPackageIntegrity) {
		return integrityError(err)
	}
	return redactionCLIError(err)
}

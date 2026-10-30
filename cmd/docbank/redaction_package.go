package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"
	"go.kenn.io/docbank/document/redaction"
	"go.kenn.io/docbank/internal/canonical"
	"go.kenn.io/docbank/internal/daemonconn"
	"go.kenn.io/docbank/internal/filepublish"
	"go.kenn.io/docbank/internal/home"
	"go.kenn.io/docbank/internal/processing"
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
			selection, err := readRecipientSelection(args[0])
			if err != nil {
				return err
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
			checked, err := preflightRecipientPackage(ctx, c, selection)
			if err != nil {
				return recipientCLIError(err)
			}
			return recipientCLIError(publishRecipientPackage(ctx, cmd, destination, asJSON, c, checked))
		}}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print JSON package receipt and saved archive identity")
	return cmd
}

func publishRecipientPackage(ctx context.Context, cmd *cobra.Command, destination string, asJSON bool, c *daemonconn.Connection, checked recipientPreflight) (retErr error) {
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
	receipt, err := writeRecipientPackage(ctx, c, checked, filepath.Dir(stage.Path()), stage.File)
	if err != nil {
		return err
	}
	info, err := stage.File.Stat()
	if err != nil {
		return err
	}
	if err = processing.VerifyRecipientPackage(ctx, stage.File, info.Size(), receipt); err != nil {
		return err
	}
	hash := sha256.New()
	if _, err = stage.File.Seek(0, io.SeekStart); err != nil {
		return err
	}
	if _, err = io.Copy(hash, &recipientContextReader{ctx, stage.File}); err != nil {
		return err
	}
	encoded, err := canonical.Marshal(receipt)
	if err != nil {
		return err
	}
	receiptHash := sha256.Sum256(encoded)
	result := recipientPackageResult{
		State: "published", Destination: destination, Size: info.Size(),
		SHA256: hex.EncodeToString(hash.Sum(nil)), ReceiptSHA256: hex.EncodeToString(receiptHash[:]),
		Verification: "masks_text_and_numbers", Receipt: receipt,
	}
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
		destination, receipt.DocumentCount, receipt.PageCount, result.Size, result.SHA256)
	if err != nil {
		return fmt.Errorf("writing recipient receipt: %w", err)
	}
	return nil
}

func writeRecipientPackage(ctx context.Context, c *daemonconn.Connection, checked recipientPreflight, directory string, out io.Writer) (processing.RecipientPackageReceipt, error) {
	return processing.WriteRecipientPackage(ctx, out, checked.Documents, func(ctx context.Context, i int) (io.ReadCloser, []byte, error) {
		input := checked.Inputs[i]
		review, err := readRecipientReview(input)
		if err != nil {
			return nil, nil, err
		}
		if review.SHA256 != checked.Documents[i].ReviewSHA256 {
			return nil, nil, redactionCLIError(processing.ErrRedactionReviewChanged)
		}
		text, err := redaction.Text(review.Resolved)
		if err != nil {
			return nil, nil, err
		}
		file, err := os.OpenFile(filepath.Join(directory, "document.pdf"), os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
		if err != nil {
			return nil, nil, err
		}
		staged := &recipientPDFStage{file}
		readCtx, cancel := context.WithTimeout(ctx, 10*time.Minute)
		_, err = c.DownloadBatesExportTo(readCtx, checked.Receipts[i], file, &review)
		cancel()
		if err != nil {
			return nil, nil, errors.Join(err, staged.Close())
		}
		return staged, text, nil
	})
}

type recipientPDFStage struct{ *os.File }

func (s *recipientPDFStage) Close() error { return errors.Join(s.File.Close(), os.Remove(s.Name())) }

type recipientContextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r *recipientContextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(p)
}

package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"
	"go.kenn.io/docbank/internal/daemonconn"
	"go.kenn.io/docbank/internal/processing"
)

func newRedactionVerifyCommand() *cobra.Command {
	var asJSON bool
	command := &cobra.Command{
		Use:   "verify <preparation.json> <masks.json> <review.json> <archive.zip>",
		Short: "Check a saved redaction ZIP against its review",
		Long: "Check the receipt, file hashes and sanitized text offline.\n" +
			"Print the archive hash for comparison with the saved render-time value.\n" +
			"PDF contents and producer identity are not independently verified.",
		Example: "  docbank redaction verify preparation.json masks.json review.json redacted.zip\n" +
			"  docbank redaction verify preparation.json masks.json review.json redacted.zip --json",
		Args: cobra.ExactArgs(4),
		RunE: func(cmd *cobra.Command, args []string) error {
			request, err := readRedactionInputs(args[0], args[1])
			if err != nil {
				return err
			}
			review, err := readRedactionReview(args[2], request)
			if err != nil {
				return err
			}
			result, err := verifyRedactionFile(cmd.Context(), args[3], review)
			if err != nil {
				return fmt.Errorf("checking redaction archive %q: %w", args[3], redactionCLIError(err))
			}
			if asJSON {
				return writeCLIJSON(cmd.OutOrStdout(), result)
			}
			return writeRedactionVerification(cmd.OutOrStdout(), result)
		},
	}
	command.Flags().BoolVar(&asJSON, "json", false, "print JSON receipt and archive identity")
	return command
}

func verifyRedactionFile(ctx context.Context, path string,
	review processing.RedactionReview,
) (result processing.RedactionArchive, retErr error) {
	info, err := os.Stat(path)
	if err != nil {
		return result, err
	}
	if !info.Mode().IsRegular() {
		return result, usageError(errors.New("archive must be a regular file"))
	}
	file, err := os.Open(path)
	if err != nil {
		return result, err
	}
	defer func() { retErr = errors.Join(retErr, file.Close()) }()
	info, err = file.Stat()
	if err != nil {
		return result, err
	}
	if !info.Mode().IsRegular() {
		return result, usageError(errors.New("archive must be a regular file"))
	}
	result.Receipt, err = daemonconn.VerifyRedactionArchive(ctx, file, info.Size(), review)
	if err != nil {
		return result, err
	}
	result.Size = info.Size()
	hash := sha256.New()
	reader := io.NewSectionReader(file, 0, result.Size)
	var buffer [64 << 10]byte
	for remaining := result.Size; remaining > 0; {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		n, err := reader.Read(buffer[:min(int64(len(buffer)), remaining)])
		_, _ = hash.Write(buffer[:n])
		remaining -= int64(n)
		if err != nil {
			if errors.Is(err, io.EOF) {
				return result, fmt.Errorf("%w: archive ended before its stated size", daemonconn.ErrIntegrity)
			}
			return result, err
		}
	}
	result.SHA256 = hex.EncodeToString(hash.Sum(nil))
	return result, ctx.Err()
}

func writeRedactionVerification(output io.Writer, result processing.RedactionArchive) error {
	receipt := result.Receipt
	_, err := fmt.Fprintf(output, "Receipt and sanitized text match the supplied review.\n"+
		"Archive: %d bytes, SHA-256 %s\nReview SHA-256: %s\nPages: %d\n"+
		"PDF: %d bytes, SHA-256 %s\nText: %d bytes, SHA-256 %s\n"+
		"PDF contents were not compared with the review. "+
		"Compare the archive SHA-256 with the render-time value.\n",
		result.Size, result.SHA256, receipt.ReviewSHA256, receipt.PageCount,
		receipt.PDF.Size, receipt.PDF.SHA256, receipt.Text.Size, receipt.Text.SHA256)
	if err != nil {
		return fmt.Errorf("writing redaction verification: %w", err)
	}
	return nil
}

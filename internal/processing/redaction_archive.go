package processing

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"

	"go.kenn.io/docbank/document/redaction"
	"go.kenn.io/docbank/internal/canonical"
)

func writeRedactionArchive(ctx context.Context, destination *os.File, pdf renderedNativePDF,
	admitted admittedRedaction, maxBytes int64,
) (RedactionArchive, error) {
	review := admitted.review
	text, err := redaction.Text(review.Resolved)
	if err != nil {
		return RedactionArchive{}, err
	}
	receipt := RedactionReceipt{Contract: "native-pdf-redaction-result/v1", Selection: review.Selection,
		AdmittedNodeRevision: admitted.revision, PreparationSHA256: review.PreparationSHA256,
		DecisionsSHA256: review.DecisionsSHA256, ReviewSHA256: review.SHA256, ResolvedSHA256: review.Resolved.SHA256,
		Recipe: review.Recipe, PageCount: review.Resolved.PageCount, PDF: pdf.digest,
		Text: RedactionFileDigest{Size: int64(len(text)), SHA256: sha256HexBytes(text)}}
	encoded, err := canonical.Marshal(receipt)
	if err != nil {
		return RedactionArchive{}, err
	}
	if len(encoded) > 256<<10 {
		return RedactionArchive{}, ErrRedactionPreparationLimit
	}
	writer := zip.NewWriter(&redactionBoundedWriter{ctx: ctx, out: destination, remaining: maxBytes})
	for _, entry := range []struct {
		name   string
		reader io.Reader
	}{
		{"redacted.pdf", io.NewSectionReader(pdf.file, 0, pdf.digest.Size)},
		{"redacted.txt", bytes.NewReader(text)},
		{"receipt.json", bytes.NewReader(encoded)},
	} {
		// DOS time avoids the optional extended-timestamp field emitted by Modified.
		header := &zip.FileHeader{Name: entry.name, Method: zip.Store, ModifiedDate: 33} //nolint:staticcheck
		header.SetMode(0600)
		out, err := writer.CreateHeader(header)
		if err != nil {
			return RedactionArchive{}, errors.Join(err, writer.Close())
		}
		hash := sha256.New()
		n, err := io.Copy(io.MultiWriter(out, hash), &contextReader{ctx: ctx, reader: entry.reader})
		if err != nil {
			return RedactionArchive{}, errors.Join(err, writer.Close())
		}
		if entry.name == "redacted.pdf" {
			if n != pdf.digest.Size || hex.EncodeToString(hash.Sum(nil)) != pdf.digest.SHA256 {
				return RedactionArchive{}, errors.Join(ErrRedactionPreparationIntegrity, writer.Close())
			}
		}
	}
	if err := writer.Close(); err != nil {
		return RedactionArchive{}, fmt.Errorf("closing redaction ZIP: %w", err)
	}
	digest, err := redactionFileDigest(ctx, destination)
	if err != nil {
		return RedactionArchive{}, err
	}
	return RedactionArchive{Receipt: receipt, Size: digest.Size, SHA256: digest.SHA256}, nil
}

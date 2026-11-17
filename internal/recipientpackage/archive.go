package recipientpackage

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"time"

	"go.kenn.io/docbank/document/redaction"
	"go.kenn.io/docbank/internal/canonical"
	"go.kenn.io/docbank/internal/daemonconn"
	"go.kenn.io/docbank/internal/processing"
)

type Archive struct {
	Size          int64
	SHA256        string
	ReceiptSHA256 string
	Receipt       processing.RecipientPackageReceipt
}

// Build fills and independently checks the caller's private staged archive.
func Build(ctx context.Context, c *daemonconn.Connection, checked Plan, output *os.File) (Archive, error) {
	receipt, err := writePackage(ctx, c, checked, filepath.Dir(output.Name()), output)
	if err != nil {
		return Archive{}, err
	}
	info, err := output.Stat()
	if err != nil {
		return Archive{}, err
	}
	expected := receipt
	expected.Documents = checked.Documents
	if err := processing.VerifyRecipientPackage(ctx, output, info.Size(), expected); err != nil {
		return Archive{}, err
	}
	hash := sha256.New()
	if _, err := output.Seek(0, io.SeekStart); err != nil {
		return Archive{}, err
	}
	if _, err := io.Copy(hash, &recipientContextReader{ctx, output}); err != nil {
		return Archive{}, err
	}
	encoded, err := canonical.Marshal(receipt)
	if err != nil {
		return Archive{}, err
	}
	receiptHash := sha256.Sum256(encoded)
	return Archive{Size: info.Size(), SHA256: hex.EncodeToString(hash.Sum(nil)), ReceiptSHA256: hex.EncodeToString(receiptHash[:]), Receipt: receipt}, nil
}

func writePackage(ctx context.Context, c *daemonconn.Connection, checked Plan, directory string, out io.Writer) (processing.RecipientPackageReceipt, error) {
	return processing.WriteRecipientPackage(ctx, out, checked.Documents, func(ctx context.Context, i int) (io.ReadCloser, []byte, error) {
		input := checked.Inputs[i]
		review, err := readRecipientReview(ctx, input)
		if err != nil {
			return nil, nil, err
		}
		if review.SHA256 != checked.Documents[i].ReviewSHA256 {
			return nil, nil, processing.ErrRedactionReviewChanged
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
		verified, err := c.DownloadBatesExportTo(readCtx, checked.Receipts[i], file, &review)
		if err == nil && verified.Verification != "masks_text_and_numbers" {
			err = processing.ErrRecipientPackageIntegrity
		}
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

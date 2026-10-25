package daemonconn

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"os"

	"go.kenn.io/docbank/document/redaction"
	"go.kenn.io/docbank/internal/processing"
)

// VerifyRedactionArchive checks a saved ZIP against an already validated review.
// The caller owns the reader and must keep its bytes stable during verification.
func VerifyRedactionArchive(ctx context.Context, reader io.ReaderAt, size int64,
	review processing.RedactionReview,
) (processing.RedactionReceipt, error) {
	var receipt processing.RedactionReceipt
	if size < 1 || size > processing.MaxRedactionArchiveBytes {
		return receipt, integrityErrorf("redaction archive size exceeds its bounds")
	}
	archive, err := zip.NewReader(reader, size)
	if err != nil {
		return receipt, redactionArchiveError(err)
	}
	if len(archive.File) != 3 || archive.Comment != "" {
		return receipt, integrityErrorf("redaction archive inventory differs")
	}
	names := []string{"redacted.pdf", "redacted.txt", "receipt.json"}
	limits := []uint64{512 << 20, (16 << 20) + 16, 256 << 10}
	digests := make([]processing.RedactionFileDigest, 2)
	for i, entry := range archive.File {
		if entry.Name != names[i] || !entry.Mode().IsRegular() || entry.Method != zip.Store || entry.Flags&1 != 0 || len(entry.Extra) != 0 || entry.Comment != "" || entry.UncompressedSize64 > limits[i] || entry.CompressedSize64 != entry.UncompressedSize64 {
			return receipt, integrityErrorf("redaction archive entry %d is invalid", i)
		}
		reader, err := entry.Open()
		if err != nil {
			return receipt, redactionArchiveError(err)
		}
		// Limits are fixed constants no larger than 512 MiB.
		source := redactionReader{ctx, io.LimitReader(reader, int64(limits[i])+1)} //nolint:gosec
		if i == 2 {
			data, readErr := io.ReadAll(source)
			err = errors.Join(readErr, reader.Close())
			if err == nil {
				err = json.Unmarshal(data, &receipt, json.RejectUnknownMembers(true))
			}
		} else {
			hash := sha256.New()
			n, readErr := io.Copy(hash, source)
			err = errors.Join(readErr, reader.Close())
			digests[i] = processing.RedactionFileDigest{Size: n, SHA256: hex.EncodeToString(hash.Sum(nil))}
		}
		if err != nil {
			return receipt, redactionArchiveError(err)
		}
	}
	expectedText, err := redaction.Text(review.Resolved)
	if err != nil {
		return receipt, err
	}
	textDigest := processing.RedactionFileDigest{Size: int64(len(expectedText)), SHA256: fmt.Sprintf("%x", sha256.Sum256(expectedText))}
	if receipt.Contract != "native-pdf-redaction-result/v1" || receipt.Selection != review.Selection || receipt.AdmittedNodeRevision < 1 || receipt.PreparationSHA256 != review.PreparationSHA256 || receipt.DecisionsSHA256 != review.DecisionsSHA256 || receipt.ReviewSHA256 != review.SHA256 || receipt.ResolvedSHA256 != review.Resolved.SHA256 || receipt.Recipe != review.Recipe || receipt.PageCount != review.Resolved.PageCount || receipt.PDF != digests[0] || receipt.PDF.Size == 0 || receipt.Text != digests[1] || receipt.Text != textDigest {
		return receipt, integrityErrorf("redaction receipt or text disagrees with reviewed plan")
	}
	return receipt, nil
}

func redactionArchiveError(err error) error {
	var local *os.PathError
	if errors.As(err, &local) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return fmt.Errorf("%w: redaction archive: %w", ErrIntegrity, err)
}

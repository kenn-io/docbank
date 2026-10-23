package daemonconn

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"os"

	"github.com/doordash-oss/oapi-codegen-dd/v3/pkg/runtime"
	"go.kenn.io/docbank/internal/apiclient"
	"go.kenn.io/docbank/internal/canonical"
	"go.kenn.io/docbank/internal/processing"
)

// RenderNativePDFTo verifies the archive in a caller-owned private file before publication.
func (c *Connection) RenderNativePDFTo(ctx context.Context, request processing.RedactionRenderRequest, output *os.File) (processing.RedactionArchive, error) {
	var result processing.RedactionArchive
	review, err := processing.BuildRedactionReview(request.Preparation, request.Masks, request.Preparation.Selection.Revision)
	if err != nil {
		return result, err
	}
	if review.SHA256 != request.ReviewSHA256 {
		return result, processing.ErrRedactionReviewChanged
	}
	if output == nil {
		return result, errors.New("redaction output file is required")
	}
	if err := output.Truncate(0); err != nil {
		return result, err
	}
	if _, err := output.Seek(0, io.SeekStart); err != nil {
		return result, err
	}
	var response *http.Response
	_, err = c.apiWithResponseLimit(&response, processing.MaxRedactionResponseBytes).RenderNativePDF(runtime.WithStreamingResponse(ctx), &apiclient.RenderNativePDFRequestOptions{Body: &request})
	if err != nil {
		return result, err
	}
	defer func() { _ = response.Body.Close() }()
	size, digest := response.ContentLength, response.Header.Get("X-Content-Sha256")
	if size < 1 || size > processing.MaxRedactionArchiveBytes || !canonical.IsSHA256Hex(digest) || response.Header.Get("X-Redaction-Review-Sha256") != review.SHA256 || response.Header.Get("Content-Type") != "application/zip" {
		return result, integrityErrorf("redaction archive headers disagree with review")
	}
	hash := sha256.New()
	written, err := io.Copy(io.MultiWriter(output, hash), redactionReader{ctx, io.LimitReader(response.Body, size+1)})
	if err != nil {
		return result, err
	}
	if written != size || hex.EncodeToString(hash.Sum(nil)) != digest {
		return result, integrityErrorf("redaction archive size or digest mismatch")
	}
	receipt, err := verifyRedactionArchive(ctx, output, size, review)
	if err != nil {
		return result, err
	}
	return processing.RedactionArchive{Receipt: receipt, Size: size, SHA256: digest}, nil
}

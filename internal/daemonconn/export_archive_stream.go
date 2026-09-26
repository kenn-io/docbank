package daemonconn

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"uuid"

	"github.com/doordash-oss/oapi-codegen-dd/v3/pkg/runtime"
	"go.kenn.io/docbank/document/bundle"
	"go.kenn.io/docbank/internal/apiclient"
	"go.kenn.io/docbank/internal/canonical"
)

// ExportArchiveStream carries the daemon's bounded retained archive receipt.
// CopyVerified checks the transport bytes before a caller publishes them.
type ExportArchiveStream struct {
	io.ReadCloser

	Size            int64
	SHA256          string
	PlanFingerprint string
}

func (s *ExportArchiveStream) CopyVerified(output io.Writer) (int64, error) {
	if s == nil || s.ReadCloser == nil || output == nil || s.Size < 1 ||
		s.Size > bundle.MaxArchiveBytes || !canonical.IsSHA256Hex(s.SHA256) {
		return 0, errors.New("invalid export archive authority")
	}
	hash := sha256.New()
	written, err := io.Copy(io.MultiWriter(output, hash), io.LimitReader(s, s.Size+1))
	if err != nil {
		return written, err
	}
	if written != s.Size || hex.EncodeToString(hash.Sum(nil)) != s.SHA256 {
		return written, errors.New("export archive differs from advertised size or digest")
	}
	return written, nil
}

func (c *Connection) OpenExportArchive(ctx context.Context, jobID string) (*ExportArchiveStream, error) {
	if !IsCanonicalUUIDv4(jobID) {
		return nil, errors.New("invalid export job ID")
	}
	parsedID, _ := uuid.Parse(jobID)
	var response *http.Response
	_, err := c.apiWithResponse(&response).ReadExportArchive(runtime.WithStreamingResponse(ctx),
		&apiclient.ReadExportArchiveRequestOptions{PathParams: &apiclient.ReadExportArchivePath{ID: parsedID}})
	if err != nil {
		return nil, err
	}
	if response == nil || response.Body == nil {
		return nil, errors.New("export archive has no response body")
	}
	size, sizeErr := strconv.ParseInt(response.Header.Get("Content-Length"), 10, 64)
	digest := response.Header.Get("Docbank-Archive-Sha256")
	fingerprint := response.Header.Get("Docbank-Plan-Fingerprint")
	if sizeErr != nil || size < 1 || size > bundle.MaxArchiveBytes ||
		!canonical.IsSHA256Hex(digest) || !canonical.IsSHA256Hex(fingerprint) ||
		!strings.HasPrefix(response.Header.Get("Content-Type"), "application/zip") {
		_ = response.Body.Close()
		return nil, errors.New("export archive lacks bounded size, digest or plan fingerprint")
	}
	return &ExportArchiveStream{ReadCloser: response.Body, Size: size,
		SHA256: digest, PlanFingerprint: fingerprint}, nil
}

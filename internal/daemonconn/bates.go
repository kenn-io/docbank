package daemonconn

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"

	"github.com/doordash-oss/oapi-codegen-dd/v3/pkg/runtime"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/apiclient"
	"go.kenn.io/docbank/internal/pdfstamp"
	"uuid"
)

func (c *Connection) BatesExport(ctx context.Context, allocationID string) (api.BatesExport, error) {
	if !validUUIDv4(allocationID) {
		return api.BatesExport{}, errors.New("Bates export requires an allocation UUID") //nolint:staticcheck // Bates is a proper name.
	}
	result, err := c.API().ReadBatesExport(ctx, &apiclient.ReadBatesExportRequestOptions{
		PathParams: &apiclient.ReadBatesExportPath{ID: uuid.MustParse(allocationID)}})
	if err != nil {
		return api.BatesExport{}, err
	}
	if result.AllocationID != allocationID || result.ArtifactID != allocationID || result.State != "verified" ||
		result.MediaType != "application/pdf" || !validSHA256Hex(result.BlobSHA256) ||
		!validSHA256Hex(result.RecipeSHA256) || !validSHA256Hex(result.ManifestSHA256) ||
		result.Size < 1 || result.PageCount < 1 || result.PageCount != len(result.Pages) {
		return api.BatesExport{}, integrityErrorf("Bates export receipt is inconsistent")
	}
	for index, page := range result.Pages {
		if page.Ordinal != index+1 || page.OutputPage != index+1 || page.OccurrenceID == "" ||
			!validSHA256Hex(page.SourceBlobSHA256) || page.SourcePage < 1 || page.Label == "" {
			return api.BatesExport{}, integrityErrorf("Bates export page receipt is inconsistent")
		}
	}
	return *result, nil
}

func (c *Connection) DownloadBatesExport(ctx context.Context, receipt api.BatesExport) ([]byte, error) {
	current, err := c.BatesExport(ctx, receipt.AllocationID)
	if err != nil {
		return nil, err
	}
	if current.BlobSHA256 != receipt.BlobSHA256 || current.ManifestSHA256 != receipt.ManifestSHA256 ||
		current.Size != receipt.Size || current.PageCount != receipt.PageCount {
		return nil, integrityErrorf("Bates export receipt changed")
	}
	var response *http.Response
	_, err = c.apiWithResponse(&response).DownloadBatesExportContent(runtime.WithStreamingResponse(ctx),
		&apiclient.DownloadBatesExportContentRequestOptions{PathParams: &apiclient.DownloadBatesExportContentPath{ID: uuid.MustParse(receipt.AllocationID)}})
	if err != nil {
		return nil, err
	}
	defer func() { _ = response.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(response.Body, receipt.Size+1))
	if err != nil {
		return nil, err
	}
	digest := sha256.Sum256(data)
	labels := make([]pdfstamp.PageLabel, len(receipt.Pages))
	for index, page := range receipt.Pages {
		labels[index] = pdfstamp.PageLabel{SourcePage: page.SourcePage, Label: page.Label}
	}
	verified, verifyErr := pdfstamp.VerifyStamped(ctx, data, labels)
	if response.StatusCode != http.StatusOK || int64(len(data)) != receipt.Size ||
		hex.EncodeToString(digest[:]) != receipt.BlobSHA256 || verifyErr != nil || verified.PageCount != receipt.PageCount {
		return nil, integrityErrorf("Bates export bytes disagree with verified receipt")
	}
	return data, nil
}

// DownloadBatesExportTo streams verified bytes into a seekable staging file,
// then independently checks the digest, page count, and visible labels.
func (c *Connection) DownloadBatesExportTo(
	ctx context.Context, receipt api.BatesExport, destination io.ReadWriteSeeker,
) (pdfstamp.Result, error) {
	current, err := c.BatesExport(ctx, receipt.AllocationID)
	if err != nil {
		return pdfstamp.Result{}, err
	}
	if current.BlobSHA256 != receipt.BlobSHA256 || current.ManifestSHA256 != receipt.ManifestSHA256 ||
		current.Size != receipt.Size || current.PageCount != receipt.PageCount {
		return pdfstamp.Result{}, integrityErrorf("Bates export receipt changed")
	}
	if _, err := destination.Seek(0, io.SeekStart); err != nil {
		return pdfstamp.Result{}, err
	}
	var response *http.Response
	_, err = c.apiWithResponse(&response).DownloadBatesExportContent(runtime.WithStreamingResponse(ctx),
		&apiclient.DownloadBatesExportContentRequestOptions{PathParams: &apiclient.DownloadBatesExportContentPath{ID: uuid.MustParse(receipt.AllocationID)}})
	if err != nil {
		return pdfstamp.Result{}, err
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return pdfstamp.Result{}, decodeError(response)
	}
	digest := sha256.New()
	written, err := io.Copy(io.MultiWriter(destination, digest), io.LimitReader(response.Body, receipt.Size+1))
	if err != nil {
		return pdfstamp.Result{}, err
	}
	if written != receipt.Size || hex.EncodeToString(digest.Sum(nil)) != receipt.BlobSHA256 {
		return pdfstamp.Result{}, integrityErrorf("Bates export bytes disagree with verified receipt")
	}
	labels := make([]pdfstamp.PageLabel, len(receipt.Pages))
	for index, page := range receipt.Pages {
		labels[index] = pdfstamp.PageLabel{SourcePage: page.SourcePage, Label: page.Label}
	}
	verified, err := pdfstamp.VerifyStampedReader(ctx, destination, labels)
	if err != nil || verified.SHA256 != receipt.BlobSHA256 || verified.Size != receipt.Size ||
		verified.PageCount != receipt.PageCount {
		return pdfstamp.Result{}, integrityErrorf("Bates export bytes disagree with verified receipt")
	}
	_, err = destination.Seek(0, io.SeekStart)
	return verified, err
}

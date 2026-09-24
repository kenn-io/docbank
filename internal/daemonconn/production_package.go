package daemonconn

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"

	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/apiclient"
	"uuid"
)

// PublishProductionPackage retains one verified recipient package from a
// successful production job. Exact retries return the same archive identity.
func (c *Connection) PublishProductionPackage(ctx context.Context, jobID string,
	request api.ProductionPackagePublishRequest) (api.ProductionPackagePublished, error) {
	if !validUUIDv4(jobID) || !validUUIDv4(request.OperationID) ||
		request.MaxVolumeBytes < 1 || request.MaxVolumeBytes > 50<<30 ||
		request.MaxVolumeDocuments < 1 || request.MaxVolumeDocuments > 100_000 {
		return api.ProductionPackagePublished{}, errors.New("invalid production package request")
	}
	switch request.ProfileID {
	case "export-dat-pdf-v1", "export-dat-opt-images-v1", "export-dat-lfp-images-v1":
	default:
		return api.ProductionPackagePublished{}, errors.New("invalid production package profile")
	}
	result, err := c.API().PublishProductionPackage(ctx, &apiclient.PublishProductionPackageRequestOptions{
		PathParams: &apiclient.PublishProductionPackagePath{JobID: uuid.MustParse(jobID)},
		Body:       &request,
	})
	if err != nil {
		return api.ProductionPackagePublished{}, err
	}
	if result == nil || result.JobID != jobID || result.OperationID != request.OperationID ||
		result.ProfileID != request.ProfileID || !validUUIDv4(result.VersionID) ||
		!validSHA256Hex(result.ArchiveSHA256) || !validSHA256Hex(result.EvidenceSHA256) ||
		result.Size < 1 || result.Size == math.MaxInt64 {
		return api.ProductionPackagePublished{}, integrityErrorf("production package publication is inconsistent")
	}
	return *result, nil
}

// DownloadProductionPackageTo requests a one-use verified ticket and streams
// its complete archive to destination while checking the retained hash/size.
// Callers should discard destination if an error is returned.
func (c *Connection) DownloadProductionPackageTo(ctx context.Context, jobID, operationID string,
	destination io.Writer) (api.ProductionPackageDownloadTicket, error) {
	if !validUUIDv4(jobID) || !validUUIDv4(operationID) || destination == nil {
		return api.ProductionPackageDownloadTicket{}, errors.New("production package download requires job, operation and destination")
	}
	ticket, err := c.API().DownloadProductionPackage(ctx, &apiclient.DownloadProductionPackageRequestOptions{
		PathParams: &apiclient.DownloadProductionPackagePath{
			JobID: uuid.MustParse(jobID), OperationID: uuid.MustParse(operationID),
		},
		Body: &apiclient.DownloadProductionPackageBody{},
	})
	if err != nil {
		return api.ProductionPackageDownloadTicket{}, err
	}
	parsed, err := url.Parse(ticket.URL)
	if err != nil || parsed.IsAbs() || parsed.Host != "" || parsed.User != nil ||
		parsed.Path != "/api/daemon/web-download/file" || parsed.RawPath != "" ||
		parsed.Fragment != "" || len(parsed.Query()) != 1 || len(parsed.Query()["ticket"]) != 1 ||
		parsed.Query().Get("ticket") == "" || !validUUIDv4(ticket.VersionID) ||
		!validSHA256Hex(ticket.ArchiveSHA256) || ticket.Size < 1 || ticket.Size == math.MaxInt64 {
		return api.ProductionPackageDownloadTicket{}, integrityErrorf("production package ticket is inconsistent")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+ticket.URL, nil)
	if err != nil {
		return api.ProductionPackageDownloadTicket{}, fmt.Errorf("building production package download: %w", err)
	}
	request.Header.Set("X-Api-Key", c.key)
	response, err := c.hc.Do(request)
	if err != nil {
		return api.ProductionPackageDownloadTicket{}, fmt.Errorf("downloading production package: %w", err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return api.ProductionPackageDownloadTicket{}, integrityErrorf(
			"production package download failed with HTTP %d", response.StatusCode)
	}
	digest := sha256.New()
	written, err := io.CopyBuffer(io.MultiWriter(destination, digest),
		io.LimitReader(response.Body, ticket.Size+1), make([]byte, 256<<10))
	if err != nil {
		return api.ProductionPackageDownloadTicket{}, fmt.Errorf("copying production package download: %w", err)
	}
	if written != ticket.Size || hex.EncodeToString(digest.Sum(nil)) != ticket.ArchiveSHA256 {
		return api.ProductionPackageDownloadTicket{}, integrityErrorf("production package bytes disagree with retained version")
	}
	return *ticket, nil
}

package daemonconn

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"io"
	"net/http"

	"github.com/doordash-oss/oapi-codegen-dd/v3/pkg/runtime"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/apiclient"
	productionservice "go.kenn.io/docbank/internal/production"
	"uuid"
)

const maxProductionPrivilegeExportBytes = 512 << 20

// ExportProductionPrivilegeLog downloads public bytes only after matching the
// response to a frozen receipt and verifying the delivered content digest.
func (c *Connection) ExportProductionPrivilegeLog(ctx context.Context, logID string, revision int64,
	format string) (productionservice.PrivilegeLogExport, error) {
	var empty productionservice.PrivilegeLogExport
	mediaType, ok := productionPrivilegeExportMediaType(format)
	if !validUUIDv4(logID) || revision < 1 || !ok {
		return empty, errors.New("invalid production privilege export request")
	}
	page, err := c.ProductionPrivilegeLog(ctx, logID, revision, "", 1)
	if err != nil {
		return empty, err
	}
	var response *http.Response
	_, err = c.apiWithResponse(&response).ExportProductionPrivilegeLog(runtime.WithStreamingResponse(ctx),
		&apiclient.ExportProductionPrivilegeLogRequestOptions{PathParams: &apiclient.ExportProductionPrivilegeLogPath{
			Log: uuid.MustParse(logID), Revision: revision, Format: apiclient.ExportProductionPrivilegeLogPathFormat(format),
		}})
	if err != nil {
		return empty, err
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return empty, decodeError(response)
	}
	if response.Header.Get("Content-Type") != mediaType ||
		response.Header.Get(api.ProductionPrivilegeReceiptHashHeader) != page.Receipt.SHA256 ||
		response.Header.Get(api.ProductionPrivilegeRowsHashHeader) != page.Receipt.RowsSHA256 ||
		response.ContentLength > maxProductionPrivilegeExportBytes {
		return empty, integrityErrorf("production privilege export headers disagree with frozen receipt")
	}
	content, err := io.ReadAll(io.LimitReader(response.Body, maxProductionPrivilegeExportBytes+1))
	if err != nil {
		return empty, err
	}
	if len(content) > maxProductionPrivilegeExportBytes {
		return empty, integrityErrorf("production privilege export exceeds client size limit")
	}
	digest := sha256.Sum256(content)
	if response.Header.Get("Content-Digest") != "sha-256=:"+base64.StdEncoding.EncodeToString(digest[:])+":" ||
		(response.ContentLength >= 0 && int64(len(content)) != response.ContentLength) {
		return empty, integrityErrorf("production privilege export content digest is inconsistent")
	}
	return productionservice.PrivilegeLogExport{
		MediaType: mediaType, ReceiptSHA256: page.Receipt.SHA256, RowsSHA256: page.Receipt.RowsSHA256,
		ContentSHA256: hex.EncodeToString(digest[:]), Content: content,
	}, nil
}

func productionPrivilegeExportMediaType(format string) (string, bool) {
	switch format {
	case "json":
		return productionservice.PrivilegeLogJSONMediaType, true
	case "csv":
		return productionservice.PrivilegeLogCSVMediaType, true
	case "xlsx":
		return productionservice.PrivilegeLogXLSXMediaType, true
	case "pdf":
		return productionservice.PrivilegeLogPDFMediaType, true
	default:
		return "", false
	}
}

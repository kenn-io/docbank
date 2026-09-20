package daemonconn

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"github.com/doordash-oss/oapi-codegen-dd/v3/pkg/runtime"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/emailpdf"
	"go.kenn.io/docbank/internal/apiclient"
	"io"
	"net/http"
	"uuid"
)

func (c *Connection) RequestEmailPDF(ctx context.Context, input document.EmailPDFRequest) (document.EmailPDFJob, error) {
	var out document.EmailPDFJob
	if !validUUIDv4(input.VersionID) || (input.GenerationID != "" && !validSHA256Hex(input.GenerationID)) {
		return out, errors.New("email PDF requires an exact version and decoded generation")
	}
	if input.Paper == "" {
		input.Paper = "A4"
	}
	if input.Paper != "A4" && input.Paper != "Letter" {
		return out, errors.New("email PDF paper must be A4 or Letter")
	}
	response, err := c.API().RenderEmailPDF(ctx, &apiclient.RenderEmailPDFRequestOptions{Body: &input})
	if err != nil {
		return out, err
	}
	out = *response
	if out.VersionID != input.VersionID || !validSHA256Hex(out.ProfileFingerprint) || !validSHA256Hex(out.JobID) {
		return document.EmailPDFJob{}, integrityErrorf("email PDF job identity disagrees with request")
	}
	return out, nil
}

func (c *Connection) EmailPDFJobState(ctx context.Context, jobID string) (string, error) {
	if !validSHA256Hex(jobID) {
		return "", errors.New("email PDF requires a job fingerprint")
	}
	out, err := c.API().GetEmailPDFJob(ctx, &apiclient.GetEmailPDFJobRequestOptions{PathParams: &apiclient.GetEmailPDFJobPath{JobID: jobID}})
	if err != nil {
		return "", err
	}
	switch out.State {
	case "queued", "running", "retry_wait", "operator_required", "failed", "completed":
	default:
		return "", integrityErrorf("email PDF job has an invalid state")
	}
	return out.State, nil
}

func (c *Connection) EmailPDFReceipt(ctx context.Context, versionID, profile string) (document.EmailPDFReceiptV1, error) {
	var out document.EmailPDFReceiptV1
	if !validUUIDv4(versionID) || !validSHA256Hex(profile) {
		return out, errors.New("email PDF requires exact version and profile")
	}
	response, err := c.API().GetEmailPDF(ctx, &apiclient.GetEmailPDFRequestOptions{PathParams: &apiclient.GetEmailPDFPath{VersionID: uuid.MustParse(versionID), Profile: profile}})
	if err != nil {
		return out, err
	}
	out = *response
	if out.Source.VersionID != versionID || out.ProfileFingerprint != profile || !validSHA256Hex(out.AttachmentID) || !validSHA256Hex(out.BuildID) || document.ValidateEmailPDFBinding(out.Binding) != nil || !validSHA256Hex(out.Output.PDFSHA256) || out.Output.PDFSize < 1 || out.Output.PDFSize > emailpdf.MaxPDFBytes || out.Output.Pages < 1 || out.Output.Pages > emailpdf.MaxPDFPages {
		return document.EmailPDFReceiptV1{}, integrityErrorf("email PDF retained receipt is inconsistent")
	}
	metadata, err := c.EmailMetadataGeneration(ctx, versionID, out.Binding.GenerationID)
	if err != nil {
		return document.EmailPDFReceiptV1{}, err
	}
	if metadata.Checksum != out.Binding.GenerationChecksum || metadata.Version.NodeID != out.Source.NodeID || metadata.Version.BlobHash != out.Source.SHA256 || metadata.Version.Size != out.Source.Size {
		return document.EmailPDFReceiptV1{}, integrityErrorf("email PDF source and decoded generation disagree")
	}
	return out, nil
}

// DownloadEmailPDF verifies the exact typed receipt and complete bytes; it
// never represents the derivative as an original content version.
func (c *Connection) DownloadEmailPDF(ctx context.Context, receipt document.EmailPDFReceiptV1) ([]byte, error) {
	verified, err := c.EmailPDFReceipt(ctx, receipt.Source.VersionID, receipt.ProfileFingerprint)
	if err != nil {
		return nil, err
	}
	if verified != receipt {
		return nil, integrityErrorf("email PDF receipt changed")
	}
	var resp *http.Response
	_, err = c.apiWithResponse(&resp).DownloadEmailPDF(runtime.WithStreamingResponse(ctx), &apiclient.DownloadEmailPDFRequestOptions{PathParams: &apiclient.DownloadEmailPDFPath{VersionID: uuid.MustParse(receipt.Source.VersionID), Profile: receipt.ProfileFingerprint}})
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, decodeError(resp)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, receipt.Output.PDFSize+1))
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(b)
	pages, parseErr := emailpdf.VerifyPDF(b)
	if int64(len(b)) != receipt.Output.PDFSize || hex.EncodeToString(sum[:]) != receipt.Output.PDFSHA256 || parseErr != nil || pages != receipt.Output.Pages {
		return nil, integrityErrorf("email PDF bytes disagree with verified receipt")
	}
	return b, nil
}

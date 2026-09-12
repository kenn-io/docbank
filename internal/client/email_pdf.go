package client

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/emailpdf"
	"io"
	"net/http"
	"net/url"
)

func (c *Client) RequestEmailPDF(ctx context.Context, input document.EmailPDFRequest) (document.EmailPDFJob, error) {
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
	if err := c.do(ctx, http.MethodPost, "/api/v1/email-pdfs", nil, input, &out); err != nil {
		return out, err
	}
	if out.VersionID != input.VersionID || !validSHA256Hex(out.ProfileFingerprint) || !validSHA256Hex(out.JobID) {
		return document.EmailPDFJob{}, integrityErrorf("email PDF job identity disagrees with request")
	}
	return out, nil
}

func (c *Client) EmailPDFJobState(ctx context.Context, jobID string) (string, error) {
	if !validSHA256Hex(jobID) {
		return "", errors.New("email PDF requires a job fingerprint")
	}
	var out struct {
		State string `json:"state"`
	}
	if err := c.do(ctx, http.MethodGet, "/api/v1/email-pdf-jobs/"+url.PathEscape(jobID), nil, nil, &out); err != nil {
		return "", err
	}
	switch out.State {
	case "queued", "running", "retry_wait", "operator_required", "failed", "completed":
	default:
		return "", integrityErrorf("email PDF job has an invalid state")
	}
	return out.State, nil
}

func emailPDFPath(versionID, profile string) string {
	return "/api/v1/email-pdfs/" + url.PathEscape(versionID) + "/" + url.PathEscape(profile)
}

func (c *Client) EmailPDFReceipt(ctx context.Context, versionID, profile string) (document.EmailPDFReceiptV1, error) {
	var out document.EmailPDFReceiptV1
	if !validUUIDv4(versionID) || !validSHA256Hex(profile) {
		return out, errors.New("email PDF requires exact version and profile")
	}
	if err := c.do(ctx, http.MethodGet, emailPDFPath(versionID, profile), nil, nil, &out); err != nil {
		return out, err
	}
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
func (c *Client) DownloadEmailPDF(ctx context.Context, receipt document.EmailPDFReceiptV1) ([]byte, error) {
	verified, err := c.EmailPDFReceipt(ctx, receipt.Source.VersionID, receipt.ProfileFingerprint)
	if err != nil {
		return nil, err
	}
	if verified != receipt {
		return nil, integrityErrorf("email PDF receipt changed")
	}
	req, err := c.emailRequest(ctx, http.MethodGet, emailPDFPath(receipt.Source.VersionID, receipt.ProfileFingerprint)+"/content", nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.hc.Do(req)
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

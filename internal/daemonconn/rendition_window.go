package daemonconn

import (
	"context"
	"encoding/json/v2"
	"errors"
	"github.com/doordash-oss/oapi-codegen-dd/v3/pkg/runtime"
	"go.kenn.io/docbank/internal/apiclient"
	"io"
	"unicode/utf8"

	"go.kenn.io/docbank/internal/api"
)

const (
	maxRenditionWindowResponseBytes = 1 << 20
	maxRenditionOffset              = 1<<31 - 1
)

func (c *Connection) RenditionTextWindow(
	ctx context.Context, request api.RenditionWindowRequest,
) (api.RenditionTextWindow, error) {
	if !validUUIDv4(request.VaultID) || request.NodeID < 1 ||
		!validUUIDv4(request.ContentVersionID) || !validSHA256Hex(request.AttachmentID) ||
		request.Offset < 0 || request.Offset > maxRenditionOffset ||
		request.MaxChars < 1 || request.MaxChars > 16_000 {
		return api.RenditionTextWindow{}, errors.New("rendition window request is invalid")
	}
	response, err := c.API().ReadDocumentRenditionWindowWithResponse(runtime.WithStreamingResponse(ctx), &apiclient.ReadDocumentRenditionWindowRequestOptions{Body: &request})
	if err != nil {
		return api.RenditionTextWindow{}, err
	}
	defer func() { _ = response.HTTPResponse.Body.Close() }()
	encoded, err := io.ReadAll(io.LimitReader(response.HTTPResponse.Body, maxRenditionWindowResponseBytes+1))
	if err != nil {
		return api.RenditionTextWindow{}, errors.New("reading rendition window response")
	}
	if len(encoded) > maxRenditionWindowResponseBytes {
		return api.RenditionTextWindow{}, errors.New("rendition window response is too large")
	}
	var result api.RenditionTextWindow
	if err := json.Unmarshal(encoded, &result, json.RejectUnknownMembers(true)); err != nil {
		return api.RenditionTextWindow{}, errors.New("rendition window response is invalid")
	}
	if err := validateRenditionTextWindow(request, result); err != nil {
		return api.RenditionTextWindow{}, err
	}

	return result, nil
}

func validateRenditionTextWindow(request api.RenditionWindowRequest, result api.RenditionTextWindow) error {
	runeCount := utf8.RuneCountInString(result.Text)
	if request.Offset < 0 || request.Offset > maxRenditionOffset ||
		result.RequestedOffset < 0 || result.RequestedOffset > maxRenditionOffset ||
		result.ActualStart < 0 || result.ActualStart > maxRenditionOffset ||
		result.ActualEnd < 0 || result.ActualEnd > maxRenditionOffset ||
		result.NextOffset < 0 || result.NextOffset > maxRenditionOffset ||
		result.ResponseBytes < 0 || result.ResponseBytes > maxRenditionWindowResponseBytes ||
		runeCount > request.MaxChars || runeCount > maxRenditionOffset-request.Offset {
		return errors.New("rendition window response does not bind its requested authority")
	}
	expectedEnd := request.Offset + runeCount
	if result.VaultID != request.VaultID || result.NodeID != request.NodeID ||
		result.ContentVersionID != request.ContentVersionID || result.AttachmentID != request.AttachmentID ||
		!validSHA256Hex(result.BuildID) || !validSHA256Hex(result.ProfileFingerprint) ||
		!validSHA256Hex(result.Checksum) || result.MediaType != "text/markdown" || !utf8.ValidString(result.Text) ||
		result.RequestedOffset != request.Offset || result.ActualStart != request.Offset ||
		result.ActualEnd != expectedEnd ||
		result.NextOffset != result.ActualEnd || result.ResponseBytes != len(result.Text) ||
		(!result.EOF && runeCount != request.MaxChars) {
		return errors.New("rendition window response does not bind its requested authority")
	}
	return nil
}

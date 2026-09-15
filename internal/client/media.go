package client

import (
	"context"
	"encoding/json/v2"
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"strconv"

	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/canonical"
)

type MediaSourceOptions struct {
	Cursor, Profile, Variant string
	Limit                    int
}

type MediaOccurrenceOptions struct {
	Cursor, SourceID string
	Limit            int
}

func (c *Client) SubmitSuppliedMedia(
	ctx context.Context, metadata api.MediaSuppliedMetadata, content io.Reader,
) (api.MediaReceipt, error) {
	var result api.MediaReceipt
	err := c.doMediaMultipart(ctx, "/api/v1/media/sources", metadata,
		metadata.Filename, metadata.MediaType, content, &result)
	return result, validateMediaReceipt(result, metadata.OperationID, err)
}

func (c *Client) SubmitRemoteRecording(
	ctx context.Context, request api.MediaReferenceBody,
) (api.MediaReceipt, error) {
	var result api.MediaReceipt
	err := c.do(ctx, http.MethodPost, "/api/v1/media/sources", nil, request, &result)
	return result, validateMediaReceipt(result, request.OperationID, err)
}

func (c *Client) MediaSources(ctx context.Context, cursor string, limit int) (api.MediaSourcePage, error) {
	return c.MediaSourcesWithOptions(ctx, MediaSourceOptions{Cursor: cursor, Limit: limit})
}

func (c *Client) MediaSourcesWithOptions(
	ctx context.Context, options MediaSourceOptions,
) (api.MediaSourcePage, error) {
	if options.Limit < 1 || options.Limit > 250 {
		return api.MediaSourcePage{}, errors.New("media source limit must be between 1 and 250")
	}
	query := url.Values{"limit": {strconv.Itoa(options.Limit)}}
	if options.Cursor != "" {
		query.Set("cursor", options.Cursor)
	}
	if options.Profile != "" {
		query.Set("profile", options.Profile)
	}
	if options.Variant != "" {
		query.Set("variant", options.Variant)
	}
	var result api.MediaSourcePage
	if err := c.do(ctx, http.MethodGet, "/api/v1/media/sources?"+query.Encode(), nil, nil, &result); err != nil {
		return api.MediaSourcePage{}, err
	}
	if result.Items == nil || result.Total < len(result.Items) || len(result.Items) > options.Limit {
		return api.MediaSourcePage{}, errors.New("daemon returned an invalid media source page")
	}
	seen := make(map[string]struct{}, len(result.Items))
	for _, item := range result.Items {
		if !canonical.IsSHA256Hex(item.SourceID) {
			return api.MediaSourcePage{}, errors.New("daemon returned an invalid media source identity")
		}
		if _, exists := seen[item.SourceID]; exists {
			return api.MediaSourcePage{}, errors.New("daemon returned duplicate media source rows")
		}
		seen[item.SourceID] = struct{}{}
	}
	return result, nil
}

func (c *Client) MediaStatus(ctx context.Context, sourceID string) (api.MediaReceipt, error) {
	var result api.MediaReceipt
	err := c.do(ctx, http.MethodGet, "/api/v1/media/sources/"+url.PathEscape(sourceID), nil, nil, &result)
	if err == nil && result.SourceID != sourceID {
		err = errors.New("daemon returned media status for a different source")
	}
	return result, validateMediaReceipt(result, "", err)
}

func (c *Client) RetryMedia(
	ctx context.Context, sourceID string, request api.MediaRetryBody,
) (api.MediaReceipt, error) {
	var result api.MediaReceipt
	err := c.do(ctx, http.MethodPost, "/api/v1/media/sources/"+url.PathEscape(sourceID)+"/retry",
		nil, request, &result)
	if err == nil && result.SourceID != sourceID {
		err = errors.New("daemon returned retry receipt for a different source")
	}
	return result, validateMediaReceipt(result, request.OperationID, err)
}

func (c *Client) ImportMediaArtifact(
	ctx context.Context, sourceID string, metadata api.MediaArtifactMetadata, content io.Reader,
) (api.MediaReceipt, error) {
	var result api.MediaReceipt
	err := c.doMediaMultipart(ctx, "/api/v1/media/sources/"+url.PathEscape(sourceID)+"/artifacts",
		metadata, metadata.Filename, metadata.MediaType, content, &result)
	if err == nil && result.SourceID != sourceID {
		err = errors.New("daemon returned artifact receipt for a different source")
	}
	return result, validateMediaReceipt(result, metadata.OperationID, err)
}

func (c *Client) MediaOccurrences(
	ctx context.Context, options MediaOccurrenceOptions,
) (api.MediaOccurrencePage, error) {
	if options.Limit < 1 || options.Limit > 250 {
		return api.MediaOccurrencePage{}, errors.New("media occurrence limit must be between 1 and 250")
	}
	query := url.Values{"limit": {strconv.Itoa(options.Limit)}}
	if options.Cursor != "" {
		query.Set("cursor", options.Cursor)
	}
	if options.SourceID != "" {
		query.Set("source_id", options.SourceID)
	}
	var result api.MediaOccurrencePage
	if err := c.do(ctx, http.MethodGet, "/api/v1/media/occurrences?"+query.Encode(), nil, nil, &result); err != nil {
		return api.MediaOccurrencePage{}, err
	}
	if result.Items == nil || result.Total < len(result.Items) || len(result.Items) > options.Limit {
		return api.MediaOccurrencePage{}, errors.New("daemon returned an invalid media occurrence page")
	}
	return result, nil
}

func (c *Client) DeclareMediaOccurrence(
	ctx context.Context, request api.MediaOccurrenceMutationBody,
) (api.MediaReceipt, error) {
	var result api.MediaReceipt
	err := c.do(ctx, http.MethodPost, "/api/v1/media/occurrences", nil, request, &result)
	return result, validateMediaReceipt(result, request.OperationID, err)
}

func (c *Client) RevokeMediaOccurrence(
	ctx context.Context, occurrenceID string, request api.MediaOccurrenceRevokeBody,
) (api.MediaReceipt, error) {
	var result api.MediaReceipt
	err := c.do(ctx, http.MethodDelete, "/api/v1/media/occurrences/"+url.PathEscape(occurrenceID), nil, request, &result)
	if err == nil && result.OccurrenceID != occurrenceID {
		err = errors.New("daemon returned revocation receipt for a different occurrence")
	}
	return result, validateMediaReceipt(result, request.OperationID, err)
}

func (c *Client) MediaOrigins(ctx context.Context) (api.MediaOriginPage, error) {
	var result api.MediaOriginPage
	if err := c.do(ctx, http.MethodGet, "/api/v1/media/origins", nil, nil, &result); err != nil {
		return api.MediaOriginPage{}, err
	}
	if result.Items == nil {
		return api.MediaOriginPage{}, errors.New("daemon returned an invalid media origin page")
	}
	return result, nil
}

func (c *Client) PlanMediaAcquisition(
	ctx context.Context, request api.MediaReferenceBody,
) (api.MediaAcquisitionPlan, error) {
	var result api.MediaAcquisitionPlan
	err := c.do(ctx, http.MethodPost, "/api/v1/media/acquisition-plan", nil, request, &result)
	if err == nil && (result.PlanToken == "" || !canonical.IsSHA256Hex(result.PlanFingerprint) || result.OriginID == "") {
		err = errors.New("daemon returned an invalid media acquisition plan")
	}
	return result, err
}

func (c *Client) GrantMediaAcquisition(
	ctx context.Context, request api.MediaAcquisitionGrantBody,
) (api.MediaConsentReceipt, error) {
	var result api.MediaConsentReceipt
	err := c.do(ctx, http.MethodPost, "/api/v1/media/consent/grants", nil, request, &result)
	return result, validateMediaConsentReceipt(result, request.OperationID, err)
}

func (c *Client) RevokeMediaAcquisition(
	ctx context.Context, request api.MediaAcquisitionRevokeBody,
) (api.MediaConsentReceipt, error) {
	var result api.MediaConsentReceipt
	err := c.do(ctx, http.MethodPost, "/api/v1/media/consent/revocations", nil, request, &result)
	if err == nil && result.OriginID != request.OriginID {
		err = errors.New("daemon returned consent receipt for a different media origin")
	}
	return result, validateMediaConsentReceipt(result, request.OperationID, err)
}

func (c *Client) doMediaMultipart(
	ctx context.Context, endpoint string, metadata any, filename, mediaType string,
	content io.Reader, out any,
) error {
	if content == nil || filename == "" {
		return errors.New("media upload requires a named content stream")
	}
	reader, writer := io.Pipe()
	multipartWriter := multipart.NewWriter(writer)
	writeResult := make(chan error, 1)
	go func() {
		defer close(writeResult)
		header := make(textproto.MIMEHeader)
		header.Set("Content-Disposition", `form-data; name="metadata"`)
		header.Set("Content-Type", "application/json")
		part, err := multipartWriter.CreatePart(header)
		if err == nil {
			err = json.MarshalWrite(part, metadata)
		}
		if err == nil {
			header = make(textproto.MIMEHeader)
			header.Set("Content-Disposition", multipart.FileContentDisposition("file", filename))
			header.Set("Content-Type", mediaType)
			part, err = multipartWriter.CreatePart(header)
		}
		if err == nil {
			_, err = io.Copy(part, content)
		}
		if closeErr := multipartWriter.Close(); err == nil {
			err = closeErr
		}
		_ = writer.CloseWithError(err)
		writeResult <- err
	}()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+endpoint, reader)
	if err != nil {
		_ = reader.CloseWithError(err)
		return err
	}
	req.Header.Set("Content-Type", multipartWriter.FormDataContentType())
	if c.key != "" {
		req.Header.Set("X-Api-Key", c.key)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		_ = reader.CloseWithError(err)
		<-writeResult
		return &transportError{err: err}
	}
	_ = reader.Close()
	defer func() { _ = resp.Body.Close() }()
	writeErr := <-writeResult
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return decodeError(resp)
	}
	if writeErr != nil {
		return writeErr
	}
	if err := json.UnmarshalRead(resp.Body, out); err != nil {
		return &responseDecodeError{err: err}
	}
	return nil
}

func validateMediaReceipt(receipt api.MediaReceipt, operationID string, err error) error {
	if err != nil {
		return err
	}
	if receipt.SourceID == "" || receipt.VaultUID == "" ||
		(operationID != "" && receipt.OperationID != operationID) {
		return errors.New("daemon returned an invalid media receipt identity")
	}
	switch receipt.OperationState {
	case "queued", "running", "succeeded", "failed", "cancelled":
	default:
		return errors.New("daemon returned an invalid media operation state")
	}
	return nil
}

func validateMediaConsentReceipt(receipt api.MediaConsentReceipt, operationID string, err error) error {
	if err != nil {
		return err
	}
	if receipt.OperationID != operationID || receipt.OriginID == "" {
		return errors.New("daemon returned an invalid media consent receipt")
	}
	return nil
}

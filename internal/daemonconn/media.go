package daemonconn

import (
	"context"
	"encoding/json/v2"
	"errors"
	"github.com/doordash-oss/oapi-codegen-dd/v3/pkg/runtime"
	"go.kenn.io/docbank/internal/apiclient"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"

	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/canonical"
)

type MediaOccurrenceOptions struct {
	Cursor, SourceID string
	Limit            int
}

func (c *Connection) SubmitSuppliedMedia(
	ctx context.Context, metadata api.MediaSuppliedMetadata, content io.Reader,
) (api.MediaReceipt, error) {
	var result api.MediaReceipt
	err := readMediaMultipartReceipt(metadata, metadata.Filename, metadata.MediaType, content, &result,
		func(edit runtime.RequestEditorFn) (*http.Response, error) {
			var responseHTTP *http.Response
			_, err := c.apiWithResponse(&responseHTTP).SubmitMediaSource(runtime.WithStreamingResponse(ctx), &apiclient.SubmitMediaSourceRequestOptions{}, edit)
			if err != nil {
				return nil, err
			}
			return responseHTTP, nil
		})
	return result, validateMediaReceipt(result, metadata.OperationID, err)
}

func (c *Connection) SubmitRemoteRecording(
	ctx context.Context, request api.MediaReferenceBody,
) (api.MediaReceipt, error) {
	var result api.MediaReceipt
	apiResponse, err := c.API().SubmitMediaSource(ctx, &apiclient.SubmitMediaSourceRequestOptions{Body: &request})
	if err == nil {
		result = *apiResponse
	}
	return result, validateMediaReceipt(result, request.OperationID, err)
}

func (c *Connection) MediaSources(ctx context.Context, cursor string, limit int) (api.MediaSourcePage, error) {
	if limit < 1 || limit > 250 {
		return api.MediaSourcePage{}, errors.New("media source limit must be between 1 and 250")
	}
	params := apiclient.ListMediaSourcesQuery{}
	params.Limit = new(int64(limit))
	if cursor != "" {
		params.Cursor = new(cursor)
	}
	var result api.MediaSourcePage
	apiResponse, err := c.API().ListMediaSources(ctx, &apiclient.ListMediaSourcesRequestOptions{Query: &params})
	if err != nil {
		return api.MediaSourcePage{}, err
	}
	result = *apiResponse
	if result.Items == nil || result.Total < len(result.Items) || len(result.Items) > limit {
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

func (c *Connection) MediaStatus(ctx context.Context, sourceID string) (api.MediaReceipt, error) {
	var result api.MediaReceipt
	apiResponse, err := c.API().GetMediaSource(ctx, &apiclient.GetMediaSourceRequestOptions{PathParams: &apiclient.GetMediaSourcePath{SourceID: sourceID}})
	if err == nil {
		result = *apiResponse
	}
	if err == nil && result.SourceID != sourceID {
		err = errors.New("daemon returned media status for a different source")
	}
	return result, validateMediaReceipt(result, "", err)
}

func (c *Connection) RetryMedia(
	ctx context.Context, sourceID string, request api.MediaRetryBody,
) (api.MediaReceipt, error) {
	var result api.MediaReceipt
	apiResponse, err := c.API().RetryMediaSource(ctx, &apiclient.RetryMediaSourceRequestOptions{PathParams: &apiclient.RetryMediaSourcePath{SourceID: sourceID}, Body: &request})
	if err == nil {
		result = *apiResponse
	}
	if err == nil && result.SourceID != sourceID {
		err = errors.New("daemon returned retry receipt for a different source")
	}
	return result, validateMediaReceipt(result, request.OperationID, err)
}

func (c *Connection) ImportMediaArtifact(
	ctx context.Context, sourceID string, metadata api.MediaArtifactMetadata, content io.Reader,
) (api.MediaReceipt, error) {
	var result api.MediaReceipt
	err := readMediaMultipartReceipt(metadata, metadata.Filename, metadata.MediaType, content, &result,
		func(edit runtime.RequestEditorFn) (*http.Response, error) {
			var responseHTTP *http.Response
			_, err := c.apiWithResponse(&responseHTTP).ImportMediaArtifact(runtime.WithStreamingResponse(ctx), &apiclient.ImportMediaArtifactRequestOptions{PathParams: &apiclient.ImportMediaArtifactPath{SourceID: sourceID}}, edit)
			if err != nil {
				return nil, err
			}
			return responseHTTP, nil
		})
	if err == nil && result.SourceID != sourceID {
		err = errors.New("daemon returned artifact receipt for a different source")
	}
	return result, validateMediaReceipt(result, metadata.OperationID, err)
}

func (c *Connection) MediaOccurrences(
	ctx context.Context, options MediaOccurrenceOptions,
) (api.MediaOccurrencePage, error) {
	if options.Limit < 1 || options.Limit > 250 {
		return api.MediaOccurrencePage{}, errors.New("media occurrence limit must be between 1 and 250")
	}
	params := apiclient.ListMediaOccurrencesQuery{}
	params.Limit = new(int64(options.Limit))
	if options.Cursor != "" {
		params.Cursor = new(options.Cursor)
	}
	if options.SourceID != "" {
		params.SourceID = new(options.SourceID)
	}
	var result api.MediaOccurrencePage
	apiResponse, err := c.API().ListMediaOccurrences(ctx, &apiclient.ListMediaOccurrencesRequestOptions{Query: &params})
	if err != nil {
		return api.MediaOccurrencePage{}, err
	}
	result = *apiResponse
	if result.Items == nil || result.Total < len(result.Items) || len(result.Items) > options.Limit {
		return api.MediaOccurrencePage{}, errors.New("daemon returned an invalid media occurrence page")
	}
	return result, nil
}

func (c *Connection) DeclareMediaOccurrence(
	ctx context.Context, request api.MediaOccurrenceMutationBody,
) (api.MediaReceipt, error) {
	var result api.MediaReceipt
	apiResponse, err := c.API().DeclareMediaOccurrence(ctx, &apiclient.DeclareMediaOccurrenceRequestOptions{Body: &request})
	if err == nil {
		result = *apiResponse
	}
	return result, validateMediaReceipt(result, request.OperationID, err)
}

func (c *Connection) RevokeMediaOccurrence(
	ctx context.Context, occurrenceID string, request api.MediaOccurrenceRevokeBody,
) (api.MediaReceipt, error) {
	var result api.MediaReceipt
	apiResponse, err := c.API().RevokeMediaOccurrence(ctx, &apiclient.RevokeMediaOccurrenceRequestOptions{PathParams: &apiclient.RevokeMediaOccurrencePath{OccurrenceID: occurrenceID}, Body: &request})
	if err == nil {
		result = *apiResponse
	}
	if err == nil && result.OccurrenceID != occurrenceID {
		err = errors.New("daemon returned revocation receipt for a different occurrence")
	}
	return result, validateMediaReceipt(result, request.OperationID, err)
}

func (c *Connection) MediaOrigins(ctx context.Context) (api.MediaOriginPage, error) {
	var result api.MediaOriginPage
	apiResponse, err := c.API().ListMediaOrigins(ctx)
	if err != nil {
		return api.MediaOriginPage{}, err
	}
	result = *apiResponse
	if result.Items == nil {
		return api.MediaOriginPage{}, errors.New("daemon returned an invalid media origin page")
	}
	return result, nil
}

func (c *Connection) PlanMediaAcquisition(
	ctx context.Context, request api.MediaReferenceBody,
) (api.MediaAcquisitionPlan, error) {
	var result api.MediaAcquisitionPlan
	apiResponse, err := c.API().PlanMediaAcquisition(ctx, &apiclient.PlanMediaAcquisitionRequestOptions{Body: &request})
	if err == nil {
		result = *apiResponse
	}
	if err == nil && (result.PlanToken == "" || !canonical.IsSHA256Hex(result.PlanFingerprint) || result.OriginID == "") {
		err = errors.New("daemon returned an invalid media acquisition plan")
	}
	return result, err
}

func (c *Connection) GrantMediaAcquisition(
	ctx context.Context, request api.MediaAcquisitionGrantBody,
) (api.MediaConsentReceipt, error) {
	var result api.MediaConsentReceipt
	apiResponse, err := c.API().GrantMediaAcquisitionConsent(ctx, &apiclient.GrantMediaAcquisitionConsentRequestOptions{Body: &request})
	if err == nil {
		result = *apiResponse
	}
	return result, validateMediaConsentReceipt(result, request.OperationID, err)
}

func (c *Connection) RevokeMediaAcquisition(
	ctx context.Context, request api.MediaAcquisitionRevokeBody,
) (api.MediaConsentReceipt, error) {
	var result api.MediaConsentReceipt
	apiResponse, err := c.API().RevokeMediaAcquisitionConsent(ctx, &apiclient.RevokeMediaAcquisitionConsentRequestOptions{Body: &request})
	if err == nil {
		result = *apiResponse
	}
	if err == nil && result.OriginID != request.OriginID {
		err = errors.New("daemon returned consent receipt for a different media origin")
	}
	return result, validateMediaConsentReceipt(result, request.OperationID, err)
}

func readMediaMultipartReceipt(
	metadata any, filename, mediaType string,
	content io.Reader, out any, send func(runtime.RequestEditorFn) (*http.Response, error),
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
	resp, err := send(func(_ context.Context, req *http.Request) error {
		req.Body = reader
		req.Header.Set("Content-Type", multipartWriter.FormDataContentType())
		return nil
	})
	if err != nil {
		_ = reader.CloseWithError(err)
		<-writeResult
		return err
	}
	_ = reader.Close()
	defer func() { _ = resp.Body.Close() }()
	writeErr := <-writeResult
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return decodeError(resp)
	}
	if writeErr != nil && !errors.Is(writeErr, io.ErrClosedPipe) {
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

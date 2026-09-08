package geminiembed

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	filesUploadPath = "/upload/v1beta/files"
	filesPathPrefix = "/v1beta/files/"
)

type wireStartUpload struct {
	File struct{} `json:"file"`
}

type wireCreateFileResponse struct {
	File wireFile `json:"file"`
}

type wireFile struct {
	Name           string             `json:"name"`
	DisplayName    string             `json:"displayName"`
	MIMEType       string             `json:"mimeType"`
	SizeBytes      string             `json:"sizeBytes"`
	CreateTime     string             `json:"createTime"`
	UpdateTime     string             `json:"updateTime"`
	ExpirationTime string             `json:"expirationTime"`
	SHA256Hash     string             `json:"sha256Hash"`
	URI            string             `json:"uri"`
	DownloadURI    string             `json:"downloadUri"`
	State          string             `json:"state"`
	Source         string             `json:"source"`
	Error          *wireFileError     `json:"error,omitempty"`
	VideoMetadata  *wireVideoMetadata `json:"videoMetadata,omitempty"`
}

type wireFileError struct {
	Code    int64            `json:"code"`
	Message string           `json:"message"`
	Status  string           `json:"status,omitempty"`
	Details []map[string]any `json:"details,omitempty"`
}

type wireVideoMetadata struct {
	VideoDuration string `json:"videoDuration"`
}

func (client *Client) startFileUpload(ctx context.Context, file verifiedFile, secret string, receipt *Receipt) (*url.URL, error) {
	payload, err := json.Marshal(wireStartUpload{})
	if err != nil || int64(len(payload)) > client.profile.MaxRequestBytes {
		return nil, errors.New("gemini embed: file upload start request encoding failed")
	}
	defer clear(payload)
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, origin+filesUploadPath, bytes.NewReader(payload))
	if err != nil {
		return nil, errors.New("gemini embed: file upload start request construction failed")
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Goog-Api-Key", secret)
	request.Header.Set("X-Goog-Upload-Protocol", "resumable")
	request.Header.Set("X-Goog-Upload-Command", "start")
	request.Header.Set("X-Goog-Upload-Header-Content-Length", strconv.FormatInt(file.metadata.ByteLength, 10))
	request.Header.Set("X-Goog-Upload-Header-Content-Type", file.metadata.MediaType)
	if !beginReceiptRequest(receipt) {
		return nil, &ProviderError{Kind: ErrPermanentResponse}
	}
	response, err := client.http.Do(request)
	if err != nil {
		return nil, requestFailure(ctx, "file upload start", err)
	}
	defer func() { _ = response.Body.Close() }()
	if contextErr := ctx.Err(); contextErr != nil {
		return nil, fmt.Errorf("gemini embed: file upload start canceled: %w", contextErr)
	}
	if response.StatusCode != http.StatusOK {
		return nil, statusError(response.StatusCode, response.Header.Get("Retry-After"), time.Now().UTC())
	}
	body, err := readBounded(ctx, response.Body, client.profile.MaxResponseBytes)
	if err != nil {
		return nil, err
	}
	defer clear(body)
	if len(body) != 0 {
		if !isJSONContentType(response.Header.Get("Content-Type")) {
			return nil, &ProviderError{Kind: ErrPermanentResponse}
		}
		var empty struct{}
		if err := json.Unmarshal(body, &empty, json.RejectUnknownMembers(true)); err != nil {
			return nil, &ProviderError{Kind: ErrPermanentResponse}
		}
	}
	uploadURL, err := validateUploadURL(response.Header.Get("X-Goog-Upload-Url"))
	if err != nil || !recordProviderResponseID(receipt, response.Header.Get("X-Goog-Request-Id")) {
		return nil, &ProviderError{Kind: ErrPermanentResponse}
	}
	return uploadURL, nil
}

func (client *Client) finalizeFileUpload(ctx context.Context, uploadURL *url.URL, file verifiedFile, secret string, receipt *Receipt) (validatedProviderFile, string, bool, error) {
	if int64(len(file.data)) > client.profile.MaxRequestBytes {
		return validatedProviderFile{}, "", false, errors.New("gemini embed: raw file upload exceeds request byte capacity")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, uploadURL.String(), bytes.NewReader(file.data))
	if err != nil {
		return validatedProviderFile{}, "", false, errors.New("gemini embed: file upload request construction failed")
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("X-Goog-Api-Key", secret)
	request.Header.Set("X-Goog-Upload-Offset", "0")
	request.Header.Set("X-Goog-Upload-Command", "upload, finalize")
	if !beginReceiptRequest(receipt) {
		return validatedProviderFile{}, "", false, &ProviderError{Kind: ErrPermanentResponse}
	}
	startedAt := time.Now().UTC()
	response, err := client.http.Do(request)
	completedAt := time.Now().UTC()
	if err != nil {
		return validatedProviderFile{}, "", true, requestFailure(ctx, "file upload", err)
	}
	defer func() { _ = response.Body.Close() }()
	if contextErr := ctx.Err(); contextErr != nil {
		return validatedProviderFile{}, "", true, fmt.Errorf("gemini embed: file upload canceled: %w", contextErr)
	}
	if response.StatusCode != http.StatusOK {
		return validatedProviderFile{}, "", true, statusError(response.StatusCode, response.Header.Get("Retry-After"), time.Now().UTC())
	}
	if response.Header.Get("X-Goog-Upload-Status") != "final" || !isJSONContentType(response.Header.Get("Content-Type")) {
		return validatedProviderFile{}, "", true, &ProviderError{Kind: ErrPermanentResponse}
	}
	body, err := readBounded(ctx, response.Body, client.profile.MaxResponseBytes)
	if err != nil {
		return validatedProviderFile{}, "", true, err
	}
	defer clear(body)
	var decoded wireCreateFileResponse
	if err := json.Unmarshal(body, &decoded, json.RejectUnknownMembers(true)); err != nil {
		return validatedProviderFile{}, "", true, &ProviderError{Kind: ErrPermanentResponse}
	}
	validated, ok := validateCreatedWireFile(decoded.File, file, startedAt, completedAt)
	if !ok {
		return validatedProviderFile{}, "", true, &ProviderError{Kind: ErrPermanentResponse}
	}
	return validated, response.Header.Get("X-Goog-Request-Id"), true, nil
}

func (client *Client) waitForActiveFile(ctx context.Context, current validatedProviderFile, file verifiedFile, secret string, receipt *Receipt) (validatedProviderFile, error) {
	if current.file.State == "ACTIVE" {
		return current, nil
	}
	for attempt := range client.profile.MaxPollAttempts {
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, current.file.URI, nil)
		if err != nil {
			return validatedProviderFile{}, errors.New("gemini embed: file poll request construction failed")
		}
		request.Header.Set("Accept", "application/json")
		request.Header.Set("X-Goog-Api-Key", secret)
		if !beginReceiptRequest(receipt) {
			return validatedProviderFile{}, &ProviderError{Kind: ErrPermanentResponse}
		}
		startedAt := time.Now().UTC()
		response, err := client.http.Do(request)
		completedAt := time.Now().UTC()
		if err != nil {
			return validatedProviderFile{}, requestFailure(ctx, "file poll", err)
		}
		if contextErr := ctx.Err(); contextErr != nil {
			_ = response.Body.Close()
			return validatedProviderFile{}, fmt.Errorf("gemini embed: file poll canceled: %w", contextErr)
		}
		if response.StatusCode != http.StatusOK {
			_ = response.Body.Close()
			return validatedProviderFile{}, statusError(response.StatusCode, response.Header.Get("Retry-After"), time.Now().UTC())
		}
		if !isJSONContentType(response.Header.Get("Content-Type")) {
			_ = response.Body.Close()
			return validatedProviderFile{}, &ProviderError{Kind: ErrPermanentResponse}
		}
		body, readErr := readBounded(ctx, response.Body, client.profile.MaxResponseBytes)
		_ = response.Body.Close()
		if readErr != nil {
			return validatedProviderFile{}, readErr
		}
		var decoded wireFile
		decodeErr := json.Unmarshal(body, &decoded, json.RejectUnknownMembers(true))
		clear(body)
		next, valid := validatePolledWireFile(decoded, file, current, startedAt, completedAt)
		if decodeErr != nil || !valid {
			return validatedProviderFile{}, &ProviderError{Kind: ErrPermanentResponse}
		}
		if !recordProviderResponseID(receipt, response.Header.Get("X-Goog-Request-Id")) {
			return validatedProviderFile{}, &ProviderError{Kind: ErrPermanentResponse}
		}
		switch decoded.State {
		case "ACTIVE":
			return next, nil
		case "PROCESSING":
			current = next
			if attempt+1 == client.profile.MaxPollAttempts {
				return validatedProviderFile{}, &ProviderError{Kind: ErrPermanentResponse}
			}
		case "FAILED":
			return validatedProviderFile{}, &ProviderError{Kind: ErrPermanentResponse}
		default:
			return validatedProviderFile{}, &ProviderError{Kind: ErrPermanentResponse}
		}
		timer := time.NewTimer(client.profile.PollInterval)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return validatedProviderFile{}, ctx.Err()
		case <-timer.C:
		}
	}
	return validatedProviderFile{}, &ProviderError{Kind: ErrPermanentResponse}
}

func (client *Client) deleteFile(ctx context.Context, name, secret string, receipt *Receipt) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodDelete, origin+filesPathPrefix+strings.TrimPrefix(name, "files/"), nil)
	if err != nil {
		return errors.New("gemini embed: file deletion request construction failed")
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("X-Goog-Api-Key", secret)
	if !beginReceiptRequest(receipt) {
		return &ProviderError{Kind: ErrPermanentResponse}
	}
	response, err := client.http.Do(request)
	if err != nil {
		return requestFailure(ctx, "file deletion", err)
	}
	defer func() { _ = response.Body.Close() }()
	if contextErr := ctx.Err(); contextErr != nil {
		return fmt.Errorf("gemini embed: file deletion canceled: %w", contextErr)
	}
	if response.StatusCode != http.StatusOK && response.StatusCode != http.StatusNoContent {
		return statusError(response.StatusCode, response.Header.Get("Retry-After"), time.Now().UTC())
	}
	body, err := readBounded(ctx, response.Body, client.profile.MaxResponseBytes)
	if err != nil {
		return err
	}
	defer clear(body)
	if len(body) != 0 {
		if !isJSONContentType(response.Header.Get("Content-Type")) {
			return &ProviderError{Kind: ErrPermanentResponse}
		}
		var empty struct{}
		if err := json.Unmarshal(body, &empty, json.RejectUnknownMembers(true)); err != nil {
			return &ProviderError{Kind: ErrPermanentResponse}
		}
	}
	if !recordProviderResponseID(receipt, response.Header.Get("X-Goog-Request-Id")) {
		return &ProviderError{Kind: ErrPermanentResponse}
	}
	return nil
}

type sanitizedTransportError struct {
	operation  string
	cause      error
	contextErr error
}

func (failure *sanitizedTransportError) Error() string {
	if failure.contextErr != nil {
		return fmt.Sprintf("gemini embed: %s canceled", failure.operation)
	}
	return fmt.Sprintf("gemini embed: %s failed", failure.operation)
}

func (failure *sanitizedTransportError) Unwrap() []error {
	if failure.contextErr != nil {
		return []error{failure.contextErr, failure.cause}
	}
	return []error{failure.cause}
}

func requestFailure(ctx context.Context, operation string, cause error) error {
	return &sanitizedTransportError{operation: operation, cause: cause, contextErr: ctx.Err()}
}

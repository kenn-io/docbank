// Package docling implements the fixed uploaded-file Docling Serve rendition flow.
package docling

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/providerhttp"
)

const (
	providerID = "docling.serve-v1"

	convertPath = "/v1/convert/file/async"
	pollPath    = "/v1/status/poll/"
	resultPath  = "/v1/result/"

	defaultRequestTimeout    = 30 * time.Second
	defaultTotalTimeout      = 10 * time.Minute
	defaultPollInterval      = time.Second
	defaultMaxPollAttempts   = 300
	defaultMaxResponseBytes  = int64(512 << 20)
	defaultMaxDocumentBytes  = int64(64 << 20)
	maxTimeout               = 24 * time.Hour
	maxPollAttempts          = 10_000
	maxResponseBytes         = int64(512 << 20)
	maxDocumentBytes         = int64(1 << 30)
	maxSecretBytes           = 64 << 10
	maxTaskIDBytes           = 120
	maxConsecutiveEmptyReads = 100
	timestampForm            = "2006-01-02T15:04:05.000000000Z"
)

var _ document.RenditionProvider = (*Client)(nil)

// SecretResolver resolves the one profile-bound Docling API-key binding.
type SecretResolver interface {
	ResolveSecret(ctx context.Context, name string) (string, error)
}

// Profile fixes one Docling origin, descriptor, credential binding, and every
// network/result bound used by a client instance.
type Profile struct {
	Origin           string
	Descriptor       document.RenditionDescriptor
	SecretBinding    string
	RequestTimeout   time.Duration
	TotalTimeout     time.Duration
	PollInterval     time.Duration
	MaxPollAttempts  int
	MaxResponseBytes int64
	MaxDocumentBytes int64
}

// Client renders exact authorized uploads through fixed Docling Serve routes.
type Client struct {
	origin           string
	descriptor       document.RenditionDescriptor
	secretBinding    string
	secrets          SecretResolver
	http             *http.Client
	requestTimeout   time.Duration
	totalTimeout     time.Duration
	pollInterval     time.Duration
	maxPollAttempts  int
	maxResponseBytes int64
	maxDocumentBytes int64
}

type requestUsage struct {
	requests    int64
	retries     int64
	outputBytes int64
}

// New validates a fixed profile and isolates the supplied HTTP client from
// ambient cookies and redirect behavior.
func New(profile Profile, secrets SecretResolver, httpClient *http.Client) (*Client, error) {
	origin, err := validateOrigin(profile.Origin, profile.Descriptor.TrustBoundary)
	if err != nil {
		return nil, err
	}
	descriptor, err := document.NewRenditionDescriptor(profile.Descriptor)
	if err != nil || !reflect.DeepEqual(descriptor, profile.Descriptor) {
		if err == nil {
			err = errors.New("descriptor is not canonical")
		}
		return nil, fmt.Errorf("docling: invalid descriptor: %w", err)
	}
	if descriptor.ID != providerID {
		return nil, errors.New("docling: descriptor ID must be docling.serve-v1")
	}
	if profile.SecretBinding == "" {
		if secrets != nil {
			return nil, errors.New("docling: secret resolver requires a named binding")
		}
	} else if secrets == nil {
		return nil, errors.New("docling: named secret binding requires a resolver")
	} else if err := validateToken(profile.SecretBinding, "secret binding"); err != nil {
		return nil, err
	}
	if httpClient == nil {
		return nil, errors.New("docling: HTTP client is required")
	}
	if profile.RequestTimeout == 0 {
		profile.RequestTimeout = defaultRequestTimeout
	}
	if profile.TotalTimeout == 0 {
		profile.TotalTimeout = defaultTotalTimeout
	}
	if profile.PollInterval == 0 {
		profile.PollInterval = defaultPollInterval
	}
	if profile.MaxPollAttempts == 0 {
		profile.MaxPollAttempts = defaultMaxPollAttempts
	}
	if profile.MaxResponseBytes == 0 {
		profile.MaxResponseBytes = defaultMaxResponseBytes
	}
	if profile.MaxDocumentBytes == 0 {
		profile.MaxDocumentBytes = defaultMaxDocumentBytes
	}
	if profile.RequestTimeout <= 0 || profile.RequestTimeout > maxTimeout ||
		profile.TotalTimeout <= 0 || profile.TotalTimeout > maxTimeout ||
		profile.PollInterval <= 0 || profile.PollInterval > profile.TotalTimeout ||
		profile.MaxPollAttempts < 1 || profile.MaxPollAttempts > maxPollAttempts ||
		profile.MaxResponseBytes < 1 || profile.MaxResponseBytes > maxResponseBytes ||
		profile.MaxDocumentBytes < 1 || profile.MaxDocumentBytes > maxDocumentBytes {
		return nil, errors.New("docling: execution bounds are invalid")
	}
	isolate := *httpClient
	isolate.Jar = nil
	isolate.CheckRedirect = providerhttp.RefuseRedirects
	return &Client{
		origin: origin, descriptor: cloneDescriptor(descriptor), secretBinding: profile.SecretBinding,
		secrets: secrets, http: &isolate, requestTimeout: profile.RequestTimeout,
		totalTimeout: profile.TotalTimeout, pollInterval: profile.PollInterval,
		maxPollAttempts: profile.MaxPollAttempts, maxResponseBytes: profile.MaxResponseBytes,
		maxDocumentBytes: profile.MaxDocumentBytes,
	}, nil
}

// Descriptor returns an immutable copy of the configured identity.
func (client *Client) Descriptor() document.RenditionDescriptor {
	if client == nil {
		return document.RenditionDescriptor{}
	}
	return cloneDescriptor(client.descriptor)
}

// Render verifies the sealed bytes before they cross the provider boundary,
// then submits, polls, and fetches only fixed same-origin Docling routes.
func (client *Client) Render(
	ctx context.Context, upload document.AuthorizedUpload, authorization document.RenditionAuthorization,
) (document.RenditionResult, error) {
	if client == nil {
		return document.RenditionResult{}, errors.New("docling: client is required")
	}
	metadata := upload.Metadata()
	if metadata.ByteLength > client.maxDocumentBytes {
		return document.RenditionResult{}, classifiedError(document.RenditionErrorPolicyRejected,
			"input exceeds the Docling byte limit", nil)
	}
	expiresAt, err := time.Parse(timestampForm, authorization.ExpiresAt)
	if err != nil {
		return document.RenditionResult{}, classifiedError(document.RenditionErrorPolicyRejected,
			"Docling authorization expiry is invalid", nil)
	}
	totalCtx, cancel := client.operationContext(ctx, expiresAt)
	defer cancel()
	if err := checkOperation(totalCtx, expiresAt); err != nil {
		return document.RenditionResult{}, err
	}
	source, err := readExact(totalCtx, upload, metadata)
	if err != nil {
		if !time.Now().Before(expiresAt) {
			return document.RenditionResult{}, classifiedError(document.RenditionErrorPolicyRejected, "Docling authorization expired", nil)
		}
		return document.RenditionResult{}, err
	}
	started := time.Now().UTC()

	if err := checkOperation(totalCtx, expiresAt); err != nil {
		return document.RenditionResult{}, err
	}
	usage := &requestUsage{}
	includeMarkdown := client.descriptor.ReturnsMarkdown && authorization.MaxProviderMarkdownBytes > 0
	task, err := client.submit(totalCtx, expiresAt, usage, metadata, source, includeMarkdown)
	if err != nil {
		return document.RenditionResult{}, err
	}
	pollAttempts := 0
	partialSuccess := task.status == "partial_success"
	for task.status != "success" && !partialSuccess {
		if task.status != "pending" && task.status != "started" {
			return document.RenditionResult{}, taskStatusError(task.status)
		}
		if pollAttempts >= client.maxPollAttempts {
			return document.RenditionResult{}, ambiguousSubmissionError(classifiedError(
				document.RenditionErrorCapacity, "Docling polling limit reached", nil))
		}
		if err := waitContext(totalCtx, client.pollInterval); err != nil {
			if operationErr := checkOperation(totalCtx, expiresAt); operationErr != nil {
				return document.RenditionResult{}, operationErr
			}
			return document.RenditionResult{}, classifiedError(document.RenditionErrorCanceled, "Docling rendering canceled", err)
		}
		nextTask, err := client.poll(totalCtx, expiresAt, usage, task.id)
		pollAttempts++
		if err != nil {
			if document.IsRenditionProviderErrorRetryable(err) {
				if pollAttempts >= client.maxPollAttempts {
					return document.RenditionResult{}, ambiguousSubmissionError(err)
				}
				usage.retries++
				continue
			}
			return document.RenditionResult{}, err
		}
		task = nextTask
		partialSuccess = task.status == "partial_success"
	}
	var result doclingResult
	resultAttempts := 0
	for {
		result, err = client.result(totalCtx, expiresAt, usage, task.id,
			int64(authorization.MaxTotalResultBytes))
		resultAttempts++
		if err == nil {
			break
		}
		if !document.IsRenditionProviderErrorRetryable(err) {
			return document.RenditionResult{}, err
		}
		if resultAttempts >= client.maxPollAttempts {
			return document.RenditionResult{}, ambiguousSubmissionError(err)
		}
		usage.retries++
		if err := waitContext(totalCtx, client.pollInterval); err != nil {
			if operationErr := checkOperation(totalCtx, expiresAt); operationErr != nil {
				return document.RenditionResult{}, operationErr
			}
			return document.RenditionResult{}, classifiedError(document.RenditionErrorCanceled, "Docling rendering canceled", err)
		}
	}
	partialSuccess = partialSuccess || result.status == "partial_success"
	if !partialSuccess && len(result.errors) != 0 {
		return document.RenditionResult{}, malformedError("Docling successful result contains errors", nil)
	}
	if authorization.DiscloseFilename && result.filename != metadata.Filename {
		return document.RenditionResult{}, classifiedError(document.RenditionErrorPolicyRejected,
			"Docling result source identity does not match upload", nil)
	}
	providerMarkdown := result.markdown
	if !includeMarkdown {
		providerMarkdown = nil
	}
	if injectsDocbankFrontmatter(providerMarkdown) {
		return document.RenditionResult{}, malformedError(
			"Docling provider Markdown attempts Docbank frontmatter injection", nil)
	}
	evidence, structured, usable := mapEvidence(result.document, authorization.MediaFamily)
	if !usable {
		if len(providerMarkdown) == 0 || int64(len(providerMarkdown)) > int64(authorization.MaxProviderMarkdownBytes) {
			return document.RenditionResult{}, malformedError("Docling result has no usable bounded evidence", nil)
		}
		evidence = degradedEvidence(authorization.MediaFamily, string(providerMarkdown))
		structured = nil
	}
	if partialSuccess {
		partialPages, err := parsePartialPages(result.errors)
		if err != nil {
			return document.RenditionResult{}, err
		}
		evidence, err = partialSuccessEvidence(evidence, authorization.MediaFamily, partialPages)
		if err != nil {
			return document.RenditionResult{}, err
		}
	}
	if len(providerMarkdown) > authorization.MaxProviderMarkdownBytes {
		return document.RenditionResult{}, malformedError("Docling Markdown exceeds authorization", nil)
	}
	artifacts := make([]document.RenditionArtifact, 0, 1)
	if len(structured) != 0 && allowsStructured(authorization) {
		if len(structured) > authorization.MaxArtifactBytes {
			return document.RenditionResult{}, malformedError("Docling structured output exceeds authorization", nil)
		}
		digest := sha256Hex(structured)
		artifacts = append(artifacts, document.RenditionArtifact{
			Role: document.EvidenceArtifactStructured, MediaType: "application/json", Payload: structured, SHA256: digest,
		})
		evidence.Artifacts = []document.SourceEvidenceArtifactV1{{
			ProviderID: "docling-document", Pointer: "document", Role: document.EvidenceArtifactStructured, SHA256: digest,
		}}
	}
	completed := time.Now().UTC()
	authorizationFingerprint, err := authorization.Fingerprint()
	if err != nil {
		return document.RenditionResult{}, classifiedError(document.RenditionErrorPolicyRejected,
			"authorization fingerprint is invalid", err)
	}
	return document.RenditionResult{
		Evidence: evidence, ProviderMarkdown: append([]byte(nil), providerMarkdown...), Artifacts: artifacts,
		Receipt: document.RenditionReceipt{
			ProviderID: client.descriptor.ID, DescriptorFingerprint: client.descriptor.Fingerprint,
			PolicyFingerprint:           authorization.PolicyFingerprint,
			RenditionRequestFingerprint: authorization.RenditionRequestFingerprint,
			AuthorizationFingerprint:    authorizationFingerprint,
			SourceSHA256:                metadata.SHA256,
			OperationID:                 "docling-" + task.id, StartedAt: started.Format(timestampForm), CompletedAt: completed.Format(timestampForm),
			Warnings: partialSuccessWarnings(partialSuccess),
			Usage: document.RenditionUsage{
				Requests: usage.requests, Retries: usage.retries, InputBytes: int64(len(source)),
				OutputBytes: usage.outputBytes, Units: int64(len(evidence.Units)),
			},
		},
	}, nil
}

type taskResponse struct{ id, status string }

type doclingResult struct {
	markdown []byte
	document json.RawMessage
	filename string
	status   string
	errors   []json.RawMessage
}

func (client *Client) submit(
	ctx context.Context, expiresAt time.Time, usage *requestUsage, metadata document.AuthorizedUploadMetadata, source []byte,
	includeMarkdown bool,
) (taskResponse, error) {
	if strings.ContainsAny(metadata.Filename, "\r\n") {
		return taskResponse{}, classifiedError(document.RenditionErrorPolicyRejected,
			"Docling upload filename contains a newline", nil)
	}
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	fileHeader := make(textproto.MIMEHeader)
	fileHeader.Set("Content-Disposition", multipart.FileContentDisposition("files", metadata.Filename))
	fileHeader.Set("Content-Type", metadata.MediaType)
	part, err := writer.CreatePart(fileHeader)
	if err != nil {
		return taskResponse{}, classifiedError(document.RenditionErrorTransient, "could not prepare Docling upload", err)
	}
	if _, err := part.Write(source); err != nil {
		return taskResponse{}, classifiedError(document.RenditionErrorTransient, "could not prepare Docling upload", err)
	}
	formats := []string{"json"}
	if includeMarkdown {
		formats = []string{"md", "json"}
	}
	for _, format := range formats {
		if err := writer.WriteField("to_formats", format); err != nil {
			return taskResponse{}, classifiedError(document.RenditionErrorTransient, "could not prepare Docling upload", err)
		}
	}
	if err := writer.WriteField("target_type", "inbody"); err != nil {
		return taskResponse{}, classifiedError(document.RenditionErrorTransient, "could not prepare Docling upload", err)
	}
	if err := writer.Close(); err != nil {
		return taskResponse{}, classifiedError(document.RenditionErrorTransient, "could not prepare Docling upload", err)
	}
	bodyBytes, status, err := client.request(ctx, expiresAt, usage, http.MethodPost, convertPath,
		writer.FormDataContentType(), body.Bytes(), client.maxResponseBytes)
	if err != nil {
		if document.IsRenditionProviderErrorRetryable(err) || status == http.StatusOK || status == http.StatusAccepted {
			return taskResponse{}, ambiguousSubmissionError(err)
		}
		return taskResponse{}, err
	}
	if status != http.StatusOK && status != http.StatusAccepted {
		err := statusError("submission", status)
		if status == http.StatusRequestTimeout || status >= http.StatusInternalServerError && status < 600 {
			return taskResponse{}, ambiguousSubmissionError(err)
		}
		return taskResponse{}, err
	}
	task, err := parseTask(bodyBytes)
	if err != nil {
		return taskResponse{}, ambiguousSubmissionError(err)
	}
	return task, nil
}

func ambiguousSubmissionError(cause error) error {
	return classifiedError(document.RenditionErrorAmbiguousSubmission,
		"Docling submission outcome is unknown", cause)
}

func (client *Client) poll(ctx context.Context, expiresAt time.Time, usage *requestUsage, taskID string) (taskResponse, error) {
	body, status, err := client.request(ctx, expiresAt, usage, http.MethodGet, pollPath+taskID,
		"", nil, client.maxResponseBytes)
	if err != nil {
		return taskResponse{}, err
	}
	if status != http.StatusOK && status != http.StatusAccepted {
		return taskResponse{}, statusError("poll", status)
	}
	task, err := parseTask(body)
	if err != nil {
		return taskResponse{}, err
	}
	if task.id != taskID {
		return taskResponse{}, malformedError("Docling task identity changed while polling", nil)
	}
	return task, nil
}

func (client *Client) result(
	ctx context.Context, expiresAt time.Time, usage *requestUsage, taskID string, maxResultBytes int64,
) (doclingResult, error) {
	body, status, err := client.request(ctx, expiresAt, usage, http.MethodGet, resultPath+taskID,
		"", nil, maxResultBytes)
	if err != nil {
		return doclingResult{}, err
	}
	if status != http.StatusOK {
		return doclingResult{}, statusError("result", status)
	}
	var wire struct {
		Status   string            `json:"status"`
		Document json.RawMessage   `json:"document"`
		Errors   []json.RawMessage `json:"errors"`
	}
	if err := json.Unmarshal(body, &wire); err != nil {
		return doclingResult{}, malformedError("Docling result JSON is invalid", err)
	}
	if wire.Status != "success" && wire.Status != "partial_success" {
		return doclingResult{}, taskStatusError(wire.Status)
	}
	var documentWire struct {
		Filename    string          `json:"filename"`
		Markdown    string          `json:"md_content"`
		JSONContent json.RawMessage `json:"json_content"`
	}
	if len(wire.Document) == 0 || json.Unmarshal(wire.Document, &documentWire) != nil || documentWire.Filename == "" {
		return doclingResult{}, classifiedError(document.RenditionErrorPolicyRejected,
			"Docling result source identity is missing", nil)
	}
	if len(documentWire.JSONContent) != 0 && string(documentWire.JSONContent) != "null" {
		var identity struct {
			Origin struct {
				Filename string `json:"filename"`
			} `json:"origin"`
		}
		if json.Unmarshal(documentWire.JSONContent, &identity) == nil && identity.Origin.Filename != "" &&
			identity.Origin.Filename != documentWire.Filename {
			return doclingResult{}, classifiedError(document.RenditionErrorPolicyRejected,
				"Docling result source identity is inconsistent", nil)
		}
	}
	return doclingResult{markdown: []byte(documentWire.Markdown),
		document: append([]byte(nil), documentWire.JSONContent...), filename: documentWire.Filename,
		status: wire.Status, errors: append([]json.RawMessage(nil), wire.Errors...)}, nil
}

func (client *Client) request(
	ctx context.Context, expiresAt time.Time, usage *requestUsage, method, path, contentType string,
	body []byte, maxResponseBytes int64,
) ([]byte, int, error) {
	if err := checkOperation(ctx, expiresAt); err != nil {
		return nil, 0, err
	}
	requestCtx, cancel := context.WithTimeout(ctx, client.requestTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(requestCtx, method, client.origin+path, bytes.NewReader(body))
	if err != nil {
		return nil, 0, classifiedError(document.RenditionErrorTransient, "could not create Docling request", err)
	}
	request.Header.Set("Accept", "application/json")
	if contentType != "" {
		request.Header.Set("Content-Type", contentType)
	}
	if err := client.authorize(request); err != nil {
		return nil, 0, err
	}
	if err := checkOperation(ctx, expiresAt); err != nil {
		return nil, 0, err
	}
	usage.requests++
	response, err := client.http.Do(request)
	if err != nil {
		var requestErr error
		if !time.Now().Before(expiresAt) {
			requestErr = classifiedError(document.RenditionErrorPolicyRejected, "Docling authorization expired", nil)
		} else if ctxErr := ctx.Err(); ctxErr != nil {
			requestErr = classifiedError(document.RenditionErrorCanceled, "Docling rendering canceled", ctxErr)
		} else {
			requestErr = classifiedError(document.RenditionErrorTransient, "Docling request failed", err)
		}
		if method == http.MethodPost {
			requestErr = ambiguousSubmissionError(requestErr)
		}
		return nil, 0, requestErr
	}
	defer func() { _ = response.Body.Close() }()
	responseBody, err := readBounded(response.Body, min(maxResponseBytes, client.maxResponseBytes))
	usage.outputBytes += int64(len(responseBody))
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return responseBody, response.StatusCode, nil
	}
	if err != nil {
		return nil, response.StatusCode, err
	}
	mediaType, _, mediaErr := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if mediaErr != nil || mediaType != "application/json" {
		return nil, response.StatusCode, malformedError("Docling response content type is invalid", mediaErr)
	}
	return responseBody, response.StatusCode, nil
}

func (client *Client) operationContext(ctx context.Context, expiresAt time.Time) (context.Context, context.CancelFunc) {
	deadline := time.Now().Add(client.totalTimeout)
	if callerDeadline, ok := ctx.Deadline(); ok && callerDeadline.Before(deadline) {
		deadline = callerDeadline
	}
	if expiresAt.Before(deadline) {
		deadline = expiresAt
	}
	return context.WithDeadline(ctx, deadline)
}

func checkOperation(ctx context.Context, expiresAt time.Time) error {
	if errors.Is(ctx.Err(), context.Canceled) {
		return classifiedError(document.RenditionErrorCanceled, "Docling rendering canceled", ctx.Err())
	}
	if !time.Now().Before(expiresAt) {
		return classifiedError(document.RenditionErrorPolicyRejected, "Docling authorization expired", nil)
	}
	if err := ctx.Err(); err != nil {
		return classifiedError(document.RenditionErrorCanceled, "Docling rendering canceled", err)
	}
	return nil
}

func (client *Client) authorize(request *http.Request) error {
	if client.secretBinding == "" {
		return nil
	}
	secret, err := client.secrets.ResolveSecret(request.Context(), client.secretBinding)
	if err != nil {
		return classifiedError(document.RenditionErrorAuthentication, "Docling credential is unavailable", err)
	}
	if secret == "" || len(secret) > maxSecretBytes || strings.ContainsAny(secret, "\r\n\x00") {
		return classifiedError(document.RenditionErrorAuthentication, "Docling credential is invalid", nil)
	}
	request.Header.Set("X-Api-Key", secret)
	return nil
}

func parseTask(body []byte) (taskResponse, error) {
	var wire struct {
		ID     string `json:"task_id"`
		Type   string `json:"task_type"`
		Status string `json:"task_status"`
	}
	if err := json.Unmarshal(body, &wire); err != nil {
		return taskResponse{}, malformedError("Docling task response JSON is invalid", err)
	}
	if err := validateTaskID(wire.ID); err != nil {
		return taskResponse{}, malformedError("Docling task ID is invalid", err)
	}
	if wire.Type != "convert" {
		return taskResponse{}, malformedError("Docling task type is invalid", nil)
	}
	if !officialTaskStatus(wire.Status) {
		return taskResponse{}, malformedError("Docling task status is invalid", nil)
	}
	return taskResponse{id: wire.ID, status: wire.Status}, nil
}

func mapEvidence(raw json.RawMessage, family string) (document.SourceEvidenceV1, []byte, bool) {
	var topLevel map[string]json.RawMessage
	if json.Unmarshal(raw, &topLevel) != nil {
		return document.SourceEvidenceV1{}, nil, false
	}
	for field := range topLevel {
		switch field {
		case "schema_name", "version", "name", "origin", "furniture", "body", "groups", "texts", "pictures", "tables",
			"key_value_items", "form_items", "field_regions", "field_items", "pages":
		default:
			return document.SourceEvidenceV1{}, nil, false
		}
	}
	var wire struct {
		SchemaName string `json:"schema_name"`
		Version    string `json:"version"`
		Texts      []struct {
			Text string `json:"text"`
			Prov []struct {
				PageNo int64 `json:"page_no"`
			} `json:"prov"`
		} `json:"texts"`
		Pages         map[string]json.RawMessage `json:"pages"`
		Tables        []json.RawMessage          `json:"tables"`
		Pictures      []json.RawMessage          `json:"pictures"`
		KeyValueItems []json.RawMessage          `json:"key_value_items"`
		FormItems     []json.RawMessage          `json:"form_items"`
		FieldRegions  []json.RawMessage          `json:"field_regions"`
		FieldItems    []json.RawMessage          `json:"field_items"`
	}
	if len(raw) == 0 || json.Unmarshal(raw, &wire) != nil || wire.SchemaName != "DoclingDocument" || !supportedDoclingMajor(wire.Version) || len(wire.Pages) == 0 {
		return document.SourceEvidenceV1{}, nil, false
	}
	kind, locatorKind, natural := familyUnit(family)
	if !natural {
		return document.SourceEvidenceV1{}, nil, false
	}
	pages := make(map[int64][]string, len(wire.Pages))
	for key := range wire.Pages {
		page, err := strconv.ParseInt(key, 10, 64)
		if err != nil || page < 1 || strconv.FormatInt(page, 10) != key {
			return document.SourceEvidenceV1{}, nil, false
		}
		pages[page] = nil
	}
	indexes := make([]int64, 0, len(pages))
	for page := range pages {
		indexes = append(indexes, page)
	}
	slices.Sort(indexes)
	for index, page := range indexes {
		if page != int64(index+1) {
			return document.SourceEvidenceV1{}, nil, false
		}
	}
	for _, text := range wire.Texts {
		if text.Text == "" {
			continue
		}
		if len(text.Prov) == 0 {
			return document.SourceEvidenceV1{}, nil, false
		}
		page := text.Prov[0].PageNo
		if page < 1 {
			return document.SourceEvidenceV1{}, nil, false
		}
		for _, provenance := range text.Prov {
			if provenance.PageNo != page {
				return document.SourceEvidenceV1{}, nil, false
			}
		}
		if _, ok := pages[page]; !ok {
			return document.SourceEvidenceV1{}, nil, false
		}
		pages[page] = append(pages[page], text.Text)
	}
	evidence := document.SourceEvidenceV1{
		ContractVersion: document.SourceEvidenceContractV1, Completeness: document.EvidenceComplete,
		Family: family, UnitKind: kind, Units: make([]document.SourceEvidenceUnitV1, 0, len(indexes)),
	}
	for order, index := range indexes {
		evidence.Units = append(evidence.Units, document.SourceEvidenceUnitV1{
			Order: order, Text: strings.Join(pages[index], "\n\n"),
			Locator: document.SourceEvidenceLocatorV1{Kind: locatorKind, IndexOrigin: document.EvidenceIndexOriginOne, Start: index, End: index},
		})
	}
	for _, field := range []struct {
		name   string
		values []json.RawMessage
	}{
		{name: "tables", values: wire.Tables},
		{name: "pictures", values: wire.Pictures},
		{name: "key_value_items", values: wire.KeyValueItems},
		{name: "form_items", values: wire.FormItems},
		{name: "field_regions", values: wire.FieldRegions},
		{name: "field_items", values: wire.FieldItems},
	} {
		if len(field.values) == 0 {
			continue
		}
		evidence.Omissions = append(evidence.Omissions, document.SourceEvidenceOmissionV1{
			Kind: document.EvidenceOmissionField, Field: field.name, Reason: "Docling structured content is not mapped",
		})
	}
	if len(evidence.Omissions) != 0 {
		evidence.Completeness = document.EvidencePartial
	}
	return evidence, append([]byte(nil), raw...), true
}

func familyUnit(family string) (document.EvidenceUnitKind, document.EvidenceLocatorKind, bool) {
	switch family {
	case "pdf":
		return document.EvidenceUnitPage, document.EvidenceLocatorPage, true
	default:
		return "", "", false
	}
}

func degradedEvidence(family, markdown string) document.SourceEvidenceV1 {
	return document.SourceEvidenceV1{
		ContractVersion: document.SourceEvidenceContractV1, Completeness: document.EvidenceDegradedProvenance,
		Family: family, UnitKind: document.EvidenceUnitGeneric,
		Omissions: []document.SourceEvidenceOmissionV1{{
			Kind: document.EvidenceOmissionField, Field: "natural_provenance", Reason: "Docling structured evidence is unavailable",
		}},
		Units: []document.SourceEvidenceUnitV1{{Order: 0, Text: markdown,
			Locator: document.SourceEvidenceLocatorV1{Kind: document.EvidenceLocatorGeneric, IndexOrigin: document.EvidenceIndexOriginNone}}},
	}
}

func injectsDocbankFrontmatter(markdown []byte) bool {
	frontmatter := bytes.HasPrefix(markdown, []byte("---\n")) ||
		bytes.HasPrefix(markdown, []byte("---\r\n")) ||
		bytes.HasPrefix(markdown, []byte("---\r"))
	return frontmatter && bytes.Contains(markdown, []byte("docbank-sanitized-markdown/v1"))
}

func allowsStructured(authorization document.RenditionAuthorization) bool {
	if slices.Contains(authorization.AllowedArtifactRoles, document.EvidenceArtifactStructured) {
		return authorization.MaxArtifacts > 0 && authorization.MaxArtifactBytes > 0
	}
	return false
}

func readExact(ctx context.Context, upload io.Reader, metadata document.AuthorizedUploadMetadata) ([]byte, error) {
	limited := &io.LimitedReader{R: upload, N: metadata.ByteLength + 1}
	data := make([]byte, 0, 32<<10)
	buffer := make([]byte, 32<<10)
	emptyReads := 0
	for limited.N > 0 {
		if err := ctx.Err(); err != nil {
			return nil, classifiedError(document.RenditionErrorCanceled, "Docling rendering canceled", err)
		}
		count, err := limited.Read(buffer)
		if count > 0 {
			data = append(data, buffer[:count]...)
			emptyReads = 0
		}
		switch {
		case errors.Is(err, io.EOF):
			limited.N = 0
		case err != nil:
			if ctxErr := ctx.Err(); ctxErr != nil {
				return nil, classifiedError(document.RenditionErrorCanceled, "Docling rendering canceled", ctxErr)
			}
			return nil, classifiedError(document.RenditionErrorTransient, "could not read the authorized upload", err)
		case count == 0:
			emptyReads++
			if emptyReads >= maxConsecutiveEmptyReads {
				return nil, classifiedError(document.RenditionErrorTransient,
					"authorized upload stopped making progress", io.ErrNoProgress)
			}
		}
	}
	if int64(len(data)) != metadata.ByteLength || sha256Hex(data) != metadata.SHA256 {
		return nil, classifiedError(document.RenditionErrorPolicyRejected, "authorized upload identity mismatch", nil)
	}
	return data, nil
}

func waitContext(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func validateOrigin(raw string, trust document.RenditionTrustBoundary) (string, error) {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Opaque != "" ||
		parsed.ForceQuery || parsed.Fragment != "" || (parsed.Path != "" && parsed.Path != "/") {
		return "", errors.New("docling: origin must be one absolute origin without path, credentials, query, or fragment")
	}
	if parsed.Scheme != "https" && (parsed.Scheme != "http" || trust != document.RenditionTrustOperatorNetwork) {
		return "", errors.New("docling: hosted origins require HTTPS; HTTP is operator-network only")
	}
	if trust != document.RenditionTrustOperatorNetwork && trust != document.RenditionTrustHostedProvider {
		return "", errors.New("docling: network origin requires an operator-network or hosted trust boundary")
	}
	return parsed.Scheme + "://" + parsed.Host, nil
}

func validateToken(value, subject string) error {
	if value == "" || len(value) > maxTaskIDBytes || value != strings.TrimSpace(value) || !utf8.ValidString(value) {
		return fmt.Errorf("docling: %s is invalid", subject)
	}
	for _, character := range value {
		if (character < 'a' || character > 'z') && (character < 'A' || character > 'Z') &&
			(character < '0' || character > '9') && character != '.' && character != '_' && character != '-' {
			return fmt.Errorf("docling: %s is invalid", subject)
		}
	}
	return nil
}

func validateTaskID(value string) error {
	if value == "" || len(value) > maxTaskIDBytes || value == "." || value == ".." {
		return errors.New("invalid task ID")
	}
	for _, character := range value {
		if (character < 'a' || character > 'z') && (character < '0' || character > '9') && character != '_' && character != '-' {
			return errors.New("invalid task ID")
		}
	}
	return nil
}

func supportedDoclingMajor(version string) bool {
	major, _, ok := strings.Cut(version, ".")
	return ok && major == "1"
}

func cloneDescriptor(value document.RenditionDescriptor) document.RenditionDescriptor {
	value.SupportedFormats = append([]document.RenditionFormatCapability(nil), value.SupportedFormats...)
	value.ArtifactRoles = append([]document.EvidenceArtifactRole(nil), value.ArtifactRoles...)
	return value
}

func readBounded(reader io.Reader, maximum int64) ([]byte, error) {
	value, err := io.ReadAll(io.LimitReader(reader, maximum+1))
	if err != nil {
		return value, classifiedError(document.RenditionErrorTransient, "could not read Docling response", err)
	}
	if int64(len(value)) > maximum {
		return value, malformedError("Docling response exceeds byte limit", nil)
	}
	return value, nil
}

func sha256Hex(value []byte) string {
	digest := sha256.Sum256(value)
	return hex.EncodeToString(digest[:])
}

func taskStatusError(status string) error {
	switch status {
	case "failure":
		return malformedError("Docling task failed", nil)
	case "skipped":
		return classifiedError(document.RenditionErrorPolicyRejected, "Docling task was skipped", nil)
	default:
		return malformedError("Docling task status is unsupported", nil)
	}
}

func officialTaskStatus(status string) bool {
	switch status {
	case "pending", "started", "success", "failure", "partial_success", "skipped":
		return true
	default:
		return false
	}
}

func parsePartialPages(raw []json.RawMessage) ([]int64, error) {
	if len(raw) == 0 {
		return nil, malformedError("Docling partial result has no page omissions", nil)
	}
	pages := make([]int64, 0, len(raw))
	seen := make(map[int64]struct{}, len(raw))
	for _, value := range raw {
		var omission struct {
			PageNo *int64 `json:"page_no"`
		}
		if json.Unmarshal(value, &omission) != nil || omission.PageNo == nil || *omission.PageNo < 1 {
			return nil, malformedError("Docling partial result has an unlocated omission", nil)
		}
		if _, ok := seen[*omission.PageNo]; ok {
			continue
		}
		seen[*omission.PageNo] = struct{}{}
		pages = append(pages, *omission.PageNo)
	}
	return pages, nil
}

func partialSuccessEvidence(
	evidence document.SourceEvidenceV1, family string, pages []int64,
) (document.SourceEvidenceV1, error) {
	if family != "pdf" ||
		(evidence.Completeness != document.EvidenceComplete && evidence.Completeness != document.EvidencePartial) ||
		len(pages) == 0 {
		return document.SourceEvidenceV1{}, malformedError("Docling partial result lacks exact PDF page evidence", nil)
	}
	unitByPage := make(map[int64]int, len(evidence.Units))
	for index := range evidence.Units {
		unitByPage[evidence.Units[index].Locator.Start] = index
	}
	for _, page := range pages {
		index, ok := unitByPage[page]
		if !ok {
			return document.SourceEvidenceV1{}, malformedError("Docling partial result names an unknown page", nil)
		}
		unit := &evidence.Units[index]
		unit.Omissions = append(unit.Omissions, document.SourceEvidenceOmissionV1{
			Kind: document.EvidenceOmissionField, Field: "provider_output", Reason: "Docling reported partial success", UnitOrder: unit.Order,
		})
	}
	evidence.Completeness = document.EvidencePartial
	return evidence, nil
}

func partialSuccessWarnings(partial bool) []string {
	if !partial {
		return nil
	}
	return []string{"partial_success"}
}

func statusError(operation string, status int) error {
	switch status {
	case http.StatusUnauthorized, http.StatusForbidden:
		return classifiedError(document.RenditionErrorAuthentication, "Docling authentication failed", nil)
	case http.StatusNotFound, http.StatusGone:
		if operation == "poll" || operation == "result" {
			return classifiedError(document.RenditionErrorUnknownJob, "Docling task is unknown or expired", nil)
		}
		return malformedError("Docling returned an unexpected HTTP status", nil)
	case http.StatusBadRequest, http.StatusRequestEntityTooLarge, http.StatusUnsupportedMediaType, http.StatusUnprocessableEntity:
		if operation == "submission" {
			return classifiedError(document.RenditionErrorPolicyRejected, "Docling rejected the submitted input", nil)
		}
		return malformedError("Docling returned an unexpected HTTP status", nil)
	case http.StatusTooManyRequests:
		return classifiedError(document.RenditionErrorRateLimited, "Docling rate limit", nil)
	case http.StatusRequestTimeout, http.StatusInternalServerError, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return classifiedError(document.RenditionErrorTransient, "Docling is temporarily unavailable", nil)
	default:
		return malformedError("Docling returned an unexpected HTTP status", nil)
	}
}

func malformedError(message string, cause error) error {
	return classifiedError(document.RenditionErrorMalformedEvidence, message, cause)
}

func classifiedError(code document.RenditionErrorCode, message string, cause error) error {
	if cause == nil {
		cause = errors.New(message)
	} else {
		cause = fmt.Errorf("%s: %w", message, cause)
	}
	providerError, err := document.NewRenditionProviderError(code, 0, cause)
	if err == nil {
		return providerError
	}
	fallback, fallbackErr := document.NewRenditionProviderError(document.RenditionErrorMalformedEvidence,
		0, fmt.Errorf("docling returned an invalid error: %w", err))
	if fallbackErr == nil {
		return fallback
	}
	return errors.Join(err, fallbackErr)
}

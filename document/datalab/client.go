// Package datalab implements the fixed uploaded-file Datalab Convert flow.
package datalab

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

	xhtml "golang.org/x/net/html"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/providerhttp"
)

const (
	providerID  = "datalab.convert-v1"
	convertPath = "/api/v1/convert"

	defaultRequestTimeout   = 30 * time.Second
	defaultTotalTimeout     = 10 * time.Minute
	defaultPollInterval     = 2 * time.Second
	defaultMaxPollAttempts  = 300
	defaultMaxResponseBytes = int64(64 << 20)
	defaultMaxDocumentBytes = int64(200 << 20)
	maxTimeout              = 24 * time.Hour
	maxPollAttempts         = 10_000
	maxResponseBytes        = int64(512 << 20)
	maxDocumentBytes        = int64(200 << 20)
	maxSecretBytes          = 64 << 10
	maxRequestIDBytes       = 120
	timestampForm           = "2006-01-02T15:04:05.000000000Z"
)

var _ document.RenditionProvider = (*Client)(nil)

// SecretResolver resolves the one profile-bound Datalab API-key binding.
type SecretResolver interface {
	ResolveSecret(ctx context.Context, name string) (string, error)
}

// Profile fixes one Datalab origin, descriptor, credential, conversion mode,
// optional provider-version snapshot, and every network/result bound.
type Profile struct {
	Origin           string
	Descriptor       document.RenditionDescriptor
	SecretBinding    string
	Mode             string
	ExpectedVersions json.RawMessage
	RequestTimeout   time.Duration
	TotalTimeout     time.Duration
	PollInterval     time.Duration
	MaxPollAttempts  int
	MaxResponseBytes int64
	MaxDocumentBytes int64
}

// Client renders exact authorized uploads through fixed Datalab Convert routes.
type Client struct {
	origin                      string
	descriptor                  document.RenditionDescriptor
	secretBinding               string
	secrets                     SecretResolver
	http                        *http.Client
	mode                        string
	expectedVersionsFingerprint string
	requestTimeout              time.Duration
	totalTimeout                time.Duration
	pollInterval                time.Duration
	maxPollAttempts             int
	maxResponseBytes            int64
	maxDocumentBytes            int64
}

type requestUsage struct {
	requests    int64
	retries     int64
	outputBytes int64
}

type versionState struct {
	expected string
	observed string
}

// New validates a fixed hosted profile and isolates the supplied HTTP client
// from ambient cookies and redirect behavior.
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
		return nil, fmt.Errorf("datalab: invalid descriptor: %w", err)
	}
	if descriptor.ID != providerID {
		return nil, errors.New("datalab: descriptor ID must be datalab.convert-v1")
	}
	if profile.SecretBinding == "" || secrets == nil {
		return nil, errors.New("datalab: a named secret binding and resolver are required")
	}
	if err := validateToken(profile.SecretBinding, "secret binding"); err != nil {
		return nil, err
	}
	if httpClient == nil {
		return nil, errors.New("datalab: HTTP client is required")
	}
	if profile.Mode == "" {
		profile.Mode = "balanced"
	}
	if !slices.Contains([]string{"fast", "balanced", "accurate"}, profile.Mode) {
		return nil, errors.New("datalab: mode must be fast, balanced, or accurate")
	}
	expectedVersionsFingerprint := ""
	if len(profile.ExpectedVersions) != 0 {
		expectedVersionsFingerprint, err = versionsFingerprint(profile.ExpectedVersions)
		if err != nil || expectedVersionsFingerprint == "" {
			return nil, errors.New("datalab: expected versions must be a non-null JSON object or string")
		}
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
		return nil, errors.New("datalab: execution bounds are invalid")
	}
	isolate := *httpClient
	isolate.Jar = nil
	isolate.CheckRedirect = providerhttp.RefuseRedirects
	return &Client{
		origin: origin, descriptor: cloneDescriptor(descriptor), secretBinding: profile.SecretBinding,
		secrets: secrets, http: &isolate, mode: profile.Mode,
		expectedVersionsFingerprint: expectedVersionsFingerprint,
		requestTimeout:              profile.RequestTimeout, totalTimeout: profile.TotalTimeout,
		pollInterval: profile.PollInterval, maxPollAttempts: profile.MaxPollAttempts,
		maxResponseBytes: profile.MaxResponseBytes, maxDocumentBytes: profile.MaxDocumentBytes,
	}, nil
}

// Descriptor returns an immutable copy of the configured identity.
func (client *Client) Descriptor() document.RenditionDescriptor {
	if client == nil {
		return document.RenditionDescriptor{}
	}
	return cloneDescriptor(client.descriptor)
}

// Render verifies sealed bytes before egress, submits them once, and polls only
// the fixed same-origin route derived from the returned request ID.
func (client *Client) Render(
	ctx context.Context, upload document.AuthorizedUpload, authorization document.RenditionAuthorization,
) (document.RenditionResult, error) {
	if client == nil {
		return document.RenditionResult{}, errors.New("datalab: client is required")
	}
	if _, err := document.ValidateRenditionProviderRequest(client, upload, authorization); err != nil {
		return document.RenditionResult{}, err
	}
	metadata := upload.Metadata()
	if metadata.ByteLength > client.maxDocumentBytes {
		return document.RenditionResult{}, classifiedError(document.RenditionErrorPolicyRejected,
			"input exceeds the Datalab byte limit", nil)
	}
	expiresAt, err := time.Parse(timestampForm, authorization.ExpiresAt)
	if err != nil {
		return document.RenditionResult{}, classifiedError(document.RenditionErrorPolicyRejected,
			"Datalab authorization expiry is invalid", nil)
	}
	totalCtx, cancel := client.operationContext(ctx, expiresAt)
	defer cancel()
	if err := checkOperation(totalCtx, expiresAt); err != nil {
		return document.RenditionResult{}, err
	}
	source, err := readExact(totalCtx, upload, metadata)
	if err != nil {
		if !time.Now().Before(expiresAt) {
			return document.RenditionResult{}, classifiedError(document.RenditionErrorPolicyRejected, "Datalab authorization expired", nil)
		}
		return document.RenditionResult{}, err
	}
	started := time.Now().UTC()
	usage := &requestUsage{}
	versions := &versionState{expected: client.expectedVersionsFingerprint}
	requestID, err := client.submit(totalCtx, expiresAt, usage, versions, metadata, source)
	if err != nil {
		return document.RenditionResult{}, err
	}

	var result finalResponse
	for range client.maxPollAttempts {
		result, err = client.poll(totalCtx, expiresAt, usage, versions, requestID)
		if err != nil {
			if !document.IsRenditionProviderErrorRetryable(err) {
				return document.RenditionResult{}, err
			}
			usage.retries++
			if waitErr := waitContext(totalCtx, client.pollInterval); waitErr != nil {
				return document.RenditionResult{}, operationFailure(totalCtx, expiresAt, waitErr)
			}
			continue
		}
		if result.status == "complete" {
			return client.buildResult(result, requestID, metadata, authorization, source, started, usage)
		}
		if result.status != "processing" {
			return document.RenditionResult{}, malformedError("Datalab result status is unsupported", nil)
		}
		if waitErr := waitContext(totalCtx, client.pollInterval); waitErr != nil {
			return document.RenditionResult{}, operationFailure(totalCtx, expiresAt, waitErr)
		}
	}
	return document.RenditionResult{}, ambiguousJobError()
}

type initialResponse struct {
	success  bool
	id       string
	versions json.RawMessage
}

type finalResponse struct {
	status     string
	success    *bool
	markdown   []byte
	structured json.RawMessage
	pageCount  int
	versions   json.RawMessage
}

func (client *Client) submit(
	ctx context.Context, expiresAt time.Time, usage *requestUsage, versions *versionState,
	metadata document.AuthorizedUploadMetadata, source []byte,
) (string, error) {
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	header := make(textproto.MIMEHeader)
	header.Set("Content-Disposition", multipart.FileContentDisposition("file", metadata.Filename))
	header.Set("Content-Type", metadata.MediaType)
	part, err := writer.CreatePart(header)
	if err != nil {
		return "", classifiedError(document.RenditionErrorTransient, "could not prepare Datalab upload", err)
	}
	if _, err := part.Write(source); err != nil {
		return "", classifiedError(document.RenditionErrorTransient, "could not prepare Datalab upload", err)
	}
	for name, value := range map[string]string{
		"output_format": "markdown,json", "mode": client.mode, "paginate": "true", "disable_image_extraction": "true",
	} {
		if err := writer.WriteField(name, value); err != nil {
			return "", classifiedError(document.RenditionErrorTransient, "could not prepare Datalab upload", err)
		}
	}
	if err := writer.Close(); err != nil {
		return "", classifiedError(document.RenditionErrorTransient, "could not prepare Datalab upload", err)
	}
	responseBody, status, err := client.request(ctx, expiresAt, usage, http.MethodPost, convertPath, writer.FormDataContentType(), body.Bytes())
	if err != nil {
		if document.IsRenditionProviderErrorRetryable(err) {
			return "", ambiguousSubmissionError()
		}
		return "", err
	}
	if status != http.StatusOK && status != http.StatusAccepted {
		err = statusError("submission", status)
		if document.IsRenditionProviderErrorRetryable(err) {
			return "", ambiguousSubmissionError()
		}
		return "", err
	}
	initial, err := parseInitial(responseBody)
	if err != nil {
		return "", err
	}
	if err := versions.observe(initial.versions); err != nil {
		return "", err
	}
	return initial.id, nil
}

func (client *Client) poll(
	ctx context.Context, expiresAt time.Time, usage *requestUsage, versions *versionState, requestID string,
) (finalResponse, error) {
	body, status, err := client.request(ctx, expiresAt, usage, http.MethodGet, convertPath+"/"+requestID, "", nil)
	if err != nil {
		return finalResponse{}, err
	}
	if status != http.StatusOK && status != http.StatusAccepted {
		return finalResponse{}, statusError("poll", status)
	}
	result, err := parseFinal(body)
	if err != nil {
		return finalResponse{}, err
	}
	if err := versions.observe(result.versions); err != nil {
		return finalResponse{}, err
	}
	return result, nil
}

func (client *Client) buildResult(
	result finalResponse, requestID string, metadata document.AuthorizedUploadMetadata,
	authorization document.RenditionAuthorization, source []byte, started time.Time, usage *requestUsage,
) (document.RenditionResult, error) {
	if result.success == nil || !*result.success {
		return document.RenditionResult{}, malformedError("Datalab completed without a successful result", nil)
	}
	if len(result.markdown) == 0 || len(result.markdown) > authorization.MaxProviderMarkdownBytes {
		return document.RenditionResult{}, malformedError("Datalab result has no usable bounded Markdown", nil)
	}
	evidence, structured, usable := mapEvidence(result.structured, authorization.MediaFamily, result.pageCount)
	if !usable {
		evidence = degradedEvidence(authorization.MediaFamily, string(result.markdown))
		structured = nil
	}
	artifacts := make([]document.RenditionArtifact, 0, 1)
	if len(structured) != 0 && allowsStructured(authorization) {
		if len(structured) > authorization.MaxArtifactBytes {
			return document.RenditionResult{}, malformedError("Datalab structured output exceeds authorization", nil)
		}
		digest := sha256Hex(structured)
		artifacts = append(artifacts, document.RenditionArtifact{
			Role: document.EvidenceArtifactStructured, MediaType: "application/json", Payload: append([]byte(nil), structured...), SHA256: digest,
		})
		evidence.Artifacts = []document.SourceEvidenceArtifactV1{{
			ProviderID: "datalab-document", Pointer: "json", Role: document.EvidenceArtifactStructured, SHA256: digest,
		}}
	}
	completed := time.Now().UTC()
	authorizationFingerprint, err := authorization.Fingerprint()
	if err != nil {
		return document.RenditionResult{}, fmt.Errorf("datalab: fingerprint authorization: %w", err)
	}
	return document.RenditionResult{
		Evidence: evidence, ProviderMarkdown: append([]byte(nil), result.markdown...), Artifacts: artifacts,
		Receipt: document.RenditionReceipt{
			ProviderID: client.descriptor.ID, DescriptorFingerprint: client.descriptor.Fingerprint,
			PolicyFingerprint:           authorization.PolicyFingerprint,
			RenditionRequestFingerprint: authorization.RenditionRequestFingerprint,
			AuthorizationFingerprint:    authorizationFingerprint, SourceSHA256: metadata.SHA256,
			OperationID: "datalab-" + requestID, StartedAt: started.Format(timestampForm), CompletedAt: completed.Format(timestampForm),
			Usage: document.RenditionUsage{
				Requests: usage.requests, Retries: usage.retries, InputBytes: int64(len(source)),
				OutputBytes: usage.outputBytes, Units: int64(len(evidence.Units)),
			},
		},
	}, nil
}

func (client *Client) request(
	ctx context.Context, expiresAt time.Time, usage *requestUsage, method, path, contentType string, body []byte,
) ([]byte, int, error) {
	if err := checkOperation(ctx, expiresAt); err != nil {
		return nil, 0, err
	}
	requestCtx, cancel := context.WithTimeout(ctx, client.requestTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(requestCtx, method, client.origin+path, bytes.NewReader(body))
	if err != nil {
		return nil, 0, classifiedError(document.RenditionErrorTransient, "could not create Datalab request", err)
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
		if !time.Now().Before(expiresAt) {
			return nil, 0, classifiedError(document.RenditionErrorPolicyRejected, "Datalab authorization expired", nil)
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, 0, classifiedError(document.RenditionErrorCanceled, "Datalab rendering canceled", ctxErr)
		}
		return nil, 0, classifiedError(document.RenditionErrorTransient, "Datalab request failed", err)
	}
	defer func() { _ = response.Body.Close() }()
	responseBody, err := readBounded(response.Body, client.maxResponseBytes)
	usage.outputBytes += int64(len(responseBody))
	if err != nil {
		return nil, response.StatusCode, err
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return responseBody, response.StatusCode, nil
	}
	mediaType, _, mediaErr := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if mediaErr != nil || mediaType != "application/json" {
		return nil, response.StatusCode, malformedError("Datalab response content type is invalid", mediaErr)
	}
	return responseBody, response.StatusCode, nil
}

func (client *Client) authorize(request *http.Request) error {
	secret, err := client.secrets.ResolveSecret(request.Context(), client.secretBinding)
	if err != nil {
		return classifiedError(document.RenditionErrorAuthentication, "Datalab credential is unavailable", err)
	}
	if secret == "" || len(secret) > maxSecretBytes || strings.ContainsAny(secret, "\r\n\x00") {
		return classifiedError(document.RenditionErrorAuthentication, "Datalab credential is invalid", nil)
	}
	request.Header.Set("X-Api-Key", secret)
	return nil
}

func parseInitial(body []byte) (initialResponse, error) {
	var wire struct {
		Success         *bool           `json:"success"`
		RequestID       string          `json:"request_id"`
		RequestCheckURL string          `json:"request_check_url"`
		Versions        json.RawMessage `json:"versions"`
	}
	if err := json.Unmarshal(body, &wire); err != nil {
		return initialResponse{}, malformedError("Datalab submission JSON is invalid", err)
	}
	if wire.Success == nil || !*wire.Success {
		return initialResponse{}, malformedError("Datalab rejected the conversion request", nil)
	}
	if err := validateRequestID(wire.RequestID); err != nil {
		return initialResponse{}, malformedError("Datalab request ID is invalid", err)
	}
	if wire.RequestCheckURL == "" || !utf8.ValidString(wire.RequestCheckURL) {
		return initialResponse{}, malformedError("Datalab request check URL is missing", nil)
	}
	return initialResponse{success: true, id: wire.RequestID, versions: cloneRaw(wire.Versions)}, nil
}

func parseFinal(body []byte) (finalResponse, error) {
	var wire struct {
		Status       string          `json:"status"`
		Success      *bool           `json:"success"`
		OutputFormat string          `json:"output_format"`
		Markdown     *string         `json:"markdown"`
		JSON         json.RawMessage `json:"json"`
		PageCount    *int            `json:"page_count"`
		Versions     json.RawMessage `json:"versions"`
	}
	if err := json.Unmarshal(body, &wire); err != nil {
		return finalResponse{}, malformedError("Datalab result JSON is invalid", err)
	}
	if wire.Status != "processing" && wire.Status != "complete" && wire.Status != "failed" {
		return finalResponse{}, malformedError("Datalab result status is unsupported", nil)
	}
	if wire.Status == "failed" {
		return finalResponse{}, malformedError("Datalab conversion failed", nil)
	}
	if wire.PageCount != nil && *wire.PageCount < 0 {
		return finalResponse{}, malformedError("Datalab page count is invalid", nil)
	}
	if wire.OutputFormat != "" && !containsOutputFormat(wire.OutputFormat, "markdown") {
		return finalResponse{}, malformedError("Datalab result omitted requested Markdown format", nil)
	}
	structured, err := decodeStructured(wire.JSON)
	if err != nil {
		return finalResponse{}, malformedError("Datalab structured result is invalid", err)
	}
	result := finalResponse{status: wire.Status, success: wire.Success, structured: structured, versions: cloneRaw(wire.Versions)}
	if wire.Markdown != nil {
		result.markdown = []byte(*wire.Markdown)
	}
	if wire.PageCount != nil {
		result.pageCount = *wire.PageCount
	}
	return result, nil
}

func mapEvidence(raw json.RawMessage, family string, pageCount int) (document.SourceEvidenceV1, []byte, bool) {
	if len(raw) == 0 {
		return document.SourceEvidenceV1{}, nil, false
	}
	var root markerNode
	if json.Unmarshal(raw, &root) != nil || root.BlockType != "Document" || len(root.Children) == 0 {
		return document.SourceEvidenceV1{}, nil, false
	}
	kind, locatorKind, natural := familyUnit(family)
	if !natural {
		return document.SourceEvidenceV1{}, nil, false
	}
	pages := make(map[int]markerNode, len(root.Children))
	for _, child := range root.Children {
		if child.BlockType != "Page" {
			return document.SourceEvidenceV1{}, nil, false
		}
		page, ok := pageIndex(child.ID)
		if !ok {
			return document.SourceEvidenceV1{}, nil, false
		}
		if _, exists := pages[page]; exists {
			return document.SourceEvidenceV1{}, nil, false
		}
		pages[page] = child
	}
	if pageCount > 0 && pageCount != len(pages) {
		return document.SourceEvidenceV1{}, nil, false
	}
	evidence := document.SourceEvidenceV1{
		ContractVersion: document.SourceEvidenceContractV1, Completeness: document.EvidenceComplete,
		Family: family, UnitKind: kind, Units: make([]document.SourceEvidenceUnitV1, 0, len(pages)),
	}
	for index := range len(pages) {
		page, ok := pages[index]
		if !ok {
			return document.SourceEvidenceV1{}, nil, false
		}
		text, ok := nodeText(page)
		if !ok {
			return document.SourceEvidenceV1{}, nil, false
		}
		evidence.Units = append(evidence.Units, document.SourceEvidenceUnitV1{
			Order: index, ProviderID: page.ID, Text: text,
			Locator: document.SourceEvidenceLocatorV1{
				Kind: locatorKind, IndexOrigin: document.EvidenceIndexOriginZero, Start: int64(index), End: int64(index),
			},
		})
	}
	return evidence, append([]byte(nil), raw...), true
}

type markerNode struct {
	ID        string       `json:"id"`
	BlockType string       `json:"block_type"`
	HTML      string       `json:"html"`
	Children  []markerNode `json:"children"`
}

func nodeText(node markerNode) (string, bool) {
	parts := make([]string, 0)
	var walk func(markerNode) bool
	walk = func(current markerNode) bool {
		if len(current.Children) != 0 {
			for _, child := range current.Children {
				if !walk(child) {
					return false
				}
			}
			return true
		}
		if current.HTML == "" {
			return current.BlockType != ""
		}
		text, err := htmlText(current.HTML)
		if err != nil {
			return false
		}
		if text != "" {
			parts = append(parts, text)
		}
		return true
	}
	if !walk(node) {
		return "", false
	}
	return strings.Join(parts, "\n\n"), true
}

func htmlText(value string) (string, error) {
	documentNode, err := xhtml.Parse(strings.NewReader(value))
	if err != nil {
		return "", fmt.Errorf("parse Datalab block HTML: %w", err)
	}
	parts := make([]string, 0)
	var walk func(*xhtml.Node, bool)
	walk = func(node *xhtml.Node, hidden bool) {
		if node.Type == xhtml.ElementNode && (node.Data == "script" || node.Data == "style") {
			hidden = true
		}
		if node.Type == xhtml.TextNode && !hidden {
			if text := strings.TrimSpace(node.Data); text != "" {
				parts = append(parts, text)
			}
		}
		for child := node.FirstChild; child != nil; child = child.NextSibling {
			walk(child, hidden)
		}
	}
	walk(documentNode, false)
	return strings.Join(parts, " "), nil
}

func familyUnit(family string) (document.EvidenceUnitKind, document.EvidenceLocatorKind, bool) {
	switch family {
	case "pdf", "image", "word":
		return document.EvidenceUnitPage, document.EvidenceLocatorPage, true
	case "presentation":
		return document.EvidenceUnitSlide, document.EvidenceLocatorSlide, true
	case "spreadsheet":
		return document.EvidenceUnitSheet, document.EvidenceLocatorSheet, true
	default:
		return "", "", false
	}
}

func degradedEvidence(family, markdown string) document.SourceEvidenceV1 {
	return document.SourceEvidenceV1{
		ContractVersion: document.SourceEvidenceContractV1, Completeness: document.EvidenceDegradedProvenance,
		Family: family, UnitKind: document.EvidenceUnitGeneric,
		Omissions: []document.SourceEvidenceOmissionV1{{
			Kind: document.EvidenceOmissionField, Field: "natural_provenance", Reason: "Datalab structured evidence is unavailable",
		}},
		Units: []document.SourceEvidenceUnitV1{{Order: 0, Text: markdown,
			Locator: document.SourceEvidenceLocatorV1{Kind: document.EvidenceLocatorGeneric, IndexOrigin: document.EvidenceIndexOriginNone}}},
	}
}

func (state *versionState) observe(raw json.RawMessage) error {
	fingerprint, err := versionsFingerprint(raw)
	if err != nil {
		return classifiedError(document.RenditionErrorPolicyRejected, "Datalab provider versions are malformed", err)
	}
	if state.expected != "" && fingerprint != state.expected {
		return classifiedError(document.RenditionErrorPolicyRejected, "Datalab provider version drift detected", nil)
	}
	if fingerprint == "" {
		return nil
	}
	if state.observed != "" && fingerprint != state.observed {
		return classifiedError(document.RenditionErrorPolicyRejected, "Datalab provider version changed during conversion", nil)
	}
	state.observed = fingerprint
	return nil
}

func versionsFingerprint(raw json.RawMessage) (string, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return "", nil
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return "", err
	}
	if decoder.Decode(new(any)) != io.EOF {
		return "", errors.New("trailing JSON")
	}
	switch value.(type) {
	case map[string]any, string:
	default:
		return "", errors.New("versions are not an object or string")
	}
	canonical, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	return sha256Hex(canonical), nil
}

func decodeStructured(raw json.RawMessage) (json.RawMessage, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	if raw[0] == '"' {
		var value string
		if err := json.Unmarshal(raw, &value); err != nil {
			return nil, err
		}
		raw = []byte(value)
	}
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil, err
	}
	if _, ok := value.(map[string]any); !ok {
		return nil, errors.New("structured output is not an object")
	}
	return append([]byte(nil), raw...), nil
}

func containsOutputFormat(value, wanted string) bool {
	for item := range strings.SplitSeq(value, ",") {
		if strings.TrimSpace(item) == wanted {
			return true
		}
	}
	return false
}

func pageIndex(value string) (int, bool) {
	if !strings.HasPrefix(value, "/page/") {
		return 0, false
	}
	rest := strings.TrimPrefix(value, "/page/")
	indexText, _, ok := strings.Cut(rest, "/")
	if !ok || indexText == "" {
		return 0, false
	}
	index, err := strconv.Atoi(indexText)
	return index, err == nil && index >= 0
}

func allowsStructured(authorization document.RenditionAuthorization) bool {
	return slices.Contains(authorization.AllowedArtifactRoles, document.EvidenceArtifactStructured) &&
		authorization.MaxArtifacts > 0 && authorization.MaxArtifactBytes > 0
}

func readExact(ctx context.Context, upload io.Reader, metadata document.AuthorizedUploadMetadata) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, classifiedError(document.RenditionErrorCanceled, "Datalab rendering canceled", err)
	}
	data, err := io.ReadAll(io.LimitReader(upload, metadata.ByteLength+1))
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, classifiedError(document.RenditionErrorCanceled, "Datalab rendering canceled", ctxErr)
		}
		return nil, classifiedError(document.RenditionErrorTransient, "could not read the authorized upload", err)
	}
	if int64(len(data)) != metadata.ByteLength || sha256Hex(data) != metadata.SHA256 {
		return nil, classifiedError(document.RenditionErrorPolicyRejected, "authorized upload identity mismatch", nil)
	}
	return data, nil
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
		return classifiedError(document.RenditionErrorCanceled, "Datalab rendering canceled", ctx.Err())
	}
	if !time.Now().Before(expiresAt) {
		return classifiedError(document.RenditionErrorPolicyRejected, "Datalab authorization expired", nil)
	}
	if err := ctx.Err(); err != nil {
		return classifiedError(document.RenditionErrorCanceled, "Datalab rendering canceled", err)
	}
	return nil
}

func operationFailure(ctx context.Context, expiresAt time.Time, cause error) error {
	if err := checkOperation(ctx, expiresAt); err != nil {
		return err
	}
	return classifiedError(document.RenditionErrorCanceled, "Datalab rendering canceled", cause)
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
		return "", errors.New("datalab: origin must be one absolute origin without path, credentials, query, or fragment")
	}
	if parsed.Scheme != "https" {
		return "", errors.New("datalab: hosted origins require HTTPS")
	}
	if trust != document.RenditionTrustHostedProvider {
		return "", errors.New("datalab: descriptor trust boundary must be hosted_provider")
	}
	return parsed.Scheme + "://" + parsed.Host, nil
}

func validateToken(value, subject string) error {
	if value == "" || len(value) > maxRequestIDBytes || value != strings.TrimSpace(value) || !utf8.ValidString(value) {
		return fmt.Errorf("datalab: %s is invalid", subject)
	}
	for _, character := range value {
		if (character < 'a' || character > 'z') && (character < 'A' || character > 'Z') &&
			(character < '0' || character > '9') && character != '.' && character != '_' && character != '-' {
			return fmt.Errorf("datalab: %s is invalid", subject)
		}
	}
	return nil
}

func validateRequestID(value string) error {
	if value == "" || len(value) > maxRequestIDBytes || value == "." || value == ".." {
		return errors.New("invalid request ID")
	}
	for _, character := range value {
		if (character < 'a' || character > 'z') && (character < '0' || character > '9') &&
			character != '_' && character != '-' {
			return errors.New("invalid request ID")
		}
	}
	return nil
}

func cloneDescriptor(value document.RenditionDescriptor) document.RenditionDescriptor {
	value.SupportedFormats = append([]document.RenditionFormatCapability(nil), value.SupportedFormats...)
	value.ArtifactRoles = append([]document.EvidenceArtifactRole(nil), value.ArtifactRoles...)
	return value
}

func cloneRaw(value json.RawMessage) json.RawMessage { return append(json.RawMessage(nil), value...) }

func readBounded(reader io.Reader, maximum int64) ([]byte, error) {
	value, err := io.ReadAll(io.LimitReader(reader, maximum+1))
	if err != nil {
		return value, classifiedError(document.RenditionErrorTransient, "could not read Datalab response", err)
	}
	if int64(len(value)) > maximum {
		return value, malformedError("Datalab response exceeds byte limit", nil)
	}
	return value, nil
}

func sha256Hex(value []byte) string {
	digest := sha256.Sum256(value)
	return hex.EncodeToString(digest[:])
}

func statusError(operation string, status int) error {
	switch status {
	case http.StatusUnauthorized, http.StatusForbidden:
		return classifiedError(document.RenditionErrorAuthentication, "Datalab authentication failed", nil)
	case http.StatusNotFound, http.StatusGone:
		if operation == "poll" {
			return classifiedError(document.RenditionErrorUnknownJob, "Datalab request is unknown or expired", nil)
		}
		return malformedError("Datalab returned an unexpected HTTP status", nil)
	case http.StatusUnsupportedMediaType:
		if operation == "submission" {
			return classifiedError(document.RenditionErrorUnsupportedInput, "Datalab does not support the submitted input", nil)
		}
		return malformedError("Datalab returned an unexpected HTTP status", nil)
	case http.StatusRequestEntityTooLarge:
		if operation == "submission" {
			return classifiedError(document.RenditionErrorPolicyRejected, "Datalab rejected the input size", nil)
		}
		return malformedError("Datalab returned an unexpected HTTP status", nil)
	case http.StatusBadRequest, http.StatusUnprocessableEntity:
		if operation == "submission" {
			return classifiedError(document.RenditionErrorPolicyRejected, "Datalab rejected the submitted input", nil)
		}
		return malformedError("Datalab returned an unexpected HTTP status", nil)
	case http.StatusTooManyRequests:
		return classifiedError(document.RenditionErrorRateLimited, "Datalab rate limit", nil)
	case http.StatusServiceUnavailable:
		return classifiedError(document.RenditionErrorCapacity, "Datalab capacity is temporarily unavailable", nil)
	case http.StatusRequestTimeout, http.StatusInternalServerError, http.StatusBadGateway, http.StatusGatewayTimeout:
		return classifiedError(document.RenditionErrorTransient, "Datalab is temporarily unavailable", nil)
	default:
		return malformedError("Datalab returned an unexpected HTTP status", nil)
	}
}

func ambiguousSubmissionError() error {
	return classifiedError(document.RenditionErrorAmbiguousSubmission, "Datalab submission outcome is unknown", nil)
}

func ambiguousJobError() error {
	return classifiedError(document.RenditionErrorAmbiguousSubmission, "Datalab job outcome is unknown", nil)
}

func malformedError(message string, cause error) error {
	return classifiedError(document.RenditionErrorMalformedEvidence, message, cause)
}

func classifiedError(code document.RenditionErrorCode, message string, cause error) error {
	providerError, err := document.NewRenditionProviderError(code, message, 0, cause)
	if err == nil {
		return providerError
	}
	fallback, fallbackErr := document.NewRenditionProviderError(document.RenditionErrorMalformedEvidence,
		"Datalab returned an invalid error", 0, err)
	if fallbackErr == nil {
		return fallback
	}
	return errors.Join(err, fallbackErr)
}

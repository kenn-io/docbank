// Package datalab implements the fixed uploaded-file Datalab Convert flow.
package datalab

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	xhtml "golang.org/x/net/html"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/internal/providerutil"
	"go.kenn.io/docbank/document/providerhttp"
)

const (
	providerID  = "datalab.convert-v1"
	convertPath = "/api/v1/convert"
	provider    = providerutil.Provider("Datalab")

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
)

var _ document.RenditionProvider = (*Client)(nil)

// SecretResolver resolves the one profile-bound Datalab API-key binding.
type SecretResolver = providerutil.SecretResolver

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
	executor                    providerutil.Executor
	descriptor                  document.RenditionDescriptor
	mode                        string
	expectedVersionsFingerprint string
	totalTimeout                time.Duration
	pollInterval                time.Duration
	maxPollAttempts             int
	maxDocumentBytes            int64
}

type versionState struct {
	expected string
	observed string
}

// rendering carries the state of one Render call between its stages.
type rendering struct {
	operation       *providerutil.Operation
	usage           providerutil.Usage
	versions        versionState
	metadata        document.AuthorizedUploadMetadata
	authorization   document.RenditionAuthorization
	source          []byte
	includeMarkdown bool
	started         time.Time
}

// New validates a fixed hosted profile and isolates the supplied HTTP client
// from ambient cookies and redirect behavior.
func New(profile Profile, secrets SecretResolver, httpClient *http.Client) (*Client, error) {
	origin, err := provider.ValidateOrigin(profile.Origin, profile.Descriptor.TrustBoundary)
	if err != nil {
		return nil, err
	}
	if profile.Descriptor.TrustBoundary != document.RenditionTrustHostedProvider {
		return nil, errors.New("datalab: descriptor trust boundary must be hosted_provider")
	}
	descriptor, err := provider.CanonicalDescriptor(profile.Descriptor)
	if err != nil {
		return nil, err
	}
	if descriptor.ID != providerID {
		return nil, errors.New("datalab: descriptor ID must be datalab.convert-v1")
	}
	if profile.SecretBinding == "" || providerutil.IsNil(secrets) {
		return nil, errors.New("datalab: a named secret binding and resolver are required")
	}
	credential := providerutil.APIKeyCredential("X-Api-Key", profile.SecretBinding, secrets)
	if err := credential.Validate(provider); err != nil {
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
	if !providerutil.Bounded(&profile.RequestTimeout, defaultRequestTimeout, maxTimeout) ||
		!providerutil.Bounded(&profile.TotalTimeout, defaultTotalTimeout, maxTimeout) ||
		!providerutil.Bounded(&profile.PollInterval, defaultPollInterval, profile.TotalTimeout) ||
		!providerutil.Bounded(&profile.MaxPollAttempts, defaultMaxPollAttempts, maxPollAttempts) ||
		!providerutil.Bounded(&profile.MaxResponseBytes, defaultMaxResponseBytes, maxResponseBytes) ||
		!providerutil.Bounded(&profile.MaxDocumentBytes, defaultMaxDocumentBytes, maxDocumentBytes) {
		return nil, errors.New("datalab: execution bounds are invalid")
	}
	return &Client{
		executor: providerutil.Executor{
			Provider: provider, HTTP: providerhttp.IsolateClient(httpClient), Origin: origin,
			RequestTimeout: profile.RequestTimeout, MaxResponseBytes: profile.MaxResponseBytes,
			Credential: credential,
		},
		descriptor: descriptor, mode: profile.Mode, expectedVersionsFingerprint: expectedVersionsFingerprint,
		totalTimeout: profile.TotalTimeout, pollInterval: profile.PollInterval,
		maxPollAttempts: profile.MaxPollAttempts, maxDocumentBytes: profile.MaxDocumentBytes,
	}, nil
}

// Descriptor returns an immutable copy of the configured identity.
func (client *Client) Descriptor() document.RenditionDescriptor {
	if client == nil {
		return document.RenditionDescriptor{}
	}
	return providerutil.CloneDescriptor(client.descriptor)
}

// Render verifies sealed bytes before egress, submits them once, and polls only
// the fixed same-origin route derived from the returned request ID.
func (client *Client) Render(
	ctx context.Context, upload document.AuthorizedUpload, authorization document.RenditionAuthorization,
) (document.RenditionResult, error) {
	if client == nil {
		return document.RenditionResult{}, errors.New("datalab: client is required")
	}
	metadata := upload.Metadata()
	if metadata.ByteLength > client.maxDocumentBytes {
		return document.RenditionResult{}, provider.Classified(document.RenditionErrorPolicyRejected,
			"input exceeds the Datalab byte limit", nil)
	}
	operation, err := providerutil.NewOperation(ctx, provider, authorization.ExpiresAt, client.totalTimeout)
	if err != nil {
		return document.RenditionResult{}, err
	}
	defer operation.Cancel()
	source, err := operation.ReadUpload(upload)
	if err != nil {
		return document.RenditionResult{}, err
	}
	run := &rendering{
		operation: operation, metadata: metadata, authorization: authorization, source: source,
		versions:        versionState{expected: client.expectedVersionsFingerprint},
		includeMarkdown: client.descriptor.ReturnsMarkdown && authorization.MaxProviderMarkdownBytes > 0,
		started:         time.Now().UTC(),
	}
	requestID, err := client.submit(run)
	if err != nil {
		return document.RenditionResult{}, err
	}
	result, err := client.awaitResult(run, requestID)
	if err != nil {
		return document.RenditionResult{}, err
	}
	return client.buildResult(run, requestID, result)
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

func (client *Client) submit(run *rendering) (string, error) {
	filename := run.metadata.Filename
	if filename == "" {
		filename = "document"
		if extensions, _ := mime.ExtensionsByType(run.metadata.MediaType); len(extensions) != 0 {
			filename += extensions[0]
		}
	}
	if strings.ContainsAny(filename, "\r\n") {
		return "", provider.Classified(document.RenditionErrorPolicyRejected,
			"Datalab upload filename contains a newline", nil)
	}
	outputFormat := "json"
	if run.includeMarkdown {
		outputFormat = "markdown,json"
	}
	response, err := client.executor.Do(run.operation, &run.usage, providerutil.Request{
		Stage: providerutil.StageSubmission, Method: http.MethodPost, Path: convertPath,
		Upload: &providerutil.MultipartUpload{
			FieldName: "file", Filename: filename, MediaType: run.metadata.MediaType,
			Source: bytes.NewReader(run.source), Length: int64(len(run.source)),
			Fields: [][2]string{
				{"output_format", outputFormat}, {"mode", client.mode},
				{"paginate", "true"}, {"disable_image_extraction", "true"},
			},
		},
	})
	if err != nil {
		return "", err
	}
	if response.Status != http.StatusOK && response.Status != http.StatusAccepted {
		return "", provider.StatusError(providerutil.StageSubmission, response.Status, response.RetryAfter, nil)
	}
	initial, err := parseInitial(response.Body)
	if err != nil {
		return "", provider.AmbiguousSubmission(err)
	}
	if err := run.versions.observe(initial.versions); err != nil {
		return "", provider.AmbiguousSubmission(err)
	}
	return initial.id, nil
}

func (client *Client) awaitResult(run *rendering, requestID string) (finalResponse, error) {
	for range client.maxPollAttempts {
		result, err := client.poll(run, requestID)
		delay := client.pollInterval
		switch {
		case err != nil:
			if operationErr := run.operation.Check(); operationErr != nil {
				return finalResponse{}, provider.KnownJobError(operationErr)
			}
			if !document.IsRenditionProviderErrorRetryable(err) {
				return finalResponse{}, err
			}
			run.usage.Retries++
			delay = providerutil.RetryDelay(err, client.pollInterval)
		case result.status == "complete":
			return result, nil
		case result.status != "processing":
			return finalResponse{}, provider.Malformed("Datalab result status is unsupported", nil)
		}
		if err := run.operation.Wait(delay); err != nil {
			return finalResponse{}, provider.KnownJobError(err)
		}
	}
	return finalResponse{}, provider.AmbiguousJob(provider.Classified(
		document.RenditionErrorCapacity, "Datalab polling limit reached", nil))
}

func (client *Client) poll(run *rendering, requestID string) (finalResponse, error) {
	response, err := client.executor.Do(run.operation, &run.usage, providerutil.Request{
		Stage: providerutil.StageJob, Method: http.MethodGet, Path: convertPath + "/" + requestID,
		MaxResponseBytes: int64(run.authorization.MaxTotalResultBytes),
	})
	if err != nil {
		return finalResponse{}, err
	}
	if response.Status != http.StatusOK && response.Status != http.StatusAccepted {
		return finalResponse{}, provider.StatusError(providerutil.StageJob, response.Status, response.RetryAfter, nil)
	}
	result, err := parseFinal(response.Body, run.includeMarkdown)
	if err != nil {
		return finalResponse{}, err
	}
	if err := run.versions.observe(result.versions); err != nil {
		return finalResponse{}, err
	}
	return result, nil
}

func (client *Client) buildResult(
	run *rendering, requestID string, result finalResponse,
) (document.RenditionResult, error) {
	if result.success == nil || !*result.success {
		return document.RenditionResult{}, provider.Malformed("Datalab completed without a successful result", nil)
	}
	providerMarkdown := result.markdown
	if !run.includeMarkdown {
		providerMarkdown = nil
	}
	if providerutil.InjectsDocbankFrontmatter(providerMarkdown) {
		return document.RenditionResult{}, provider.Malformed(
			"Datalab provider Markdown attempts Docbank frontmatter injection", nil)
	}
	if len(providerMarkdown) > run.authorization.MaxProviderMarkdownBytes {
		return document.RenditionResult{}, provider.Malformed("Datalab Markdown exceeds authorization", nil)
	}
	evidence, structured, usable := mapEvidence(result.structured, run.authorization.MediaFamily, result.pageCount)
	if !usable {
		if len(providerMarkdown) == 0 {
			return document.RenditionResult{}, provider.Malformed("Datalab result has no usable bounded evidence", nil)
		}
		evidence = providerutil.DegradedEvidence(run.authorization.MediaFamily, string(providerMarkdown),
			"Datalab structured evidence is unavailable")
		structured = nil
	}
	artifacts := make([]document.RenditionArtifact, 0, 1)
	if len(structured) != 0 && providerutil.AllowsStructured(run.authorization) {
		if len(structured) > run.authorization.MaxArtifactBytes {
			return document.RenditionResult{}, provider.Malformed("Datalab structured output exceeds authorization", nil)
		}
		digest := providerutil.SHA256Hex(structured)
		artifacts = append(artifacts, document.RenditionArtifact{
			Role: document.EvidenceArtifactStructured, MediaType: "application/json",
			Payload: append([]byte(nil), structured...), SHA256: digest,
		})
		evidence.Artifacts = []document.SourceEvidenceArtifactV1{{
			ProviderID: "datalab-document", Pointer: "json", Role: document.EvidenceArtifactStructured, SHA256: digest,
		}}
	}
	receipt, err := providerutil.NewReceipt(provider, providerutil.Receipt{
		Descriptor: client.descriptor, Authorization: run.authorization, SourceSHA256: run.metadata.SHA256,
		OperationID: "datalab-" + requestID, StartedAt: run.started, CompletedAt: time.Now().UTC(),
		Usage: run.usage.Rendition(int64(len(run.source)), int64(len(evidence.Units))),
	})
	if err != nil {
		return document.RenditionResult{}, err
	}
	return document.RenditionResult{
		Evidence: evidence, ProviderMarkdown: append([]byte(nil), providerMarkdown...), Artifacts: artifacts,
		Receipt: receipt,
	}, nil
}

func parseInitial(body []byte) (initialResponse, error) {
	var wire struct {
		Success         *bool           `json:"success"`
		RequestID       string          `json:"request_id"`
		RequestCheckURL string          `json:"request_check_url"`
		Versions        json.RawMessage `json:"versions"`
	}
	if err := json.Unmarshal(body, &wire); err != nil {
		return initialResponse{}, provider.Malformed("Datalab submission JSON is invalid", err)
	}
	if wire.Success == nil || !*wire.Success {
		return initialResponse{}, provider.Malformed("Datalab rejected the conversion request", nil)
	}
	if err := providerutil.ValidateJobID(wire.RequestID); err != nil {
		return initialResponse{}, provider.Malformed("Datalab request ID is invalid", err)
	}
	if wire.RequestCheckURL == "" {
		return initialResponse{}, provider.Malformed("Datalab request check URL is missing", nil)
	}
	return initialResponse{success: true, id: wire.RequestID, versions: cloneRaw(wire.Versions)}, nil
}

func parseFinal(body []byte, requireMarkdown bool) (finalResponse, error) {
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
		return finalResponse{}, provider.Malformed("Datalab result JSON is invalid", err)
	}
	if wire.Status != "processing" && wire.Status != "complete" && wire.Status != "failed" {
		return finalResponse{}, provider.Malformed("Datalab result status is unsupported", nil)
	}
	if wire.Status == "failed" {
		return finalResponse{}, provider.Malformed("Datalab conversion failed", nil)
	}
	if wire.PageCount != nil && *wire.PageCount < 0 {
		return finalResponse{}, provider.Malformed("Datalab page count is invalid", nil)
	}
	if requireMarkdown && wire.OutputFormat != "" && !containsOutputFormat(wire.OutputFormat, "markdown") {
		return finalResponse{}, provider.Malformed("Datalab result omitted requested Markdown format", nil)
	}
	structured, err := decodeStructured(wire.JSON)
	if err != nil {
		return finalResponse{}, provider.Malformed("Datalab structured result is invalid", err)
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
	kind, locatorKind, natural := providerutil.NaturalUnit(family)
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
	hasMappedText := false
	hasUnmappedContent := false
	for index := range len(pages) {
		page, ok := pages[index]
		if !ok {
			return document.SourceEvidenceV1{}, nil, false
		}
		text, unmapped, ok := nodeText(page)
		if !ok {
			return document.SourceEvidenceV1{}, nil, false
		}
		hasUnmappedContent = hasUnmappedContent || unmapped
		if strings.TrimSpace(text) != "" {
			hasMappedText = true
		}
		evidence.Units = append(evidence.Units, document.SourceEvidenceUnitV1{
			Order: index, ProviderID: page.ID, Text: text,
			Locator: document.SourceEvidenceLocatorV1{
				Kind: locatorKind, IndexOrigin: document.EvidenceIndexOriginZero, Start: int64(index), End: int64(index),
			},
		})
	}
	if !hasMappedText {
		return document.SourceEvidenceV1{}, nil, false
	}
	if hasUnmappedContent {
		evidence.Completeness = document.EvidencePartial
		evidence.Omissions = []document.SourceEvidenceOmissionV1{{
			Kind: document.EvidenceOmissionField, Field: "structured_blocks",
			Reason: "Datalab structured content is not fully mapped",
		}}
	}
	return evidence, append([]byte(nil), raw...), true
}

type markerNode struct {
	ID        string       `json:"id"`
	BlockType string       `json:"block_type"`
	HTML      *string      `json:"html"`
	Children  []markerNode `json:"children"`
}

func nodeText(node markerNode) (string, bool, bool) {
	parts := make([]string, 0)
	unmapped := false
	var walk func(markerNode, bool) bool
	walk = func(current markerNode, page bool) bool {
		if page {
			if current.BlockType != "Page" {
				return false
			}
		} else if current.BlockType != "Text" {
			unmapped = true
		}
		if len(current.Children) != 0 {
			if !page && current.BlockType == "Text" {
				unmapped = true
			}
			for _, child := range current.Children {
				if !walk(child, false) {
					return false
				}
			}
			return true
		}
		if current.HTML == nil {
			return page
		}
		if page {
			unmapped = true
		}
		text, err := htmlText(*current.HTML)
		if err != nil {
			return false
		}
		if text != "" {
			parts = append(parts, text)
		}
		return true
	}
	if !walk(node, true) {
		return "", false, false
	}
	return strings.Join(parts, "\n\n"), unmapped, true
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

func (state *versionState) observe(raw json.RawMessage) error {
	fingerprint, err := versionsFingerprint(raw)
	if err != nil {
		return provider.Classified(document.RenditionErrorPolicyRejected, "Datalab provider versions are malformed", err)
	}
	if state.expected != "" && fingerprint != state.expected {
		return provider.Classified(document.RenditionErrorPolicyRejected, "Datalab provider version drift detected", nil)
	}
	if fingerprint == "" {
		return nil
	}
	if state.observed != "" && fingerprint != state.observed {
		return provider.Classified(document.RenditionErrorPolicyRejected,
			"Datalab provider version changed during conversion", nil)
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
	return providerutil.SHA256Hex(canonical), nil
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

func cloneRaw(value json.RawMessage) json.RawMessage { return append(json.RawMessage(nil), value...) }

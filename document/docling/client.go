// Package docling implements the fixed uploaded-file Docling Serve rendition flow.
package docling

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"mime"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/internal/providerutil"
	"go.kenn.io/docbank/document/providerhttp"
)

const (
	providerID = "docling.serve-v1"
	provider   = providerutil.Provider("Docling")

	convertPath = "/v1/convert/file/async"
	pollPath    = "/v1/status/poll/"
	resultPath  = "/v1/result/"

	defaultRequestTimeout   = 30 * time.Second
	defaultTotalTimeout     = 10 * time.Minute
	defaultPollInterval     = time.Second
	defaultMaxPollAttempts  = 300
	defaultMaxResponseBytes = int64(512 << 20)
	defaultMaxDocumentBytes = int64(64 << 20)
	maxTimeout              = 24 * time.Hour
	maxPollAttempts         = 10_000
	maxResponseBytes        = int64(512 << 20)
	maxDocumentBytes        = int64(1 << 30)
)

var _ document.RenditionProvider = (*Client)(nil)

// SecretResolver resolves the one profile-bound Docling API-key binding.
type SecretResolver = providerutil.SecretResolver

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
	executor         providerutil.Executor
	descriptor       document.RenditionDescriptor
	totalTimeout     time.Duration
	pollInterval     time.Duration
	maxPollAttempts  int
	maxDocumentBytes int64
}

// rendering carries the state of one Render call between its stages.
type rendering struct {
	operation       *providerutil.Operation
	usage           providerutil.Usage
	metadata        document.AuthorizedUploadMetadata
	authorization   document.RenditionAuthorization
	source          []byte
	includeMarkdown bool
	started         time.Time
}

// New validates a fixed profile and isolates the supplied HTTP client from
// ambient cookies and redirect behavior.
func New(profile Profile, secrets SecretResolver, httpClient *http.Client) (*Client, error) {
	origin, err := provider.ValidateOrigin(profile.Origin, profile.Descriptor.TrustBoundary)
	if err != nil {
		return nil, err
	}
	descriptor, err := provider.CanonicalDescriptor(profile.Descriptor)
	if err != nil {
		return nil, err
	}
	if descriptor.ID != providerID {
		return nil, errors.New("docling: descriptor ID must be docling.serve-v1")
	}
	credential := providerutil.APIKeyCredential("X-Api-Key", profile.SecretBinding, secrets)
	if err := credential.Validate(provider); err != nil {
		return nil, err
	}
	if httpClient == nil {
		return nil, errors.New("docling: HTTP client is required")
	}
	if !providerutil.Bounded(&profile.RequestTimeout, defaultRequestTimeout, maxTimeout) ||
		!providerutil.Bounded(&profile.TotalTimeout, defaultTotalTimeout, maxTimeout) ||
		!providerutil.Bounded(&profile.PollInterval, defaultPollInterval, profile.TotalTimeout) ||
		!providerutil.Bounded(&profile.MaxPollAttempts, defaultMaxPollAttempts, maxPollAttempts) ||
		!providerutil.Bounded(&profile.MaxResponseBytes, defaultMaxResponseBytes, maxResponseBytes) ||
		!providerutil.Bounded(&profile.MaxDocumentBytes, defaultMaxDocumentBytes, maxDocumentBytes) {
		return nil, errors.New("docling: execution bounds are invalid")
	}
	return &Client{
		executor: providerutil.Executor{
			Provider: provider, HTTP: providerhttp.IsolateClient(httpClient), Origin: origin,
			RequestTimeout: profile.RequestTimeout, MaxResponseBytes: profile.MaxResponseBytes,
			Credential: credential,
		},
		descriptor: descriptor, totalTimeout: profile.TotalTimeout, pollInterval: profile.PollInterval,
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
		return document.RenditionResult{}, provider.Classified(document.RenditionErrorPolicyRejected,
			"input exceeds the Docling byte limit", nil)
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
		includeMarkdown: client.descriptor.ReturnsMarkdown && authorization.MaxProviderMarkdownBytes > 0,
		started:         time.Now().UTC(),
	}
	task, err := client.submit(run)
	if err != nil {
		return document.RenditionResult{}, err
	}
	task, err = client.awaitTask(run, task)
	if err != nil {
		return document.RenditionResult{}, err
	}
	result, err := client.awaitResult(run, task.id)
	if err != nil {
		return document.RenditionResult{}, err
	}
	return client.buildResult(run, task, result)
}

type taskResponse struct{ id, status string }

type doclingResult struct {
	markdown []byte
	document json.RawMessage
	filename string
	status   string
	errors   []json.RawMessage
}

func (client *Client) submit(run *rendering) (taskResponse, error) {
	filename := run.metadata.Filename
	if filename == "" {
		filename = "document"
		if extensions, _ := mime.ExtensionsByType(run.metadata.MediaType); len(extensions) != 0 {
			filename += extensions[0]
		}
	}
	if strings.ContainsAny(filename, "\r\n") {
		return taskResponse{}, provider.Classified(document.RenditionErrorPolicyRejected,
			"Docling upload filename contains a newline", nil)
	}
	fields := [][2]string{{"to_formats", "json"}, {"target_type", "inbody"}}
	if run.includeMarkdown {
		fields = [][2]string{{"to_formats", "md"}, {"to_formats", "json"}, {"target_type", "inbody"}}
	}
	response, err := client.executor.Do(run.operation, &run.usage, providerutil.Request{
		Stage: providerutil.StageSubmission, Method: http.MethodPost, Path: convertPath,
		Upload: &providerutil.MultipartUpload{
			FieldName: "files", Filename: filename, MediaType: run.metadata.MediaType,
			Source: bytes.NewReader(run.source), Length: int64(len(run.source)), Fields: fields,
		},
	})
	if err != nil {
		return taskResponse{}, err
	}
	if response.Status != http.StatusOK && response.Status != http.StatusAccepted {
		return taskResponse{}, provider.StatusError(providerutil.StageSubmission, response.Status,
			response.RetryAfter, nil)
	}
	task, err := parseTask(response.Body)
	if err != nil {
		return taskResponse{}, provider.AmbiguousSubmission(err)
	}
	return task, nil
}

// awaitTask polls a known task until Docling reports a terminal status.
func (client *Client) awaitTask(run *rendering, task taskResponse) (taskResponse, error) {
	pollAttempts := 0
	delay := client.pollInterval
	for task.status != "success" && task.status != "partial_success" {
		if task.status != "pending" && task.status != "started" {
			return taskResponse{}, taskStatusError(task.status)
		}
		if pollAttempts >= client.maxPollAttempts {
			return taskResponse{}, provider.AmbiguousJob(provider.Classified(
				document.RenditionErrorCapacity, "Docling polling limit reached", nil))
		}
		if err := run.operation.Wait(delay); err != nil {
			return taskResponse{}, provider.KnownJobError(err)
		}
		delay = client.pollInterval
		nextTask, err := client.poll(run, task.id)
		pollAttempts++
		if err != nil {
			if operationErr := run.operation.Check(); operationErr != nil {
				return taskResponse{}, provider.KnownJobError(operationErr)
			}
			if !document.IsRenditionProviderErrorRetryable(err) {
				return taskResponse{}, err
			}
			if pollAttempts >= client.maxPollAttempts {
				return taskResponse{}, provider.AmbiguousJob(err)
			}
			run.usage.Retries++
			delay = providerutil.RetryDelay(err, client.pollInterval)
			continue
		}
		task = nextTask
	}
	return task, nil
}

// awaitResult fetches the completed result, retrying transient failures.
func (client *Client) awaitResult(run *rendering, taskID string) (doclingResult, error) {
	for attempt := 1; ; attempt++ {
		result, err := client.result(run, taskID)
		if err == nil {
			return result, nil
		}
		if operationErr := run.operation.Check(); operationErr != nil {
			return doclingResult{}, provider.KnownJobError(operationErr)
		}
		if !document.IsRenditionProviderErrorRetryable(err) {
			return doclingResult{}, err
		}
		if attempt >= client.maxPollAttempts {
			return doclingResult{}, provider.AmbiguousJob(err)
		}
		run.usage.Retries++
		if err := run.operation.Wait(providerutil.RetryDelay(err, client.pollInterval)); err != nil {
			return doclingResult{}, provider.KnownJobError(err)
		}
	}
}

func (client *Client) buildResult(
	run *rendering, task taskResponse, result doclingResult,
) (document.RenditionResult, error) {
	partialSuccess := task.status == "partial_success" || result.status == "partial_success"
	if !partialSuccess && len(result.errors) != 0 {
		return document.RenditionResult{}, provider.Malformed("Docling successful result contains errors", nil)
	}
	if run.authorization.DiscloseFilename && result.filename != run.metadata.Filename {
		return document.RenditionResult{}, provider.Classified(document.RenditionErrorPolicyRejected,
			"Docling result source identity does not match upload", nil)
	}
	providerMarkdown := result.markdown
	if !run.includeMarkdown {
		providerMarkdown = nil
	}
	if providerutil.InjectsDocbankFrontmatter(providerMarkdown) {
		return document.RenditionResult{}, provider.Malformed(
			"Docling provider Markdown attempts Docbank frontmatter injection", nil)
	}
	evidence, structured, usable := mapEvidence(result.document, run.authorization.MediaFamily)
	if !usable {
		if len(providerMarkdown) == 0 || len(providerMarkdown) > run.authorization.MaxProviderMarkdownBytes {
			return document.RenditionResult{}, provider.Malformed("Docling result has no usable bounded evidence", nil)
		}
		evidence = providerutil.DegradedEvidence(run.authorization.MediaFamily, string(providerMarkdown),
			"Docling structured evidence is unavailable")
		structured = nil
	}
	if partialSuccess {
		partialPages, err := parsePartialPages(result.errors)
		if err != nil {
			return document.RenditionResult{}, err
		}
		evidence, err = partialSuccessEvidence(evidence, run.authorization.MediaFamily, partialPages)
		if err != nil {
			return document.RenditionResult{}, err
		}
	}
	if len(providerMarkdown) > run.authorization.MaxProviderMarkdownBytes {
		return document.RenditionResult{}, provider.Malformed("Docling Markdown exceeds authorization", nil)
	}
	artifacts := make([]document.RenditionArtifact, 0, 1)
	if len(structured) != 0 && providerutil.AllowsStructured(run.authorization) {
		if len(structured) > run.authorization.MaxArtifactBytes {
			return document.RenditionResult{}, provider.Malformed("Docling structured output exceeds authorization", nil)
		}
		digest := providerutil.SHA256Hex(structured)
		artifacts = append(artifacts, document.RenditionArtifact{
			Role: document.EvidenceArtifactStructured, MediaType: "application/json", Payload: structured, SHA256: digest,
		})
		evidence.Artifacts = []document.SourceEvidenceArtifactV1{{
			ProviderID: "docling-document", Pointer: "document", Role: document.EvidenceArtifactStructured, SHA256: digest,
		}}
	}
	receipt, err := providerutil.NewReceipt(provider, providerutil.Receipt{
		Descriptor: client.descriptor, Authorization: run.authorization, SourceSHA256: run.metadata.SHA256,
		OperationID: "docling-" + task.id, StartedAt: run.started, CompletedAt: time.Now().UTC(),
		Warnings: partialSuccessWarnings(partialSuccess),
		Usage:    run.usage.Rendition(int64(len(run.source)), int64(len(evidence.Units))),
	})
	if err != nil {
		return document.RenditionResult{}, err
	}
	return document.RenditionResult{
		Evidence: evidence, ProviderMarkdown: append([]byte(nil), providerMarkdown...), Artifacts: artifacts,
		Receipt: receipt,
	}, nil
}

func (client *Client) poll(run *rendering, taskID string) (taskResponse, error) {
	response, err := client.executor.Do(run.operation, &run.usage, providerutil.Request{
		Stage: providerutil.StageJob, Method: http.MethodGet, Path: pollPath + taskID,
	})
	if err != nil {
		return taskResponse{}, err
	}
	if response.Status != http.StatusOK && response.Status != http.StatusAccepted {
		return taskResponse{}, provider.StatusError(providerutil.StageJob, response.Status, response.RetryAfter, nil)
	}
	task, err := parseTask(response.Body)
	if err != nil {
		return taskResponse{}, err
	}
	if task.id != taskID {
		return taskResponse{}, provider.Malformed("Docling task identity changed while polling", nil)
	}
	return task, nil
}

func (client *Client) result(run *rendering, taskID string) (doclingResult, error) {
	response, err := client.executor.Do(run.operation, &run.usage, providerutil.Request{
		Stage: providerutil.StageJob, Method: http.MethodGet, Path: resultPath + taskID,
		MaxResponseBytes: int64(run.authorization.MaxTotalResultBytes),
	})
	if err != nil {
		return doclingResult{}, err
	}
	if response.Status != http.StatusOK {
		return doclingResult{}, provider.StatusError(providerutil.StageJob, response.Status, response.RetryAfter, nil)
	}
	return parseResult(response.Body)
}

func parseResult(body []byte) (doclingResult, error) {
	var wire struct {
		Status   string            `json:"status"`
		Document json.RawMessage   `json:"document"`
		Errors   []json.RawMessage `json:"errors"`
	}
	if err := json.Unmarshal(body, &wire); err != nil {
		return doclingResult{}, provider.Malformed("Docling result JSON is invalid", err)
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
		return doclingResult{}, provider.Classified(document.RenditionErrorPolicyRejected,
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
			return doclingResult{}, provider.Classified(document.RenditionErrorPolicyRejected,
				"Docling result source identity is inconsistent", nil)
		}
	}
	return doclingResult{markdown: []byte(documentWire.Markdown),
		document: append([]byte(nil), documentWire.JSONContent...), filename: documentWire.Filename,
		status: wire.Status, errors: append([]json.RawMessage(nil), wire.Errors...)}, nil
}

func parseTask(body []byte) (taskResponse, error) {
	var wire struct {
		ID     string `json:"task_id"`
		Type   string `json:"task_type"`
		Status string `json:"task_status"`
	}
	if err := json.Unmarshal(body, &wire); err != nil {
		return taskResponse{}, provider.Malformed("Docling task response JSON is invalid", err)
	}
	if err := providerutil.ValidateJobID(wire.ID); err != nil {
		return taskResponse{}, provider.Malformed("Docling task ID is invalid", err)
	}
	if wire.Type != "convert" {
		return taskResponse{}, provider.Malformed("Docling task type is invalid", nil)
	}
	if !officialTaskStatus(wire.Status) {
		return taskResponse{}, provider.Malformed("Docling task status is invalid", nil)
	}
	return taskResponse{id: wire.ID, status: wire.Status}, nil
}

func mapEvidence(raw json.RawMessage, family string) (document.SourceEvidenceV1, []byte, bool) {
	var topLevel map[string]json.RawMessage
	if json.Unmarshal(raw, &topLevel) != nil {
		return document.SourceEvidenceV1{}, nil, false
	}
	texts, ok := topLevel["texts"]
	if !ok || bytes.Equal(bytes.TrimSpace(texts), []byte("null")) {
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
		SchemaName    string                     `json:"schema_name"`
		Version       string                     `json:"version"`
		Texts         []doclingText              `json:"texts"`
		Pages         map[string]json.RawMessage `json:"pages"`
		Tables        []json.RawMessage          `json:"tables"`
		Pictures      []json.RawMessage          `json:"pictures"`
		KeyValueItems []json.RawMessage          `json:"key_value_items"`
		FormItems     []json.RawMessage          `json:"form_items"`
		FieldRegions  []json.RawMessage          `json:"field_regions"`
		FieldItems    []json.RawMessage          `json:"field_items"`
	}
	if len(raw) == 0 || json.Unmarshal(raw, &wire) != nil || wire.SchemaName != "DoclingDocument" ||
		!supportedDoclingMajor(wire.Version) || len(wire.Pages) == 0 || family != "pdf" {
		return document.SourceEvidenceV1{}, nil, false
	}
	kind, locatorKind, natural := providerutil.NaturalUnit(family)
	if !natural {
		return document.SourceEvidenceV1{}, nil, false
	}
	indexes, pages, ok := doclingPageTexts(wire.Pages, wire.Texts)
	if !ok {
		return document.SourceEvidenceV1{}, nil, false
	}
	evidence := document.SourceEvidenceV1{
		ContractVersion: document.SourceEvidenceContractV1, Completeness: document.EvidenceComplete,
		Family: family, UnitKind: kind, Units: make([]document.SourceEvidenceUnitV1, 0, len(indexes)),
	}
	for order, page := range indexes {
		evidence.Units = append(evidence.Units, document.SourceEvidenceUnitV1{
			Order: order, Text: strings.Join(pages[page], "\n\n"),
			Locator: document.SourceEvidenceLocatorV1{Kind: locatorKind, IndexOrigin: document.EvidenceIndexOriginOne, Start: page, End: page},
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

type doclingText struct {
	Text string `json:"text"`
	Prov []struct {
		PageNo int64 `json:"page_no"`
	} `json:"prov"`
}

// doclingPageTexts builds the contiguous one-based page registry and assigns
// every non-blank text to the single page its provenance names.
func doclingPageTexts(
	rawPages map[string]json.RawMessage, texts []doclingText,
) ([]int64, map[int64][]string, bool) {
	pages := make(map[int64][]string, len(rawPages))
	for key := range rawPages {
		page, err := strconv.ParseInt(key, 10, 64)
		if err != nil || page < 1 || strconv.FormatInt(page, 10) != key {
			return nil, nil, false
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
			return nil, nil, false
		}
	}
	hasMappedText := false
	for _, text := range texts {
		if strings.TrimSpace(text.Text) == "" {
			continue
		}
		if len(text.Prov) == 0 {
			return nil, nil, false
		}
		page := text.Prov[0].PageNo
		if page < 1 {
			return nil, nil, false
		}
		for _, provenance := range text.Prov {
			if provenance.PageNo != page {
				return nil, nil, false
			}
		}
		if _, ok := pages[page]; !ok {
			return nil, nil, false
		}
		pages[page] = append(pages[page], text.Text)
		hasMappedText = true
	}
	return indexes, pages, hasMappedText
}

func supportedDoclingMajor(version string) bool {
	major, _, ok := strings.Cut(version, ".")
	return ok && major == "1"
}

func taskStatusError(status string) error {
	switch status {
	case "failure":
		return provider.Malformed("Docling task failed", nil)
	case "skipped":
		return provider.Classified(document.RenditionErrorPolicyRejected, "Docling task was skipped", nil)
	default:
		return provider.Malformed("Docling task status is unsupported", nil)
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
		return nil, provider.Malformed("Docling partial result has no page omissions", nil)
	}
	pages := make([]int64, 0, len(raw))
	seen := make(map[int64]struct{}, len(raw))
	for _, value := range raw {
		var omission struct {
			PageNo *int64 `json:"page_no"`
		}
		if json.Unmarshal(value, &omission) != nil || omission.PageNo == nil || *omission.PageNo < 1 {
			return nil, provider.Malformed("Docling partial result has an unlocated omission", nil)
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
		return document.SourceEvidenceV1{}, provider.Malformed("Docling partial result lacks exact PDF page evidence", nil)
	}
	unitByPage := make(map[int64]int, len(evidence.Units))
	for index := range evidence.Units {
		unitByPage[evidence.Units[index].Locator.Start] = index
	}
	for _, page := range pages {
		index, ok := unitByPage[page]
		if !ok {
			return document.SourceEvidenceV1{}, provider.Malformed("Docling partial result names an unknown page", nil)
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

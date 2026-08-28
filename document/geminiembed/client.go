package geminiembed

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/media"
)

const (
	maxSecretBytes = 64 << 10
	maxUsageValue  = int64(1 << 50)
	fileClockSkew  = 5 * time.Minute
)

var _ document.EmbeddingProvider = (*Client)(nil)

type wireRequest struct {
	Model                string      `json:"model"`
	Content              wireContent `json:"content"`
	OutputDimensionality int         `json:"outputDimensionality"`
}

type wireContent struct {
	Parts []wirePart `json:"parts"`
}

type wirePart struct {
	Text       string          `json:"text,omitzero"`
	InlineData *wireInlineData `json:"inlineData,omitempty"`
	FileData   *wireFileData   `json:"fileData,omitempty"`
}

type wireInlineData struct {
	MIMEType string `json:"mimeType"`
	Data     string `json:"data"`
}

type wireFileData struct {
	MIMEType string `json:"mimeType"`
	FileURI  string `json:"fileUri"`
}

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

type wireResponse struct {
	Embedding     wireEmbedding `json:"embedding"`
	UsageMetadata *wireUsage    `json:"usageMetadata,omitempty"`
}

type wireEmbedding struct {
	Values []float32 `json:"values"`
}

type wireUsage struct {
	PromptTokenCount        *int64 `json:"promptTokenCount,omitempty"`
	CachedContentTokenCount *int64 `json:"cachedContentTokenCount,omitempty"`
	CandidatesTokenCount    *int64 `json:"candidatesTokenCount,omitempty"`
	ToolUsePromptTokenCount *int64 `json:"toolUsePromptTokenCount,omitempty"`
	ThoughtsTokenCount      *int64 `json:"thoughtsTokenCount,omitempty"`
	TotalTokenCount         *int64 `json:"totalTokenCount,omitempty"`
}

// Receipt is bounded provider provenance and numeric usage without request,
// response, vector, source, or credential material.
type Receipt struct {
	ProviderID                 string
	DescriptorFingerprint      string
	PolicyFingerprint          string
	Model                      string
	ModelRevision              string
	Transport                  Transport
	RequestCount               int
	PromptTokens               int64
	CachedContentTokens        int64
	CandidateTokens            int64
	ToolUsePromptTokens        int64
	ThoughtTokens              int64
	TotalTokens                int64
	ProviderResponseIDs        []string
	OmittedProviderResponseIDs int
	Warnings                   []string
	ProviderRetentionCeiling   time.Duration
}

type Execution struct {
	Result  document.EmbeddingResult
	Receipt Receipt
}

func (client *Client) Embed(ctx context.Context, inputs []document.EmbeddingInput, authorization document.EmbeddingAuthorization) (document.EmbeddingResult, error) {
	return client.embed(ctx, inputs, authorization, nil)
}

func (client *Client) EmbedWithReceipt(ctx context.Context, inputs []document.EmbeddingInput, authorization document.EmbeddingAuthorization) (Execution, error) {
	if client == nil {
		return Execution{}, errors.New("gemini embed: client is required")
	}
	receipt := Receipt{ProviderID: ProviderID, DescriptorFingerprint: client.descriptor.Fingerprint,
		PolicyFingerprint: client.descriptor.PolicyFingerprint, Model: Model, ModelRevision: client.descriptor.ModelRevision,
		Transport: client.profile.Transport, ProviderRetentionCeiling: profileRetention(client.profile.Transport)}
	result, err := client.embed(ctx, inputs, authorization, &receipt)
	if err != nil {
		return Execution{}, err
	}
	return Execution{Result: result, Receipt: receipt}, nil
}

func (client *Client) embed(ctx context.Context, inputs []document.EmbeddingInput, authorization document.EmbeddingAuthorization, receipt *Receipt) (document.EmbeddingResult, error) {
	if client == nil || ctx == nil {
		return document.EmbeddingResult{}, errors.New("gemini embed: client and context are required")
	}
	requestCtx, cancel := context.WithTimeout(ctx, client.profile.RequestTimeout)
	defer cancel()
	frozenInputs, enrolled, enrollmentErr := enrollOriginalUploads(inputs)
	defer closeEnrolledUploads(enrolled)
	if enrollmentErr != nil {
		return document.EmbeddingResult{}, enrollmentErr
	}
	if err := requestCtx.Err(); err != nil {
		return document.EmbeddingResult{}, fmt.Errorf("gemini embed: embedding canceled: %w", err)
	}
	if err := document.ValidateEmbeddingProviderRequest(client, frozenInputs, authorization); err != nil {
		return document.EmbeddingResult{}, err
	}
	if authorization.MaxInputBytes > client.profile.MaxInputBytes || authorization.MaxResponseBytes > client.profile.MaxResponseBytes {
		return document.EmbeddingResult{}, errors.New("gemini embed: embedding authorization exceeds profile capacity")
	}

	sourceGate := newActiveSourceGate()
	closeFinished := make(chan struct{})
	stopClose := context.AfterFunc(requestCtx, func() {
		sourceGate.Cancel()
		close(closeFinished)
	})
	defer func() {
		if !stopClose() {
			<-closeFinished
		}
	}()

	prepared := make([]preparedInput, len(frozenInputs))
	defer clearPreparedInputs(prepared)
	var inputBytes int64
	for index, input := range frozenInputs {
		switch {
		case input.Role == document.EmbeddingRoleDocument && input.Kind == document.EmbeddingInputRenditionChunk:
			text := client.descriptor.ModelInput.EncodeDocument(input.Text)
			if int64(len(text)) > client.profile.MaxInputBytes-inputBytes {
				return document.EmbeddingResult{}, errors.New("gemini embed: embedding input exceeds profile byte capacity")
			}
			inputBytes += int64(len(text))
			payload, err := client.marshalRequest(wirePart{Text: text})
			if err != nil {
				return document.EmbeddingResult{}, err
			}
			prepared[index].payload = payload
		case input.Role == document.EmbeddingRoleQuery && input.Kind == document.EmbeddingInputQueryText:
			text := client.descriptor.ModelInput.EncodeQuery(input.Text)
			if int64(len(text)) > client.profile.MaxInputBytes-inputBytes {
				return document.EmbeddingResult{}, errors.New("gemini embed: embedding input exceeds profile byte capacity")
			}
			inputBytes += int64(len(text))
			payload, err := client.marshalRequest(wirePart{Text: text})
			if err != nil {
				return document.EmbeddingResult{}, err
			}
			prepared[index].payload = payload
		case input.Role == document.EmbeddingRoleDocument && input.Kind == document.EmbeddingInputOriginalFile:
			frozen, ok := input.Source.(*frozenUpload)
			if !ok {
				return document.EmbeddingResult{}, errors.New("gemini embed: original upload was not frozen")
			}
			if err := client.validateDirectCapability(frozen.enrolled, authorization); err != nil {
				return document.EmbeddingResult{}, err
			}
			if client.profile.Transport == TransportFilesAPI {
				if err := client.preflightFileDataRequest(frozen.enrolled.metadata.MediaType); err != nil {
					return document.EmbeddingResult{}, err
				}
			}
			verified, err := client.readDirectFile(requestCtx, frozen.enrolled, sourceGate)
			if err != nil {
				return document.EmbeddingResult{}, err
			}
			prepared[index].file = &verified
			if client.profile.Transport == TransportInline {
				encoded := base64.StdEncoding.EncodeToString(verified.data)
				payload, encodeErr := client.marshalRequest(wirePart{InlineData: &wireInlineData{
					MIMEType: verified.metadata.MediaType, Data: encoded,
				}})
				if encodeErr != nil {
					return document.EmbeddingResult{}, encodeErr
				}
				limit := int64(100 << 20)
				if verified.metadata.MediaType == "application/pdf" {
					limit = 50 << 20
				}
				if int64(len(payload)) > limit {
					clear(payload)
					return document.EmbeddingResult{}, errors.New("gemini embed: inline request exceeds provider byte capacity")
				}
				prepared[index].payload = payload
			}
		default:
			return document.EmbeddingResult{}, errors.New("gemini embed: unsupported input role or kind")
		}
	}
	secret, err := client.secrets.ResolveSecret(requestCtx, client.profile.SecretBinding)
	if err != nil || !validSecret(secret) {
		if contextErr := requestCtx.Err(); contextErr != nil {
			return document.EmbeddingResult{}, fmt.Errorf("gemini embed: credential resolution canceled: %w", contextErr)
		}
		return document.EmbeddingResult{}, errors.New("gemini embed: API-key resolution failed")
	}
	result := document.EmbeddingResult{Vectors: make([]document.EmbeddingVector, len(frozenInputs))}
	for index := range prepared {
		var vector []float32
		if prepared[index].file != nil && client.profile.Transport == TransportFilesAPI {
			vector, err = client.executeFile(requestCtx, *prepared[index].file, secret, receipt)
		} else {
			vector, err = client.execute(requestCtx, prepared[index].payload, secret, receipt)
		}
		if err != nil {
			return document.EmbeddingResult{}, err
		}
		result.Vectors[index] = document.EmbeddingVector{Key: frozenInputs[index].Key, Values: vector}
	}
	if err := document.ValidateEmbeddingProviderResult(client.descriptor, frozenInputs, authorization, result); err != nil {
		return document.EmbeddingResult{}, errors.New("gemini embed: provider response violates embedding contract")
	}
	return result, nil
}

type preparedInput struct {
	payload []byte
	file    *verifiedFile
}

func clearPreparedInputs(prepared []preparedInput) {
	for index := range prepared {
		clear(prepared[index].payload)
		if prepared[index].file != nil {
			clear(prepared[index].file.data)
		}
	}
}

type verifiedFile struct {
	data       []byte
	metadata   document.AuthorizedUploadMetadata
	capability media.CapabilityRecord
}

func (client *Client) marshalRequest(part wirePart) ([]byte, error) {
	payload, err := json.Marshal(wireRequest{Model: "models/" + Model,
		Content: wireContent{Parts: []wirePart{part}}, OutputDimensionality: client.descriptor.Dimension})
	if err != nil {
		return nil, errors.New("gemini embed: request encoding failed")
	}
	if int64(len(payload)) > client.profile.MaxRequestBytes {
		clear(payload)
		return nil, errors.New("gemini embed: embedding request exceeds profile byte capacity")
	}
	return payload, nil
}

func (client *Client) preflightFileDataRequest(mediaType string) error {
	payload, err := client.marshalRequest(wirePart{FileData: &wireFileData{
		MIMEType: mediaType, FileURI: maximumProviderFileURI(),
	}})
	clear(payload)
	if err != nil {
		return errors.New("gemini embed: fileData request exceeds profile byte capacity")
	}
	return nil
}

type capabilityCarrier interface {
	CapabilityRecord() media.CapabilityRecord
}

type enrolledUpload struct {
	source        document.AuthorizedUpload
	metadata      document.AuthorizedUploadMetadata
	capability    media.CapabilityRecord
	hasCapability bool

	closeOnce sync.Once
	closeErr  error
}

func (upload *enrolledUpload) Close() error {
	upload.closeOnce.Do(func() { upload.closeErr = upload.source.Close() })
	return upload.closeErr
}

func (upload *enrolledUpload) liveMetadata() document.AuthorizedUploadMetadata {
	return upload.source.Metadata()
}

func (upload *enrolledUpload) liveCapability() (media.CapabilityRecord, bool) {
	carrier, ok := upload.source.(capabilityCarrier)
	if !ok {
		return media.CapabilityRecord{}, false
	}
	return carrier.CapabilityRecord(), true
}

type frozenUpload struct {
	enrolled *enrolledUpload
}

func (upload *frozenUpload) Read(value []byte) (int, error) {
	return upload.enrolled.source.Read(value)
}
func (upload *frozenUpload) Close() error { return upload.enrolled.Close() }
func (upload *frozenUpload) Metadata() document.AuthorizedUploadMetadata {
	return upload.enrolled.metadata
}
func (upload *frozenUpload) CapabilityRecord() media.CapabilityRecord {
	return upload.enrolled.capability
}

func enrollOriginalUploads(inputs []document.EmbeddingInput) ([]document.EmbeddingInput, []*enrolledUpload, error) {
	frozen := slices.Clone(inputs)
	enrolled := make([]*enrolledUpload, 0, len(inputs))
	owners := make([]*enrolledUpload, len(inputs))
	seen := make(map[document.AuthorizedUpload]*enrolledUpload, len(inputs))
	var enrollmentErr error
	for index := range frozen {
		frozen[index].HeadingPath = slices.Clone(frozen[index].HeadingPath)
		frozen[index].SourceSpans = slices.Clone(frozen[index].SourceSpans)
		source := frozen[index].Source
		if source == nil {
			if frozen[index].Kind == document.EmbeddingInputOriginalFile && enrollmentErr == nil {
				enrollmentErr = errors.New("gemini embed: original upload source is nil")
			}
			continue
		}
		if nilInterface(source) {
			if enrollmentErr == nil {
				if frozen[index].Kind == document.EmbeddingInputOriginalFile {
					enrollmentErr = errors.New("gemini embed: original upload source is nil")
				} else {
					enrollmentErr = errors.New("gemini embed: attached upload source is nil")
				}
			}
			continue
		}
		identity := reflect.ValueOf(source)
		if !identity.Comparable() || (identity.Kind() == reflect.Pointer && identity.Type().Elem().Size() == 0) {
			upload := &enrolledUpload{source: source}
			enrolled = append(enrolled, upload)
			owners[index] = upload
			if enrollmentErr == nil {
				enrollmentErr = errors.New("gemini embed: original upload identity is not safely comparable")
			}
			continue
		}
		if upload, duplicate := seen[source]; duplicate {
			owners[index] = upload
			if enrollmentErr == nil {
				enrollmentErr = errors.New("gemini embed: original upload source is repeated")
			}
			continue
		}
		upload := &enrolledUpload{source: source}
		enrolled = append(enrolled, upload)
		owners[index] = upload
		seen[source] = upload
		if (frozen[index].Role != document.EmbeddingRoleDocument ||
			frozen[index].Kind != document.EmbeddingInputOriginalFile) && enrollmentErr == nil {
			enrollmentErr = errors.New("gemini embed: upload source is attached to an unsupported input role or kind")
		}
	}
	if enrollmentErr != nil {
		return frozen, enrolled, enrollmentErr
	}
	for _, upload := range enrolled {
		upload.metadata = upload.source.Metadata()
		if carrier, ok := upload.source.(capabilityCarrier); ok {
			upload.capability = carrier.CapabilityRecord()
			upload.hasCapability = true
		}
	}
	for index, upload := range owners {
		if upload != nil {
			frozen[index].Source = &frozenUpload{enrolled: upload}
		}
	}
	return frozen, enrolled, nil
}

func closeEnrolledUploads(uploads []*enrolledUpload) {
	for _, upload := range uploads {
		_ = upload.Close()
	}
}

type activeSourceGate struct {
	mu          sync.Mutex
	canceled    bool
	nextToken   uint64
	activeToken uint64
	active      *enrolledUpload
}

func newActiveSourceGate() *activeSourceGate { return new(activeSourceGate) }

func (gate *activeSourceGate) Begin(source *enrolledUpload) (uint64, bool) {
	gate.mu.Lock()
	defer gate.mu.Unlock()
	if gate.canceled {
		return 0, false
	}
	gate.nextToken++
	gate.activeToken = gate.nextToken
	gate.active = source
	return gate.activeToken, true
}

func (gate *activeSourceGate) End(token uint64) {
	gate.mu.Lock()
	defer gate.mu.Unlock()
	if gate.activeToken != token {
		return
	}
	gate.active = nil
	gate.activeToken = 0
}

func (gate *activeSourceGate) Cancel() {
	gate.mu.Lock()
	if gate.canceled {
		gate.mu.Unlock()
		return
	}
	gate.canceled = true
	active := gate.active
	gate.active = nil
	gate.activeToken = 0
	gate.mu.Unlock()
	if active != nil {
		_ = active.Close()
	}
}

func (client *Client) validateDirectCapability(upload *enrolledUpload, authorization document.EmbeddingAuthorization) error {
	if !upload.hasCapability {
		return errors.New("gemini embed: direct-file upload lacks local capability authority")
	}
	record := upload.capability
	if err := media.ValidateCapabilityRecord(record); err != nil {
		return errors.New("gemini embed: direct-file capability record is invalid")
	}
	policy, local := record.InspectionPolicy()
	if !local || !record.Eligible || record.Reason != media.CapabilityReasonEligible {
		return errors.New("gemini embed: direct-file capability record is not locally eligible")
	}
	metadata := upload.metadata
	if metadata.Filename != policy.Filename || metadata.MediaFamily != record.MediaFamily ||
		metadata.MediaType != record.MediaType || metadata.ByteLength != record.SourceBytes ||
		metadata.SHA256 != record.SourceSHA256 || metadata.CapabilityRecordChecksum != record.Checksum ||
		metadata.InputKind != document.RenditionInputOriginalFile ||
		record.DescriptorFingerprint != client.descriptor.Fingerprint ||
		record.ProfileFingerprint != client.profile.CapabilityProfileFingerprint ||
		record.DisclosureFingerprint != client.profile.DisclosureFingerprint ||
		record.InputKind != document.RenditionInputOriginalFile ||
		policy.DescriptorFingerprint != client.descriptor.Fingerprint ||
		policy.ProfileFingerprint != client.profile.CapabilityProfileFingerprint ||
		policy.DisclosureFingerprint != client.profile.DisclosureFingerprint ||
		policy.InputKind != document.RenditionInputOriginalFile {
		return errors.New("gemini embed: direct-file capability authority does not match upload and profile")
	}
	if record.SourceBytes > authorization.MaxInputBytes || record.SourceBytes > client.profile.MaxInputBytes ||
		policy.MaxSourceBytes > client.profile.MaxInputBytes {
		return errors.New("gemini embed: direct-file source exceeds byte capacity")
	}
	if client.profile.Transport == TransportFilesAPI && record.SourceBytes > client.profile.MaxRequestBytes {
		return errors.New("gemini embed: raw file upload exceeds request byte capacity")
	}
	if !geminiCapabilitySupported(record, policy) {
		return errors.New("gemini embed: direct-file capability is unsupported or over limit")
	}
	return nil
}

func geminiCapabilitySupported(record media.CapabilityRecord, policy media.InspectionPolicy) bool {
	switch record.MediaFamily {
	case "image":
		return (record.MediaType == "image/png" || record.MediaType == "image/jpeg") &&
			record.Measurements.Pixels > 0 && record.Measurements.Frames > 0 &&
			policy.MaxPixels > 0 && policy.MaxFrames > 0
	case "audio":
		return (record.MediaType == "audio/wav" || record.MediaType == "audio/mpeg") &&
			record.Measurements.DurationMS > 0 && record.Measurements.DurationMS <= 180_000 &&
			policy.MaxDurationMS > 0 && policy.MaxDurationMS <= 180_000
	case "video":
		return (record.MediaType == "video/mp4" || record.MediaType == "video/quicktime") &&
			record.Measurements.DurationMS > 0 && record.Measurements.DurationMS <= 120_000 &&
			record.Measurements.Frames > 0 && record.Measurements.Frames <= 32 &&
			policy.MaxDurationMS > 0 && policy.MaxDurationMS <= 120_000 &&
			policy.MaxFrames > 0 && policy.MaxFrames <= 32
	case "pdf":
		return record.MediaType == "application/pdf" && record.Measurements.Pages > 0 &&
			record.Measurements.Pages <= 6 && policy.MaxPages > 0 && policy.MaxPages <= 6
	default:
		return false
	}
}

func (client *Client) readDirectFile(ctx context.Context, upload *enrolledUpload, gate *activeSourceGate) (verifiedFile, error) {
	if live := upload.liveMetadata(); live != upload.metadata {
		return verifiedFile{}, errors.New("gemini embed: direct-file upload metadata changed")
	}
	if live, ok := upload.liveCapability(); !ok || live != upload.capability {
		return verifiedFile{}, errors.New("gemini embed: direct-file capability authority changed")
	}
	token, ok := gate.Begin(upload)
	if !ok {
		if contextErr := ctx.Err(); contextErr != nil {
			return verifiedFile{}, contextErr
		}
		return verifiedFile{}, errors.New("gemini embed: direct-file source transfer stopped")
	}
	data, readErr := io.ReadAll(io.LimitReader(upload.source, upload.metadata.ByteLength+1))
	gate.End(token)
	metadataChanged := upload.liveMetadata() != upload.metadata
	capability, capabilityOK := upload.liveCapability()
	capabilityChanged := !capabilityOK || capability != upload.capability
	closeErr := upload.Close()
	if contextErr := ctx.Err(); contextErr != nil {
		clear(data)
		return verifiedFile{}, contextErr
	}
	if readErr != nil || closeErr != nil || metadataChanged || capabilityChanged || int64(len(data)) != upload.metadata.ByteLength {
		clear(data)
		return verifiedFile{}, errors.New("gemini embed: direct-file source changed or could not be read exactly")
	}
	digest := sha256.Sum256(data)
	if hex.EncodeToString(digest[:]) != upload.metadata.SHA256 {
		clear(data)
		return verifiedFile{}, errors.New("gemini embed: direct-file source checksum changed")
	}
	return verifiedFile{data: data, metadata: upload.metadata, capability: upload.capability}, nil
}

func (client *Client) executeFile(ctx context.Context, file verifiedFile, secret string, receipt *Receipt) (vector []float32, retErr error) {
	uploadURL, err := client.startFileUpload(ctx, file, secret, receipt)
	if err != nil {
		return nil, err
	}
	created, responseID, err := client.finalizeFileUpload(ctx, uploadURL, file, secret, receipt)
	if err != nil {
		return nil, err
	}
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), client.profile.CleanupTimeout)
		defer cancel()
		if deleteErr := client.deleteFile(cleanupCtx, created.file.Name, secret, receipt); deleteErr != nil && receipt != nil {
			addCleanupWarning(receipt)
		}
	}()
	if !recordReceiptResponse(receipt, nil, responseID) {
		return nil, &ProviderError{Kind: ErrPermanentResponse}
	}
	active, err := client.waitForActiveFile(ctx, created, file, secret, receipt)
	if err != nil {
		return nil, err
	}
	payload, err := client.marshalRequest(wirePart{FileData: &wireFileData{
		MIMEType: file.metadata.MediaType, FileURI: active.file.URI,
	}})
	if err != nil {
		return nil, err
	}
	defer clear(payload)
	return client.execute(ctx, payload, secret, receipt)
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
	if err != nil {
		return nil, &ProviderError{Kind: ErrPermanentResponse}
	}
	if !recordReceiptResponse(receipt, nil, response.Header.Get("X-Goog-Request-Id")) {
		return nil, &ProviderError{Kind: ErrPermanentResponse}
	}
	return uploadURL, nil
}

func (client *Client) finalizeFileUpload(ctx context.Context, uploadURL *url.URL, file verifiedFile, secret string, receipt *Receipt) (validatedProviderFile, string, error) {
	if int64(len(file.data)) > client.profile.MaxRequestBytes {
		return validatedProviderFile{}, "", errors.New("gemini embed: raw file upload exceeds request byte capacity")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, uploadURL.String(), bytes.NewReader(file.data))
	if err != nil {
		return validatedProviderFile{}, "", errors.New("gemini embed: file upload request construction failed")
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("X-Goog-Api-Key", secret)
	request.Header.Set("X-Goog-Upload-Offset", "0")
	request.Header.Set("X-Goog-Upload-Command", "upload, finalize")
	if !beginReceiptRequest(receipt) {
		return validatedProviderFile{}, "", &ProviderError{Kind: ErrPermanentResponse}
	}
	startedAt := time.Now().UTC()
	response, err := client.http.Do(request)
	completedAt := time.Now().UTC()
	if err != nil {
		return validatedProviderFile{}, "", requestFailure(ctx, "file upload", err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return validatedProviderFile{}, "", statusError(response.StatusCode, response.Header.Get("Retry-After"), time.Now().UTC())
	}
	if response.Header.Get("X-Goog-Upload-Status") != "final" || !isJSONContentType(response.Header.Get("Content-Type")) {
		return validatedProviderFile{}, "", &ProviderError{Kind: ErrPermanentResponse}
	}
	body, err := readBounded(ctx, response.Body, client.profile.MaxResponseBytes)
	if err != nil {
		return validatedProviderFile{}, "", err
	}
	defer clear(body)
	var decoded wireCreateFileResponse
	if err := json.Unmarshal(body, &decoded, json.RejectUnknownMembers(true)); err != nil {
		return validatedProviderFile{}, "", &ProviderError{Kind: ErrPermanentResponse}
	}
	validated, ok := validateCreatedWireFile(decoded.File, file, startedAt, completedAt)
	if !ok {
		return validatedProviderFile{}, "", &ProviderError{Kind: ErrPermanentResponse}
	}
	return validated, response.Header.Get("X-Goog-Request-Id"), nil
}

func (client *Client) waitForActiveFile(ctx context.Context, current validatedProviderFile, file verifiedFile, secret string, receipt *Receipt) (validatedProviderFile, error) {
	if current.file.State == "ACTIVE" {
		return current, nil
	}
	for attempt := range client.profile.MaxPollAttempts {
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, origin+filesPathPrefix+strings.TrimPrefix(current.file.Name, "files/"), nil)
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
		if !recordReceiptResponse(receipt, nil, response.Header.Get("X-Goog-Request-Id")) {
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
	if !recordReceiptResponse(receipt, nil, response.Header.Get("X-Goog-Request-Id")) {
		return &ProviderError{Kind: ErrPermanentResponse}
	}
	return nil
}

func validateUploadURL(value string) (*url.URL, error) {
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme != "https" || parsed.Hostname() != host ||
		(parsed.Port() != "" && parsed.Port() != "443") || parsed.User != nil || parsed.ForceQuery ||
		parsed.Fragment != "" || parsed.RawPath != "" || parsed.Opaque != "" ||
		parsed.Path != filesUploadPath || !validUploadQuery(parsed.RawQuery) {
		return nil, errors.New("gemini embed: provider upload URL is outside sealed egress")
	}
	return parsed, nil
}

func validUploadQuery(rawQuery string) bool {
	query, err := url.ParseQuery(rawQuery)
	if err != nil || len(query) != 2 || len(query["upload_id"]) != 1 ||
		len(query["upload_protocol"]) != 1 || query.Get("upload_protocol") != "resumable" {
		return false
	}
	uploadID := query.Get("upload_id")
	if len(uploadID) == 0 || len(uploadID) > 1024 {
		return false
	}
	for _, character := range uploadID {
		if character != '-' && character != '_' &&
			(character < 'a' || character > 'z') && (character < 'A' || character > 'Z') &&
			(character < '0' || character > '9') {
			return false
		}
	}
	first := "upload_id=" + uploadID + "&upload_protocol=resumable"
	second := "upload_protocol=resumable&upload_id=" + uploadID
	return rawQuery == first || rawQuery == second
}

type validatedProviderFile struct {
	file      wireFile
	created   time.Time
	updated   time.Time
	expiresAt time.Time
}

func validateWireFile(value wireFile, expected verifiedFile) (validatedProviderFile, bool) {
	if !validFileName(value.Name) || value.URI != origin+"/v1beta/"+value.Name ||
		value.MIMEType != expected.metadata.MediaType || value.Source != "UPLOADED" || value.Error != nil ||
		value.State != "PROCESSING" && value.State != "ACTIVE" && value.State != "FAILED" ||
		len(value.DisplayName) > 512 {
		return validatedProviderFile{}, false
	}
	size, err := strconv.ParseInt(value.SizeBytes, 10, 64)
	if err != nil || size != expected.metadata.ByteLength {
		return validatedProviderFile{}, false
	}
	hash, err := base64.StdEncoding.Strict().DecodeString(value.SHA256Hash)
	if err != nil || !matchesFileHash(hash, expected.metadata.SHA256) {
		return validatedProviderFile{}, false
	}
	created, createErr := time.Parse(time.RFC3339Nano, value.CreateTime)
	updated, updateErr := time.Parse(time.RFC3339Nano, value.UpdateTime)
	expires, expiryErr := time.Parse(time.RFC3339Nano, value.ExpirationTime)
	if createErr != nil || updateErr != nil || expiryErr != nil || updated.Before(created) ||
		updated.After(expires) || !expires.After(created) || expires.Sub(created) > retentionCeiling {
		return validatedProviderFile{}, false
	}
	return validatedProviderFile{file: value, created: created, updated: updated, expiresAt: expires}, true
}

func validateCreatedWireFile(value wireFile, expected verifiedFile, startedAt, completedAt time.Time) (validatedProviderFile, bool) {
	validated, ok := validateWireFile(value, expected)
	if !ok || validated.created.Before(startedAt.Add(-fileClockSkew)) ||
		validated.created.After(completedAt.Add(fileClockSkew)) ||
		validated.updated.Before(startedAt.Add(-fileClockSkew)) ||
		validated.updated.After(completedAt.Add(fileClockSkew)) ||
		!validated.expiresAt.After(completedAt) {
		return validatedProviderFile{}, false
	}
	return validated, true
}

func validatePolledWireFile(value wireFile, expected verifiedFile, current validatedProviderFile, startedAt, completedAt time.Time) (validatedProviderFile, bool) {
	validated, ok := validateWireFile(value, expected)
	if !ok || value.Name != current.file.Name || value.URI != current.file.URI ||
		!validated.created.Equal(current.created) || !validated.expiresAt.Equal(current.expiresAt) ||
		validated.updated.Before(current.updated) || validated.updated.Before(startedAt.Add(-fileClockSkew)) ||
		validated.updated.After(completedAt.Add(fileClockSkew)) || !validated.expiresAt.After(completedAt) {
		return validatedProviderFile{}, false
	}
	return validated, true
}

func matchesFileHash(decoded []byte, expectedHex string) bool {
	return len(decoded) == sha256.Size && hex.EncodeToString(decoded) == expectedHex ||
		len(decoded) == sha256.Size*2 && string(decoded) == expectedHex
}

func validFileName(name string) bool {
	id := strings.TrimPrefix(name, "files/")
	if id == name || len(id) == 0 || len(id) > 40 || id[0] == '-' || id[len(id)-1] == '-' {
		return false
	}
	for _, character := range id {
		if character != '-' && (character < 'a' || character > 'z') && (character < '0' || character > '9') {
			return false
		}
	}
	return true
}

func requestFailure(ctx context.Context, operation string, _ error) error {
	if contextErr := ctx.Err(); contextErr != nil {
		return fmt.Errorf("gemini embed: %s canceled: %w", operation, contextErr)
	}
	return fmt.Errorf("gemini embed: %s failed", operation)
}

func beginReceiptRequest(receipt *Receipt) bool {
	if receipt == nil {
		return true
	}
	if receipt.RequestCount == int(^uint(0)>>1) {
		return false
	}
	receipt.RequestCount++
	return true
}

func addCleanupWarning(receipt *Receipt) {
	if len(receipt.Warnings) >= 32 {
		return
	}
	receipt.Warnings = append(receipt.Warnings, "provider file deletion attempt failed")
}

func (client *Client) execute(ctx context.Context, payload []byte, secret string, receipt *Receipt) ([]float32, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, origin+embedPath, bytes.NewReader(payload))
	if err != nil {
		return nil, errors.New("gemini embed: request construction failed")
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Goog-Api-Key", secret)
	if !beginReceiptRequest(receipt) {
		return nil, &ProviderError{Kind: ErrPermanentResponse}
	}
	response, err := client.http.Do(request)
	if err != nil {
		if contextErr := ctx.Err(); contextErr != nil {
			return nil, fmt.Errorf("gemini embed: request canceled: %w", contextErr)
		}
		return nil, errors.New("gemini embed: provider request failed")
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return nil, statusError(response.StatusCode, response.Header.Get("Retry-After"), time.Now().UTC())
	}
	if !isJSONContentType(response.Header.Get("Content-Type")) {
		return nil, &ProviderError{Kind: ErrPermanentResponse}
	}
	body, err := readBounded(ctx, response.Body, client.profile.MaxResponseBytes)
	if err != nil {
		return nil, err
	}
	defer clear(body)
	var decoded wireResponse
	if err := json.Unmarshal(body, &decoded, json.RejectUnknownMembers(true)); err != nil {
		return nil, &ProviderError{Kind: ErrPermanentResponse}
	}
	if len(decoded.Embedding.Values) != client.descriptor.Dimension || !validUsage(decoded.UsageMetadata) {
		return nil, &ProviderError{Kind: ErrPermanentResponse}
	}
	if !normalizeUnitVector(decoded.Embedding.Values) {
		return nil, &ProviderError{Kind: ErrPermanentResponse}
	}
	if !recordReceiptResponse(receipt, decoded.UsageMetadata, response.Header.Get("X-Goog-Request-Id")) {
		return nil, &ProviderError{Kind: ErrPermanentResponse}
	}
	return decoded.Embedding.Values, nil
}

func normalizeUnitVector(vector []float32) bool {
	var squaredNorm float64
	for _, value := range vector {
		floatValue := float64(value)
		if math.IsNaN(floatValue) || math.IsInf(floatValue, 0) {
			return false
		}
		squaredNorm += floatValue * floatValue
	}
	if squaredNorm == 0 || math.IsInf(squaredNorm, 0) {
		return false
	}
	norm := math.Sqrt(squaredNorm)
	for index, value := range vector {
		vector[index] = float32(float64(value) / norm)
	}
	return true
}

func validSecret(value string) bool { return validToken(value) && len(value) <= maxSecretBytes }

func isJSONContentType(value string) bool {
	mediaType, _, found := strings.Cut(value, ";")
	return strings.EqualFold(strings.TrimSpace(mediaType), "application/json") && found || strings.EqualFold(strings.TrimSpace(value), "application/json")
}

func readBounded(ctx context.Context, body io.Reader, maximum int64) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(body, maximum+1))
	if err != nil {
		if contextErr := ctx.Err(); contextErr != nil {
			return nil, fmt.Errorf("gemini embed: response read canceled: %w", contextErr)
		}
		return nil, errors.New("gemini embed: provider response read failed")
	}
	if int64(len(data)) > maximum {
		clear(data)
		return nil, errors.New("gemini embed: provider response exceeds byte capacity")
	}
	return data, nil
}

func validUsage(usage *wireUsage) bool {
	if usage == nil {
		return true
	}
	for _, value := range []*int64{usage.PromptTokenCount, usage.CachedContentTokenCount, usage.CandidatesTokenCount,
		usage.ToolUsePromptTokenCount, usage.ThoughtsTokenCount, usage.TotalTokenCount} {
		if value != nil && (*value < 0 || *value > maxUsageValue) {
			return false
		}
	}
	return true
}

func recordReceiptResponse(receipt *Receipt, usage *wireUsage, responseID string) bool {
	if receipt == nil {
		return true
	}
	if responseID != "" && !validToken(responseID) {
		return false
	}
	if usage != nil {
		values := []struct{ source, destination *int64 }{
			{usage.PromptTokenCount, &receipt.PromptTokens}, {usage.CachedContentTokenCount, &receipt.CachedContentTokens},
			{usage.CandidatesTokenCount, &receipt.CandidateTokens}, {usage.ToolUsePromptTokenCount, &receipt.ToolUsePromptTokens},
			{usage.ThoughtsTokenCount, &receipt.ThoughtTokens}, {usage.TotalTokenCount, &receipt.TotalTokens},
		}
		for _, value := range values {
			source, destination := value.source, value.destination
			if source != nil && *source > maxUsageValue-*destination {
				return false
			}
		}
		for _, value := range values {
			source, destination := value.source, value.destination
			if source != nil {
				*destination += *source
			}
		}
	}
	if responseID != "" {
		if len(receipt.ProviderResponseIDs) < 128 {
			receipt.ProviderResponseIDs = append(receipt.ProviderResponseIDs, responseID)
		} else {
			receipt.OmittedProviderResponseIDs++
		}
	}
	return true
}

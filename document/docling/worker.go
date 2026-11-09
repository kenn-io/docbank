package docling

import (
	"context"
	"encoding/json/v2"
	"errors"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"time"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/internal/providerutil"
	"go.kenn.io/docbank/document/workerprotocol"
)

// WorkerDocumentDescriptor declares the durable operator-network document pilot.
func WorkerDocumentDescriptor() (document.RenditionDescriptor, error) {
	return workerprotocol.DocumentDescriptor()
}

// WorkerASRDescriptor binds qualified audio evidence and transcript limits.
func WorkerASRDescriptor(maxTranscriptChars int) (document.RenditionDescriptor, error) {
	fingerprint, err := asrPolicyFingerprint(maxTranscriptChars, workerprotocol.ASRAdapter)
	if err != nil {
		return document.RenditionDescriptor{}, err
	}
	return workerprotocol.ASRDescriptor(fingerprint)
}

// WorkerDisclosureFingerprint binds the explicit worker adapter to its deployment.
func WorkerDisclosureFingerprint(adapter string, descriptor document.RenditionDescriptor, endpoint, deployment string) string {
	return providerutil.RenditionDisclosureFingerprint(adapter, descriptor, endpoint, deployment)
}

// WorkerClient resumes issued gateway operations; native Docling stays separate.
type WorkerClient struct{ *Client }

// WorkerASRClient resumes the locally proven audio projection without reopening it.
type WorkerASRClient struct{ *ASRClient }

// NewWorker constructs the explicit durable gateway document adapter.
func NewWorker(profile Profile, secrets SecretResolver, httpClient *http.Client) (*WorkerClient, error) {
	expected, err := WorkerDocumentDescriptor()
	if err != nil {
		return nil, err
	}
	if !reflect.DeepEqual(profile.Descriptor, expected) {
		return nil, errors.New("docling: worker document descriptor is not qualified")
	}
	if err := workerBounds(&profile); err != nil {
		return nil, err
	}
	client, err := newTransportClientFor(profile, secrets, httpClient, workerprotocol.ProviderID)
	if err != nil {
		return nil, err
	}
	return &WorkerClient{client}, nil
}

// NewWorkerASR constructs the explicit durable gateway WAV/MP3 adapter.
func NewWorkerASR(profile ASRProfile, secrets SecretResolver, httpClient *http.Client) (*WorkerASRClient, error) {
	expected, err := WorkerASRDescriptor(profile.MaxTranscriptChars)
	if err != nil {
		return nil, err
	}
	if !reflect.DeepEqual(profile.Descriptor, expected) {
		return nil, errors.New("docling: worker ASR descriptor is not qualified")
	}
	if err := workerBounds(&profile.Profile); err != nil {
		return nil, err
	}
	client, err := newASRFor(profile, secrets, httpClient, workerprotocol.ProviderID, workerprotocol.ASRAdapter)
	if err != nil {
		return nil, err
	}
	return &WorkerASRClient{client}, nil
}
func workerBounds(profile *Profile) error {
	if profile.MaxDocumentBytes == 0 {
		profile.MaxDocumentBytes = 64 << 20
	}
	if profile.MaxResponseBytes == 0 {
		profile.MaxResponseBytes = 64 << 20
	}
	if profile.MaxDocumentBytes < 1 || profile.MaxDocumentBytes > 100<<20 || profile.MaxResponseBytes < 1 || profile.MaxResponseBytes > 128<<20 || profile.SecretBinding == "" {
		return errors.New("docling: worker bounds and credential binding are required")
	}
	return nil
}

func (client *WorkerClient) Render(ctx context.Context, upload document.AuthorizedUpload, authorization document.RenditionAuthorization) (document.RenditionResult, error) {
	return client.RenderResumable(ctx, upload, authorization, nil, nil)
}
func (client *WorkerASRClient) Render(ctx context.Context, upload document.AuthorizedUpload, authorization document.RenditionAuthorization) (document.RenditionResult, error) {
	return client.RenderResumable(ctx, upload, authorization, nil, nil)
}

// RenderResumable checkpoints an issued ID before polling; resume consumes no source.
func (client *WorkerClient) RenderResumable(ctx context.Context, upload document.AuthorizedUpload, authorization document.RenditionAuthorization, resume *document.RenditionResumeHandle, checkpoint document.RenditionResumeCheckpoint) (document.RenditionResult, error) {
	if client == nil || client.Client == nil {
		return document.RenditionResult{}, errors.New("docling: worker client is required")
	}
	fields := [][2]string{{"to_formats", "json"}, {"target_type", "inbody"}}
	if authorization.MaxProviderMarkdownBytes > 0 {
		fields = [][2]string{{"to_formats", "md"}, {"to_formats", "json"}, {"target_type", "inbody"}}
	}
	return client.renderWorker(ctx, upload, authorization, resume, checkpoint, 0, fields, func(run *rendering, task taskResponse, result doclingResult, _ int64) (document.RenditionResult, error) {
		return client.buildResult(run, task, result, 1, false)
	})
}

// RenderResumable retains only the initially verified ASR duration in its checkpoint.
func (client *WorkerASRClient) RenderResumable(ctx context.Context, upload document.AuthorizedUpload, authorization document.RenditionAuthorization, resume *document.RenditionResumeHandle, checkpoint document.RenditionResumeCheckpoint) (document.RenditionResult, error) {
	if client == nil || client.ASRClient == nil {
		return document.RenditionResult{}, errors.New("docling: worker ASR client is required")
	}
	if !providerutil.AllowsArtifact(authorization, document.EvidenceArtifactTranscript) {
		return document.RenditionResult{}, provider.Classified(document.RenditionErrorPolicyRejected, "worker transcript retention is required", nil)
	}
	var duration int64
	if resume == nil {
		if providerutil.IsNil(upload) {
			return document.RenditionResult{}, provider.Classified(document.RenditionErrorPolicyRejected, "worker source is required", nil)
		}
		var err error
		duration, err = client.inputDuration(upload, authorization)
		if err != nil {
			return document.RenditionResult{}, err
		}
	}
	return client.client.renderWorker(ctx, upload, authorization, resume, checkpoint, duration, [][2]string{{"from_formats", "audio"}, {"to_formats", "json"}, {"target_type", "inbody"}}, client.buildResult)
}

type workerResume struct {
	id, authorization, filenameSHA256 string
	started, accepted                 time.Time
	durationMS                        int64
}

func encodeWorkerResume(value workerResume) string {
	return strings.Join([]string{"cw1", value.id, value.authorization, strconv.FormatInt(value.started.UnixNano(), 10), strconv.FormatInt(value.accepted.UnixNano(), 10), value.filenameSHA256, strconv.FormatInt(value.durationMS, 10)}, ".")
}
func parseWorkerResume(value string) (workerResume, error) {
	if len(value) > 512 {
		return workerResume{}, errors.New("worker handle exceeds bound")
	}
	parts := strings.Split(value, ".")
	if len(parts) != 7 || parts[0] != "cw1" || !workerHex(parts[1], 32) || !workerHex(parts[2], 64) || !workerHex(parts[5], 64) {
		return workerResume{}, errors.New("worker handle identity is invalid")
	}
	numbers := make([]int64, 3)
	for i, index := range []int{3, 4, 6} {
		n, err := strconv.ParseInt(parts[index], 10, 64)
		if err != nil || strconv.FormatInt(n, 10) != parts[index] || n < 0 {
			return workerResume{}, errors.New("worker handle number is invalid")
		}
		numbers[i] = n
	}
	if numbers[0] <= 0 || numbers[1] < numbers[0] || numbers[2] > (24*time.Hour).Milliseconds() {
		return workerResume{}, errors.New("worker handle acceptance facts are invalid")
	}
	return workerResume{id: parts[1], authorization: parts[2], started: time.Unix(0, numbers[0]).UTC(), accepted: time.Unix(0, numbers[1]).UTC(), filenameSHA256: parts[5], durationMS: numbers[2]}, nil
}
func workerHex(value string, length int) bool {
	if len(value) != length {
		return false
	}
	for _, char := range value {
		if char >= '0' && char <= '9' || char >= 'a' && char <= 'f' {
			continue
		}
		return false
	}
	return true
}

type workerResultBuilder func(*rendering, taskResponse, doclingResult, int64) (document.RenditionResult, error)

func (client *Client) renderWorker(ctx context.Context, upload document.AuthorizedUpload, authorization document.RenditionAuthorization, resume *document.RenditionResumeHandle, checkpoint document.RenditionResumeCheckpoint, duration int64, fields [][2]string, build workerResultBuilder) (document.RenditionResult, error) {
	fingerprint, err := authorization.Fingerprint()
	if err != nil {
		return document.RenditionResult{}, provider.Classified(document.RenditionErrorPolicyRejected, "worker authority is invalid", err)
	}
	if authorization.ProviderID != client.descriptor.ID || authorization.DescriptorFingerprint != client.descriptor.Fingerprint || authorization.PolicyFingerprint != client.descriptor.PolicyFingerprint {
		return document.RenditionResult{}, provider.Classified(document.RenditionErrorPolicyRejected, "worker authority differs from descriptor", nil)
	}
	var run *rendering
	var task taskResponse
	if resume != nil {
		if !providerutil.IsNil(upload) {
			return document.RenditionResult{}, provider.Classified(document.RenditionErrorPolicyRejected, "worker resume must not receive source", nil)
		}
		saved, err := parseWorkerResume(resume.Value)
		if err != nil {
			return document.RenditionResult{}, provider.Classified(document.RenditionErrorUnknownJob, "worker handle is invalid", err)
		}
		start, startErr := time.Parse(providerutil.TimestampForm, authorization.AuthorizedAt)
		expiry, expiryErr := time.Parse(providerutil.TimestampForm, authorization.ExpiresAt)
		audio := authorization.MediaFamily == "audio"
		if saved.authorization != fingerprint || startErr != nil || expiryErr != nil || saved.started.Before(start) || !saved.accepted.Before(expiry) || audio != (saved.durationMS > 0) {
			return document.RenditionResult{}, provider.Classified(document.RenditionErrorPolicyRejected, "worker resume authority changed", nil)
		}
		// These are the original adapter's acceptance facts, not a reconstructed capability.
		run = &rendering{operation: providerutil.NewResumedOperation(ctx, provider, client.totalTimeout), authorization: authorization, metadata: document.AuthorizedUploadMetadata{SHA256: authorization.SourceSHA256, ByteLength: authorization.SourceBytes, MediaType: authorization.MediaType, MediaFamily: authorization.MediaFamily, InputKind: authorization.InputKind}, started: saved.started, completed: saved.accepted, inputBytes: authorization.SourceBytes, expectedFilenameSHA256: saved.filenameSHA256, includeMarkdown: client.descriptor.ReturnsMarkdown && authorization.MaxProviderMarkdownBytes > 0, workerProtocol: true}
		duration = saved.durationMS
		task = taskResponse{id: saved.id, status: "pending"}
	} else {
		if providerutil.IsNil(upload) {
			return document.RenditionResult{}, provider.Classified(document.RenditionErrorPolicyRejected, "worker source is required", nil)
		}
		if _, encrypted := document.UploadPDFPassword(upload); encrypted {
			return document.RenditionResult{}, provider.Classified(document.RenditionErrorUnsupportedInput, "worker pilot excludes password-decrypted PDFs", nil)
		}
		metadata := upload.Metadata()
		if !authorization.DiscloseFilename {
			metadata.Filename = ""
		}
		key, err := workerprotocol.SubmissionKey(metadata, authorization, fields)
		if err != nil {
			return document.RenditionResult{}, provider.Classified(document.RenditionErrorPolicyRejected, "worker request identity is invalid", err)
		}
		run, err = client.newRendering(ctx, metadata, authorization)
		if err != nil {
			return document.RenditionResult{}, err
		}
		run.workerProtocol = true
		found, exists, lookupErr := client.lookupWorker(ctx, run, key)
		if lookupErr != nil {
			run.operation.Cancel()
			return document.RenditionResult{}, lookupErr
		}
		if !exists {
			if err := client.readyWorker(ctx, run); err != nil {
				run.operation.Cancel()
				return document.RenditionResult{}, err
			}
		}
		if err := client.readSource(run, upload); err != nil {
			run.operation.Cancel()
			return document.RenditionResult{}, err
		}
		run.expectedFilenameSHA256 = providerutil.SHA256Hex([]byte(metadata.Filename))
		if run.metadata.Filename == "" {
			run.metadata.Filename = workerFilename(metadata.MediaType)
		}
		if exists {
			task = found
		} else {
			run.headers = map[string]string{"Idempotency-Key": key, workerprotocol.SourceSHA256Header: metadata.SHA256}
			task, err = client.submit(run, fields)
			if err != nil {
				run.operation.Cancel()
				clear(run.source)
				if classified, ok := errors.AsType[*document.RenditionProviderError](err); ok && classified.Code() == document.RenditionErrorAmbiguousSubmission {
					return document.RenditionResult{}, provider.Classified(document.RenditionErrorTransient, "worker submission can be reconciled by key", err)
				}
				return document.RenditionResult{}, err
			}
		}
		if !workerHex(task.id, 32) {
			run.operation.Cancel()
			clear(run.source)
			return document.RenditionResult{}, provider.Malformed("worker issued an invalid task ID", nil)
		}
		run.completed = time.Now().UTC()
		if err := run.operation.Check(); err != nil {
			run.operation.Cancel()
			clear(run.source)
			return document.RenditionResult{}, err
		}
		handle := encodeWorkerResume(workerResume{id: task.id, authorization: fingerprint, started: run.started, accepted: run.completed, filenameSHA256: run.expectedFilenameSHA256, durationMS: duration})
		if len(handle) > 512 {
			run.operation.Cancel()
			clear(run.source)
			return document.RenditionResult{}, provider.Malformed("worker checkpoint exceeds bound", nil)
		}
		if checkpoint != nil {
			if err := checkpoint(document.RenditionResumeHandle{Value: handle}); err != nil {
				run.operation.Cancel()
				clear(run.source)
				return document.RenditionResult{}, err
			}
		}
	}
	defer run.operation.Cancel()
	defer clear(run.source)
	task, err = client.awaitTask(run, task)
	if err != nil {
		return document.RenditionResult{}, err
	}
	result, err := client.awaitResult(run, task.id)
	if err != nil {
		return document.RenditionResult{}, err
	}
	if task.status == "success" && result.status == "success" && len(result.errors) > 0 {
		return document.RenditionResult{}, provider.Malformed("worker successful result contains errors", nil)
	}
	if authorization.DiscloseFilename && providerutil.SHA256Hex([]byte(result.filename)) != run.expectedFilenameSHA256 {
		return document.RenditionResult{}, provider.Classified(document.RenditionErrorPolicyRejected, "worker result filename differs from source", nil)
	}
	return build(run, task, result, duration)
}
func (client *Client) workerRequest(ctx context.Context, run *rendering, request providerutil.Request) (providerutil.Response, error) {
	if err := ctx.Err(); err != nil {
		return providerutil.Response{}, err
	}
	response, err := client.executor.Do(run.operation, &run.usage, request)
	if err != nil {
		return response, err
	}
	return response, validateWorkerResponse(run, response)
}
func validateWorkerResponse(run *rendering, response providerutil.Response) error {
	if run.workerProtocol && (len(response.Header.Values(workerprotocol.ProtocolHeader)) != 1 || response.Header.Get(workerprotocol.ProtocolHeader) != workerprotocol.Contract) {
		return provider.Classified(document.RenditionErrorPolicyRejected, "endpoint does not speak the durable worker protocol", nil)
	}
	return nil
}
func (client *Client) lookupWorker(ctx context.Context, run *rendering, key string) (taskResponse, bool, error) {
	response, err := client.workerRequest(ctx, run, providerutil.Request{Stage: providerutil.StageJob, Method: http.MethodGet, Path: workerprotocol.LookupPath + key, MaxResponseBytes: 16 << 10})
	if err != nil {
		return taskResponse{}, false, err
	}
	if response.Status == 404 {
		return taskResponse{}, false, nil
	}
	if response.Status != 200 {
		return taskResponse{}, false, provider.StatusError(providerutil.StageJob, response.Status, response.RetryAfter, nil)
	}
	task, err := parseTaskForProtocol(response.Body, true)
	if err != nil || !workerHex(task.id, 32) {
		return taskResponse{}, false, provider.Malformed("worker lookup is invalid", err)
	}
	return task, true, nil
}
func (client *Client) readyWorker(ctx context.Context, run *rendering) error {
	response, err := client.workerRequest(ctx, run, providerutil.Request{Stage: providerutil.StageJob, Method: http.MethodGet, Path: workerprotocol.ReadyPath, MaxResponseBytes: 4096})
	if err != nil {
		return err
	}
	if response.Status != 200 {
		return provider.StatusError(providerutil.StageJob, response.Status, response.RetryAfter, nil)
	}
	var ready struct {
		Protocol string `json:"protocol"`
	}
	if err := json.Unmarshal(response.Body, &ready); err != nil || ready.Protocol != workerprotocol.Contract {
		return provider.Malformed("worker readiness contract differs", err)
	}
	return nil
}
func workerFilename(mediaType string) string {
	extensions := map[string]string{"application/pdf": ".pdf", "application/vnd.openxmlformats-officedocument.presentationml.presentation": ".pptx", "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet": ".xlsx", "image/png": ".png", "image/jpeg": ".jpg", "text/plain": ".txt", "text/markdown": ".md", "audio/wav": ".wav", "audio/mpeg": ".mp3"}
	return "document" + extensions[mediaType]
}

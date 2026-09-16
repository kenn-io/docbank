package api

import (
	"context"
	"encoding/json/v2"
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"reflect"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"go.kenn.io/docbank/internal/processing"
	"go.kenn.io/docbank/internal/store"
)

func registerMediaRoutes(mux *http.ServeMux, api huma.API, d Deps, g *gate) {
	registerMediaUploadOpenAPI(api)
	mux.HandleFunc("POST /api/v1/media/sources", func(w http.ResponseWriter, r *http.Request) {
		handleMediaSourceSubmission(w, r, d)
	})
	mux.HandleFunc("POST /api/v1/media/sources/{source_id}/artifacts", func(w http.ResponseWriter, r *http.Request) {
		handleMediaArtifactUpload(w, r, d)
	})

	type sourcePageOutput struct{ Body MediaSourcePage }
	huma.Register(api, huma.Operation{OperationID: "listMediaSources", Method: http.MethodGet,
		Path: "/api/v1/media/sources", Summary: "List caller-visible media sources"},
		func(ctx context.Context, input *struct {
			Cursor string `query:"cursor" maxLength:"4096"`
			Limit  int    `query:"limit" default:"100" minimum:"1" maximum:"250"`
		}) (*sourcePageOutput, error) {
			if d.Processing == nil {
				return nil, mediaUnavailable()
			}
			page, err := d.Processing.ListMediaSources(ctx, processing.MediaListOptions{
				Cursor: input.Cursor, Limit: input.Limit})
			if err != nil {
				return nil, fromMediaError(err)
			}
			return &sourcePageOutput{Body: fromMediaSourcePage(page)}, nil
		})

	type receiptOutput struct{ Body MediaReceipt }
	huma.Register(api, huma.Operation{OperationID: "getMediaSource", Method: http.MethodGet,
		Path: "/api/v1/media/sources/{source_id}", Summary: "Read caller-visible media status"},
		func(ctx context.Context, input *struct {
			SourceID string `path:"source_id" minLength:"1" maxLength:"256"`
		}) (*receiptOutput, error) {
			if d.Processing == nil {
				return nil, mediaUnavailable()
			}
			receipt, err := d.Processing.MediaStatus(ctx, input.SourceID)
			if err != nil {
				return nil, fromMediaError(err)
			}
			return &receiptOutput{Body: fromMediaReceipt(receipt)}, nil
		})

	huma.Register(api, huma.Operation{OperationID: "retryMediaSource", Method: http.MethodPost,
		Path: "/api/v1/media/sources/{source_id}/retry", Summary: "Retry explicit processing for one source",
		BodyReadTimeout: -1},
		func(ctx context.Context, input *struct {
			SourceID string `path:"source_id" minLength:"1" maxLength:"256"`
			Body     MediaRetryBody
		}) (*receiptOutput, error) {
			if d.Processing == nil {
				return nil, mediaUnavailable()
			}
			if input.Body.Processing == nil {
				return nil, NewError(http.StatusUnprocessableEntity, "validation",
					"media retry requires a processing profile")
			}
			receipt, err := d.Processing.RetryMedia(ctx, input.Body.OperationID, input.SourceID,
				toMediaProcessing(*input.Body.Processing))
			if err != nil {
				return nil, fromMediaError(err)
			}
			return &receiptOutput{Body: fromMediaReceipt(receipt)}, nil
		})

	type occurrencePageOutput struct{ Body MediaOccurrencePage }
	huma.Register(api, huma.Operation{OperationID: "listMediaOccurrences", Method: http.MethodGet,
		Path: "/api/v1/media/occurrences", Summary: "List caller-visible media occurrences"},
		func(ctx context.Context, input *struct {
			Cursor   string `query:"cursor" maxLength:"4096"`
			Limit    int    `query:"limit" default:"100" minimum:"1" maximum:"250"`
			SourceID string `query:"source_id" maxLength:"256"`
		}) (*occurrencePageOutput, error) {
			if d.Processing == nil {
				return nil, mediaUnavailable()
			}
			page, err := d.Processing.ListMediaOccurrences(ctx, processing.MediaListOptions{
				Cursor: input.Cursor, Limit: input.Limit, SourceID: input.SourceID})
			if err != nil {
				return nil, fromMediaError(err)
			}
			return &occurrencePageOutput{Body: fromMediaOccurrencePage(page)}, nil
		})

	huma.Register(api, huma.Operation{OperationID: "declareMediaOccurrence", Method: http.MethodPost,
		Path: "/api/v1/media/occurrences", Summary: "Declare one immutable caller occurrence revision"},
		func(ctx context.Context, input *struct{ Body MediaOccurrenceMutationBody }) (*receiptOutput, error) {
			if d.Processing == nil {
				return nil, mediaUnavailable()
			}
			receipt, err := d.Processing.DeclareMediaOccurrence(ctx, input.Body.OperationID,
				input.Body.SourceID, toMediaOccurrence(input.Body.Occurrence))
			if err != nil {
				return nil, fromMediaError(err)
			}
			return &receiptOutput{Body: fromMediaReceipt(receipt)}, nil
		})

	huma.Register(api, huma.Operation{OperationID: "revokeMediaOccurrence", Method: http.MethodDelete,
		Path: "/api/v1/media/occurrences/{occurrence_id}", Summary: "Revoke one caller-owned occurrence"},
		func(ctx context.Context, input *struct {
			OccurrenceID string `path:"occurrence_id" minLength:"1" maxLength:"256"`
			Body         MediaOccurrenceRevokeBody
		}) (*receiptOutput, error) {
			if d.Processing == nil {
				return nil, mediaUnavailable()
			}
			receipt, err := d.Processing.RevokeMediaOccurrence(ctx, input.Body.OperationID,
				input.OccurrenceID, input.Body.Revision)
			if err != nil {
				return nil, fromMediaError(err)
			}
			return &receiptOutput{Body: fromMediaReceipt(receipt)}, nil
		})

	type originOutput struct{ Body MediaOriginPage }
	huma.Register(api, huma.Operation{OperationID: "listMediaOrigins", Method: http.MethodGet,
		Path: "/api/v1/media/origins", Summary: "List registered media origin capabilities"},
		func(_ context.Context, _ *struct{}) (*originOutput, error) {
			if d.Processing == nil {
				return nil, mediaUnavailable()
			}
			origins, err := d.Processing.MediaOrigins()
			if err != nil {
				return nil, fromMediaError(err)
			}
			items := make([]MediaOrigin, len(origins))
			for index, origin := range origins {
				items[index] = MediaOrigin{OriginID: origin.OriginID, Provider: origin.Provider,
					AcquisitionAvailable: origin.AcquisitionAvailable}
			}
			return &originOutput{Body: MediaOriginPage{Items: items}}, nil
		})

	type planOutput struct{ Body MediaAcquisitionPlan }
	huma.Register(api, huma.Operation{OperationID: "planMediaAcquisition", Method: http.MethodPost,
		Path: "/api/v1/media/acquisition-plan", Summary: "Recognize a private reference without network access"},
		func(ctx context.Context, input *struct{ Body MediaReferenceBody }) (*planOutput, error) {
			if d.Processing == nil {
				return nil, mediaUnavailable()
			}
			plan, err := d.Processing.PlanMediaAcquisition(ctx, toRemoteRecording(input.Body))
			if err != nil {
				return nil, fromMediaError(err)
			}
			return &planOutput{Body: MediaAcquisitionPlan{PlanToken: plan.PlanToken,
				PlanFingerprint: plan.PlanFingerprint, OriginID: plan.OriginID, Provider: plan.Provider,
				InputClasses: plan.InputClasses, RetainedClasses: plan.RetainedClasses, GrantState: plan.GrantState}}, nil
		})

	type consentOutput struct{ Body MediaConsentReceipt }
	huma.Register(api, huma.Operation{OperationID: "grantMediaAcquisitionConsent", Method: http.MethodPost,
		Path: "/api/v1/media/consent/grants", Summary: "Grant one exact current media acquisition plan"},
		func(ctx context.Context, input *struct{ Body MediaAcquisitionGrantBody }) (*consentOutput, error) {
			if d.Processing == nil {
				return nil, mediaUnavailable()
			}
			var expiresAt *time.Time
			if input.Body.ExpiresAt != "" {
				value, err := time.Parse(time.RFC3339Nano, input.Body.ExpiresAt)
				if err != nil {
					return nil, NewError(http.StatusUnprocessableEntity, "validation", "media consent expiry is invalid")
				}
				expiresAt = &value
			}
			var receipt store.MediaConsentReceipt
			err := g.mutate(func() error {
				var err error
				receipt, err = d.Processing.GrantMediaAcquisition(ctx, input.Body.OperationID, input.Body.PlanToken, expiresAt)
				if err != nil {
					return fromMediaError(err)
				}
				return nil
			})
			if err != nil {
				return nil, err
			}
			return &consentOutput{Body: fromMediaConsentReceipt(receipt)}, nil
		})

	huma.Register(api, huma.Operation{OperationID: "revokeMediaAcquisitionConsent", Method: http.MethodPost,
		Path: "/api/v1/media/consent/revocations", Summary: "Revoke acquisition consent for one registered origin"},
		func(ctx context.Context, input *struct{ Body MediaAcquisitionRevokeBody }) (*consentOutput, error) {
			if d.Processing == nil {
				return nil, mediaUnavailable()
			}
			var receipt store.MediaConsentReceipt
			err := g.mutate(func() error {
				var err error
				receipt, err = d.Processing.RevokeMediaAcquisition(ctx, input.Body.OperationID, input.Body.OriginID)
				if err != nil {
					return fromMediaError(err)
				}
				return nil
			})
			if err != nil {
				return nil, err
			}
			return &consentOutput{Body: fromMediaConsentReceipt(receipt)}, nil
		})
}

func registerMediaUploadOpenAPI(api huma.API) {
	const jsonMediaType = "application/json"
	registry := api.OpenAPI().Components.Schemas
	receipt := registry.Schema(reflect.TypeFor[MediaReceipt](), true, "")
	for _, route := range []struct {
		id, path, summary string
		metadata          reflect.Type
	}{
		{"submitMediaSource", "/api/v1/media/sources", "Retain one bounded supplied recording or private reference", reflect.TypeFor[MediaSuppliedMetadata]()},
		{"importMediaArtifact", "/api/v1/media/sources/{source_id}/artifacts", "Retain one bounded original media or transcript input", reflect.TypeFor[MediaArtifactMetadata]()},
	} {
		operation := &huma.Operation{OperationID: route.id, Method: http.MethodPost,
			Path: route.path, Summary: route.summary,
			RequestBody: &huma.RequestBody{Required: true,
				Description: "Multipart submissions require exactly two parts in order: JSON metadata without a filename, then file with a filename.",
				Content: map[string]*huma.MediaType{
					"multipart/form-data": {
						Schema: &huma.Schema{Type: "object", Properties: map[string]*huma.Schema{
							"metadata": registry.Schema(route.metadata, true, ""),
							"file":     {Type: openAPIStringType, Format: "binary"},
						}, Required: []string{"metadata", "file"}, AdditionalProperties: false},
						Encoding: map[string]*huma.Encoding{
							"metadata": {ContentType: jsonMediaType},
							"file":     {ContentType: "*/*"},
						},
					},
				}},
			Responses: map[string]*huma.Response{
				"200": {Description: "Durable media receipt", Content: map[string]*huma.MediaType{
					jsonMediaType: {Schema: receipt}}},
			}}
		if route.id == "submitMediaSource" {
			operation.RequestBody.Content[jsonMediaType] = &huma.MediaType{
				Schema: registry.Schema(reflect.TypeFor[MediaReferenceBody](), true, "")}
		} else {
			operation.Parameters = []*huma.Param{{Name: "source_id", In: "path", Required: true,
				Schema: &huma.Schema{Type: openAPIStringType, MinLength: new(1), MaxLength: new(256)}}}
		}
		api.OpenAPI().AddOperation(operation)
	}
}

func handleMediaSourceSubmission(w http.ResponseWriter, r *http.Request, d Deps) {
	if d.Processing == nil {
		writeError(w, mediaUnavailable())
		return
	}
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil {
		writeError(w, NewError(http.StatusUnsupportedMediaType, "validation", "media submission requires a supported content type"))
		return
	}
	switch mediaType {
	case "application/json":
		var body MediaReferenceBody
		if err := decodeBoundedJSON(r.Body, 64<<10, &body); err != nil {
			writeError(w, NewError(http.StatusUnprocessableEntity, "validation", err.Error()))
			return
		}
		receipt, err := d.Processing.SubmitRemoteRecording(r.Context(), toRemoteRecording(body))
		if err != nil {
			writeError(w, fromMediaError(err))
			return
		}
		writeJSON(w, http.StatusOK, fromMediaReceipt(receipt))
	case "multipart/form-data":
		r.Body = http.MaxBytesReader(w, r.Body, (2<<30)+(128<<10))
		defer func() { _ = r.Body.Close() }()
		var metadata MediaSuppliedMetadata
		part, multipartReader, parseErr := readMediaMultipartMetadata(r, &metadata)
		if parseErr != nil {
			_ = r.Body.Close()
			writeError(w, parseErr)
			return
		}
		staged, stageErr := stageCompleteMediaMultipart(r, d.Processing, part, multipartReader,
			metadata.ByteLength, 2<<30, metadata.SHA256)
		if stageErr != nil {
			_ = r.Body.Close()
			writeError(w, fromMediaError(stageErr))
			return
		}
		defer func() { _ = staged.Close() }()
		receipt, submitErr := d.Processing.SubmitSuppliedMedia(r.Context(), processing.SuppliedMediaRequest{
			OperationID: metadata.OperationID, Content: staged,
			Filename: metadata.Filename, MediaType: metadata.MediaType, SHA256: metadata.SHA256,
			ByteLength: metadata.ByteLength, ExistingContentVersionID: metadata.ExistingContentVersionID,
			Occurrence: toMediaOccurrence(metadata.Occurrence), Processing: toOptionalMediaProcessing(metadata.Processing)})
		if submitErr != nil {
			writeError(w, fromMediaError(submitErr))
			return
		}
		writeJSON(w, http.StatusOK, fromMediaReceipt(receipt))
	default:
		writeError(w, NewError(http.StatusUnsupportedMediaType, "validation", "unsupported media submission content type"))
	}
}

func handleMediaArtifactUpload(w http.ResponseWriter, r *http.Request, d Deps) {
	if d.Processing == nil {
		writeError(w, mediaUnavailable())
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, (2<<30)+(128<<10))
	defer func() { _ = r.Body.Close() }()
	var metadata MediaArtifactMetadata
	part, multipartReader, parseErr := readMediaMultipartMetadata(r, &metadata)
	if parseErr != nil {
		_ = r.Body.Close()
		writeError(w, parseErr)
		return
	}
	limit := int64(2 << 30)
	if metadata.Kind != "media" {
		limit = 16 << 20
	}
	staged, stageErr := stageCompleteMediaMultipart(r, d.Processing, part, multipartReader,
		metadata.ByteLength, limit, metadata.SHA256)
	if stageErr != nil {
		_ = r.Body.Close()
		writeError(w, fromMediaError(stageErr))
		return
	}
	defer func() { _ = staged.Close() }()
	receipt, err := d.Processing.ImportRecordingArtifact(r.Context(), processing.MediaArtifactRequest{
		OperationID: metadata.OperationID, SourceID: r.PathValue("source_id"), OccurrenceID: metadata.OccurrenceID,
		Kind: metadata.Kind, Origin: metadata.Origin, Provider: metadata.Provider, Language: metadata.Language,
		Filename: metadata.Filename, MediaType: metadata.MediaType, SHA256: metadata.SHA256,
		ByteLength: metadata.ByteLength, Content: staged})
	if err != nil {
		writeError(w, fromMediaError(err))
		return
	}
	writeJSON(w, http.StatusOK, fromMediaReceipt(receipt))
}

func stageCompleteMediaMultipart(
	r *http.Request, service *processing.Service, part io.Reader, reader *multipart.Reader,
	expectedSize, maxBytes int64, expectedSHA string,
) (*processing.StagedMediaContent, error) {
	if expectedSize < 1 || expectedSize > maxBytes || maxBytes < 1 || maxBytes > 2<<30 {
		return nil, errors.New("byte_limit")
	}
	staged, err := service.StageMediaContent(r.Context(), part, expectedSize, maxBytes, expectedSHA)
	if err != nil {
		_ = r.Body.Close()
		if strings.Contains(err.Error(), "unexpected EOF") {
			return nil, errors.New("invalid media upload envelope")
		}
		return nil, err
	}
	fail := func(cause error) (*processing.StagedMediaContent, error) {
		_ = staged.Close()
		_ = r.Body.Close()
		return nil, cause
	}
	next, nextErr := reader.NextPart()
	if next != nil || !errors.Is(nextErr, io.EOF) {
		return fail(errors.New("media upload requires exactly metadata and file parts"))
	}
	return staged, nil
}

func readMediaMultipartMetadata[T any](r *http.Request, metadata *T) (io.ReadCloser, *multipart.Reader, *Error) {
	reader, err := r.MultipartReader()
	if err != nil {
		return nil, nil, NewError(http.StatusUnsupportedMediaType, "validation", "media upload requires multipart/form-data")
	}
	metadataPart, err := reader.NextPart()
	if err != nil || metadataPart.FormName() != "metadata" || metadataPart.FileName() != "" {
		_ = r.Body.Close()
		return nil, nil, NewError(http.StatusUnprocessableEntity, "validation", "first multipart part must be metadata")
	}
	err = decodeBoundedJSON(metadataPart, 64<<10, metadata)
	if err != nil {
		_ = r.Body.Close()
		return nil, nil, NewError(http.StatusUnprocessableEntity, "validation", err.Error())
	}
	filePart, err := reader.NextPart()
	if err != nil || filePart.FormName() != "file" || filePart.FileName() == "" {
		_ = r.Body.Close()
		return nil, nil, NewError(http.StatusUnprocessableEntity, "validation", "second multipart part must be a file")
	}
	return filePart, reader, nil
}

func decodeBoundedJSON(r io.Reader, limit int64, out any) error {
	data, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return err
	}
	if int64(len(data)) > limit {
		return errors.New("JSON body exceeds limit")
	}
	return json.Unmarshal(data, out, json.RejectUnknownMembers(true))
}

func toRemoteRecording(body MediaReferenceBody) processing.RemoteRecordingRequest {
	return processing.RemoteRecordingRequest{OperationID: body.OperationID, ReferenceURL: body.ReferenceURL,
		ProviderHint: body.ProviderHint, CredentialBinding: body.CredentialBinding, Acquire: body.Acquire,
		Occurrence: toMediaOccurrence(body.Occurrence), Processing: toOptionalMediaProcessing(body.Processing)}
}

func toMediaOccurrence(value MediaOccurrenceBody) processing.MediaOccurrenceInput {
	return processing.MediaOccurrenceInput{Ref: value.Ref, Revision: value.Revision, Filename: value.Filename,
		PersonRef: value.PersonRef, SpeakerLabel: value.SpeakerLabel,
		Message: processing.MediaTimestamp{Normalized: value.Message.Normalized, Raw: value.Message.Raw,
			Precision: value.Message.Precision, Timezone: value.Message.Timezone, ZoneText: value.Message.ZoneText,
			OffsetSeconds: value.Message.OffsetSeconds, FractionDigits: value.Message.FractionDigits}}
}

func toMediaProcessing(value MediaProcessingBody) processing.MediaProcessingRequest {
	return processing.MediaProcessingRequest{Profile: value.Profile, SuppliedInputID: value.SuppliedInputID}
}

func toOptionalMediaProcessing(value *MediaProcessingBody) *processing.MediaProcessingRequest {
	if value == nil {
		return nil
	}
	result := toMediaProcessing(*value)
	return &result
}

func fromMediaReceipt(value processing.MediaReceipt) MediaReceipt {
	result := MediaReceipt{VaultUID: value.VaultUID, SourceID: value.SourceID,
		SourceVersionID: value.SourceVersionID, ContentVersionID: value.ContentVersionID,
		OccurrenceID: value.OccurrenceID, OperationID: value.OperationID, JobID: value.JobID,
		Outcome: value.Outcome, OperationState: value.OperationState, CoverageState: value.CoverageState}
	result.SuppliedInputID = value.SuppliedInputID
	return result
}

func fromMediaSourcePage(value processing.MediaSourcePage) MediaSourcePage {
	result := MediaSourcePage{Items: make([]MediaSourceRow, len(value.Items)), Total: value.Total, NextCursor: value.NextCursor}
	for index, item := range value.Items {
		result.Items[index] = MediaSourceRow{SourceID: item.SourceID, SourceVersionID: item.SourceVersionID,
			ContentVersionID: item.ContentVersionID, Filename: item.Filename, CaptureLabel: item.CaptureLabel,
			Outcome: item.Outcome, CoverageState: item.CoverageState, Excerpt: item.Excerpt}
	}
	return result
}

func fromMediaOccurrencePage(value processing.MediaOccurrencePage) MediaOccurrencePage {
	result := MediaOccurrencePage{Items: make([]MediaOccurrenceRow, len(value.Items)), Total: value.Total, NextCursor: value.NextCursor}
	for index, item := range value.Items {
		result.Items[index] = MediaOccurrenceRow{OccurrenceID: item.OccurrenceID, SourceID: item.SourceID,
			SourceVersionID: item.SourceVersionID, Ref: item.Ref, Revision: item.Revision,
			Filename: item.Filename, PersonRef: item.PersonRef, SpeakerLabel: item.SpeakerLabel,
			Message: MediaTimestamp{Normalized: item.Message.Normalized, Raw: item.Message.Raw,
				Precision: item.Message.Precision, Timezone: item.Message.Timezone,
				ZoneText: item.Message.ZoneText, OffsetSeconds: item.Message.OffsetSeconds,
				FractionDigits: item.Message.FractionDigits}}
	}
	return result
}

func fromMediaConsentReceipt(value store.MediaConsentReceipt) MediaConsentReceipt {
	return MediaConsentReceipt{OperationID: value.OperationID, OriginID: value.OriginID,
		GrantID: value.GrantID, Fence: value.Fence, RevokedAt: value.RevokedAt}
}

func mediaUnavailable() *Error {
	return NewError(http.StatusServiceUnavailable, "capability_unavailable", "media capability is unavailable")
}

func fromMediaError(err error) *Error {
	if err == nil {
		return nil
	}
	for _, item := range []struct {
		target error
		status int
		code   string
	}{
		{processing.ErrMediaCapabilityUnavailable, http.StatusServiceUnavailable, "capability_unavailable"},
		{processing.ErrMediaProcessingUnsupported, http.StatusUnprocessableEntity, "media_processing_unsupported"},
		{processing.ErrMediaCursorInvalid, http.StatusUnprocessableEntity, "invalid_cursor"},
		{processing.ErrMediaPlanInvalid, http.StatusUnprocessableEntity, "invalid_media_plan"},
		{processing.ErrMediaPlanExpired, http.StatusConflict, "expired_media_plan"},
		{store.ErrMediaOperationConflict, http.StatusConflict, "operation_conflict"},
		{store.ErrMediaOccurrenceConflict, http.StatusConflict, "occurrence_conflict"},
		{store.ErrMediaSourceConflict, http.StatusConflict, "source_conflict"},
	} {
		if errors.Is(err, item.target) {
			return NewError(item.status, item.code, err.Error())
		}
	}
	if problem, ok := errors.AsType[*Error](fromProcessingError(err)); ok && problem.Status < http.StatusInternalServerError {
		return problem
	}
	if strings.Contains(err.Error(), "byte_limit") {
		return NewError(http.StatusRequestEntityTooLarge, "byte_limit", "media bytes exceed configured limit")
	}
	if strings.Contains(err.Error(), "invalid") || strings.Contains(err.Error(), "requires") ||
		strings.Contains(err.Error(), "must") || strings.Contains(err.Error(), "partial_download") ||
		strings.Contains(err.Error(), "digest_mismatch") {
		return NewError(http.StatusUnprocessableEntity, "validation", err.Error())
	}
	return NewError(http.StatusInternalServerError, "media_failed", "media operation failed")
}

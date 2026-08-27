package geminiembed

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"image/color"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/media"
	"go.kenn.io/docbank/document/media/mediatest"
)

const (
	testCapabilityProfile = "dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"
	testDisclosurePolicy  = "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"
)

// TestEmbedInlineSendsOnlyCapabilityProvenExactBytes catches inline requests
// that serialize caller metadata or bytes not covered by the locally sealed
// capability record.
func TestEmbedInlineSendsOnlyCapabilityProvenExactBytes(t *testing.T) {
	data := geminiTinyPNG(t)
	profile := geminiDirectTestProfile(t, TransportInline)
	record := geminiCapability(t, profile, data, "synthetic.png", "image/png")
	source := newGeminiLifecycleUpload(data, record)
	var requests atomic.Int32
	client := newGeminiTestClient(t, profile, syntheticSecrets{"secret:gemini": "synthetic-key"}, roundTripFunc(func(request *http.Request) (*http.Response, error) {
		requests.Add(1)
		require.Equal(t, http.MethodPost, request.Method)
		require.Equal(t, embedPath, request.URL.Path)
		body, err := io.ReadAll(request.Body)
		require.NoError(t, err)
		want := `{"model":"models/gemini-embedding-2","content":{"parts":[{"inlineData":{"mimeType":"image/png","data":"` + base64.StdEncoding.EncodeToString(data) + `"}}]},"outputDimensionality":128}`
		assert.Equal(t, want, string(body))
		return geminiJSONResponse(request, `{"embedding":{"values":[`+testVectorJSON(128)+`]}}`), nil
	}))

	result, err := client.Embed(t.Context(), directInputs(source), geminiDirectAuthorization(profile.Descriptor, int64(len(data))))
	require.NoError(t, err)
	require.Len(t, result.Vectors, 1)
	assert.Equal(t, "direct-a", result.Vectors[0].Key)
	assert.Equal(t, int32(1), requests.Load())
	assert.Equal(t, int32(1), source.readPasses.Load())
	assert.Equal(t, int32(1), source.closeCalls.Load())

	t.Run("changed bytes fail before credentials", func(t *testing.T) {
		changed := append([]byte(nil), data...)
		changed[len(changed)-1] ^= 0xff
		changedSource := newGeminiLifecycleUpload(changed, record)
		secrets := &countingSecrets{value: "synthetic-key"}
		client := newGeminiTestClient(t, profile, secrets, roundTripFunc(func(*http.Request) (*http.Response, error) {
			t.Fatal("changed bytes reached egress")
			return nil, errors.New("unreachable changed-byte egress")
		}))
		_, err := client.Embed(t.Context(), directInputs(changedSource), geminiDirectAuthorization(profile.Descriptor, int64(len(data))))
		require.Error(t, err)
		assert.Zero(t, secrets.calls.Load())
		assert.Equal(t, int32(1), changedSource.closeCalls.Load())
	})
}

// TestEmbedFilesAPIUploadsActivatesEmbedsAndDeletes catches any lifecycle
// reordering, resumable-upload contract drift, premature embed, or omitted
// deletion attempt.
func TestEmbedFilesAPIUploadsActivatesEmbedsAndDeletes(t *testing.T) {
	data := geminiTinyPNG(t)
	profile := geminiDirectTestProfile(t, TransportFilesAPI)
	record := geminiCapability(t, profile, data, "synthetic.png", "image/png")
	source := newGeminiLifecycleUpload(data, record)
	fileName := "files/file-123"
	fileURI := origin + "/v1beta/" + fileName
	timeline := newGeminiFileTimeline()
	processing := geminiFileJSON(record, fileName, fileURI, "PROCESSING", timeline)
	active := geminiFileJSON(record, fileName, fileURI, "ACTIVE", timeline)
	sequence := atomic.Int32{}
	client := newGeminiTestClient(t, profile, syntheticSecrets{"secret:gemini": "synthetic-key"}, roundTripFunc(func(request *http.Request) (*http.Response, error) {
		step := sequence.Add(1)
		assert.Equal(t, "synthetic-key", request.Header.Get("X-Goog-Api-Key"))
		switch step {
		case 1:
			require.Equal(t, http.MethodPost, request.Method)
			assert.Equal(t, filesUploadPath, request.URL.Path)
			assert.Equal(t, "resumable", request.Header.Get("X-Goog-Upload-Protocol"))
			assert.Equal(t, "start", request.Header.Get("X-Goog-Upload-Command"))
			assert.Equal(t, strconv.Itoa(len(data)), request.Header.Get("X-Goog-Upload-Header-Content-Length"))
			assert.Equal(t, "image/png", request.Header.Get("X-Goog-Upload-Header-Content-Type"))
			body, err := io.ReadAll(request.Body)
			require.NoError(t, err)
			assert.JSONEq(t, `{"file":{}}`, string(body))
			response := &http.Response{StatusCode: http.StatusOK, Header: make(http.Header),
				Body: io.NopCloser(strings.NewReader("")), Request: request}
			response.Header.Set("X-Goog-Upload-Url", origin+"/upload/v1beta/files?upload_id=synthetic-session-123&upload_protocol=resumable")
			return response, nil
		case 2:
			require.Equal(t, http.MethodPost, request.Method)
			assert.Equal(t, "/upload/v1beta/files", request.URL.Path)
			assert.Equal(t, "upload_id=synthetic-session-123&upload_protocol=resumable", request.URL.RawQuery)
			assert.Equal(t, "upload, finalize", request.Header.Get("X-Goog-Upload-Command"))
			assert.Equal(t, "0", request.Header.Get("X-Goog-Upload-Offset"))
			assert.Equal(t, int64(len(data)), request.ContentLength)
			body, err := io.ReadAll(request.Body)
			require.NoError(t, err)
			assert.Equal(t, data, body)
			response := geminiJSONResponse(request, `{"file":`+processing+`}`)
			response.Header.Set("X-Goog-Upload-Status", "final")
			return response, nil
		case 3:
			require.Equal(t, http.MethodGet, request.Method)
			assert.Equal(t, "/v1beta/"+fileName, request.URL.Path)
			return geminiJSONResponse(request, processing), nil
		case 4:
			require.Equal(t, http.MethodGet, request.Method)
			assert.Equal(t, "/v1beta/"+fileName, request.URL.Path)
			return geminiJSONResponse(request, active), nil
		case 5:
			require.Equal(t, http.MethodPost, request.Method)
			assert.Equal(t, embedPath, request.URL.Path)
			body, err := io.ReadAll(request.Body)
			require.NoError(t, err)
			want := `{"model":"models/gemini-embedding-2","content":{"parts":[{"fileData":{"mimeType":"image/png","fileUri":"` + fileURI + `"}}]},"outputDimensionality":128}`
			assert.Equal(t, want, string(body))
			return geminiJSONResponse(request, `{"embedding":{"values":[`+testVectorJSON(128)+`]}}`), nil
		case 6:
			require.Equal(t, http.MethodDelete, request.Method)
			assert.Equal(t, "/v1beta/"+fileName, request.URL.Path)
			return geminiJSONResponse(request, `{}`), nil
		default:
			t.Fatalf("unexpected request %d: %s %s", step, request.Method, request.URL)
			return nil, errors.New("unreachable lifecycle request")
		}
	}))

	execution, err := client.EmbedWithReceipt(t.Context(), directInputs(source), geminiDirectAuthorization(profile.Descriptor, int64(len(data))))
	require.NoError(t, err)
	assert.Equal(t, int32(6), sequence.Load())
	assert.Equal(t, 6, execution.Receipt.RequestCount)
	assert.Equal(t, 48*time.Hour, execution.Receipt.ProviderRetentionCeiling)
	assert.Equal(t, int32(1), source.readPasses.Load())
	assert.Equal(t, int32(1), source.closeCalls.Load())
	assert.NotContains(t, execution.Receipt.ProviderResponseIDs, fileName)
}

// TestEmbedFilesAPIDeletesAfterValidFinalizeReceiptRejection catches cleanup
// ownership that is installed only after receipt metadata has been accepted.
func TestEmbedFilesAPIDeletesAfterValidFinalizeReceiptRejection(t *testing.T) {
	data := geminiTinyPNG(t)
	profile := geminiDirectTestProfile(t, TransportFilesAPI)
	record := geminiCapability(t, profile, data, "synthetic.png", "image/png")
	source := newGeminiLifecycleUpload(data, record)
	fileName := "files/file-123"
	fileURI := origin + "/v1beta/" + fileName
	active := geminiFileJSON(record, fileName, fileURI, "ACTIVE", newGeminiFileTimeline())
	var sequence atomic.Int32
	client := newGeminiTestClient(t, profile, syntheticSecrets{"secret:gemini": "synthetic-key"}, roundTripFunc(func(request *http.Request) (*http.Response, error) {
		switch sequence.Add(1) {
		case 1:
			response := geminiJSONResponse(request, `{}`)
			response.Header.Set("X-Goog-Upload-Url", origin+"/upload/v1beta/files?upload_id=synthetic-session-123&upload_protocol=resumable")
			return response, nil
		case 2:
			response := geminiJSONResponse(request, `{"file":`+active+`}`)
			response.Header.Set("X-Goog-Upload-Status", "final")
			response.Header.Set("X-Goog-Request-Id", "invalid response id")
			return response, nil
		case 3:
			require.Equal(t, http.MethodDelete, request.Method)
			assert.Equal(t, "/v1beta/"+fileName, request.URL.Path)
			return geminiJSONResponse(request, `{}`), nil
		default:
			return nil, errors.New("unexpected cleanup lifecycle request")
		}
	}))
	receipt := Receipt{}
	_, err := client.embed(t.Context(), directInputs(source), geminiDirectAuthorization(profile.Descriptor, int64(len(data))), &receipt)
	require.ErrorIs(t, err, ErrPermanentResponse)
	assert.Equal(t, int32(3), sequence.Load())
	assert.Equal(t, 3, receipt.RequestCount)
	assert.Empty(t, receipt.ProviderResponseIDs)
	assert.NotContains(t, err.Error(), fileName)
	assert.NotContains(t, err.Error(), "invalid response id")
	assert.Equal(t, int32(1), source.closeCalls.Load())
}

// TestEmbedFilesAPITruncatesCapacityExceedingResponseID catches receipt
// capacity handling that discards successful embedding work or retains the ID.
func TestEmbedFilesAPITruncatesCapacityExceedingResponseID(t *testing.T) {
	data := geminiTinyPNG(t)
	profile := geminiDirectTestProfile(t, TransportFilesAPI)
	record := geminiCapability(t, profile, data, "synthetic.png", "image/png")
	source := newGeminiLifecycleUpload(data, record)
	fileName := "files/file-123"
	fileURI := origin + "/v1beta/" + fileName
	active := geminiFileJSON(record, fileName, fileURI, "ACTIVE", newGeminiFileTimeline())
	var sequence atomic.Int32
	client := newGeminiTestClient(t, profile, syntheticSecrets{"secret:gemini": "synthetic-key"}, roundTripFunc(func(request *http.Request) (*http.Response, error) {
		switch sequence.Add(1) {
		case 1:
			response := geminiJSONResponse(request, `{}`)
			response.Header.Set("X-Goog-Upload-Url", origin+"/upload/v1beta/files?upload_id=synthetic-session-123&upload_protocol=resumable")
			return response, nil
		case 2:
			response := geminiJSONResponse(request, `{"file":`+active+`}`)
			response.Header.Set("X-Goog-Upload-Status", "final")
			response.Header.Set("X-Goog-Request-Id", "provider-response-over-capacity")
			return response, nil
		case 3:
			require.Equal(t, http.MethodPost, request.Method)
			return geminiJSONResponse(request, `{"embedding":{"values":[`+testVectorJSON(128)+`]}}`), nil
		case 4:
			require.Equal(t, http.MethodDelete, request.Method)
			return geminiJSONResponse(request, `{}`), nil
		default:
			return nil, errors.New("unexpected response-ID capacity request")
		}
	}))
	receipt := Receipt{ProviderResponseIDs: make([]string, 128)}
	for index := range receipt.ProviderResponseIDs {
		receipt.ProviderResponseIDs[index] = "existing-response-" + strconv.Itoa(index)
	}
	result, err := client.embed(t.Context(), directInputs(source), geminiDirectAuthorization(profile.Descriptor, int64(len(data))), &receipt)
	require.NoError(t, err)
	assert.Len(t, result.Vectors, 1)
	assert.Equal(t, int32(4), sequence.Load())
	assert.Equal(t, 4, receipt.RequestCount)
	assert.Len(t, receipt.ProviderResponseIDs, 128)
	assert.Equal(t, 1, receipt.OmittedProviderResponseIDs)
	assert.NotContains(t, receipt.ProviderResponseIDs, "provider-response-over-capacity")
	assert.Equal(t, int32(1), source.closeCalls.Load())
}

// TestEmbedFilesAPIRejectsUntrustedLifecycleTimestamps catches provider file
// state that escapes the local finalize window or changes its sealed timeline.
func TestEmbedFilesAPIRejectsUntrustedLifecycleTimestamps(t *testing.T) {
	data := geminiTinyPNG(t)
	profile := geminiDirectTestProfile(t, TransportFilesAPI)
	record := geminiCapability(t, profile, data, "synthetic.png", "image/png")
	fileName := "files/file-123"
	fileURI := origin + "/v1beta/" + fileName
	valid := newGeminiFileTimeline()

	for _, testCase := range []struct {
		name         string
		finalize     geminiFileTimeline
		poll         *geminiFileTimeline
		wantRequests int32
	}{
		{name: "creation outside local finalize window", finalize: geminiFileTimeline{
			created: valid.created.Add(6 * time.Minute), updated: valid.updated.Add(6 * time.Minute), expires: valid.expires.Add(6 * time.Minute),
		}, wantRequests: 2},
		{name: "finalize update after expiration", finalize: geminiFileTimeline{
			created: valid.created, updated: valid.expires.Add(time.Second), expires: valid.expires,
		}, wantRequests: 2},
		{name: "poll creation drift", finalize: valid, poll: &geminiFileTimeline{
			created: valid.created.Add(time.Second), updated: valid.updated.Add(time.Second), expires: valid.expires,
		}, wantRequests: 4},
		{name: "poll expiration drift", finalize: valid, poll: &geminiFileTimeline{
			created: valid.created, updated: valid.updated.Add(time.Second), expires: valid.expires.Add(-time.Second),
		}, wantRequests: 4},
		{name: "poll update regression", finalize: valid, poll: &geminiFileTimeline{
			created: valid.created, updated: valid.created, expires: valid.expires,
		}, wantRequests: 4},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			source := newGeminiLifecycleUpload(data, record)
			finalizeState := "ACTIVE"
			if testCase.poll != nil {
				finalizeState = "PROCESSING"
			}
			finalized := geminiFileJSON(record, fileName, fileURI, finalizeState, testCase.finalize)
			var polled string
			if testCase.poll != nil {
				polled = geminiFileJSON(record, fileName, fileURI, "ACTIVE", *testCase.poll)
			}
			var sequence atomic.Int32
			client := newGeminiTestClient(t, profile, syntheticSecrets{"secret:gemini": "synthetic-key"}, roundTripFunc(func(request *http.Request) (*http.Response, error) {
				switch sequence.Add(1) {
				case 1:
					response := geminiJSONResponse(request, `{}`)
					response.Header.Set("X-Goog-Upload-Url", origin+"/upload/v1beta/files?upload_id=synthetic-session-123&upload_protocol=resumable")
					return response, nil
				case 2:
					response := geminiJSONResponse(request, `{"file":`+finalized+`}`)
					response.Header.Set("X-Goog-Upload-Status", "final")
					return response, nil
				case 3:
					require.NotNil(t, testCase.poll)
					require.Equal(t, http.MethodGet, request.Method)
					return geminiJSONResponse(request, polled), nil
				case 4:
					require.Equal(t, http.MethodDelete, request.Method)
					return geminiJSONResponse(request, `{}`), nil
				default:
					return nil, errors.New("unsafe timestamp follow-up request")
				}
			}))
			receipt := Receipt{}
			_, err := client.embed(t.Context(), directInputs(source), geminiDirectAuthorization(profile.Descriptor, int64(len(data))), &receipt)
			require.ErrorIs(t, err, ErrPermanentResponse)
			assert.Equal(t, testCase.wantRequests, sequence.Load())
			assert.Equal(t, int(testCase.wantRequests), receipt.RequestCount)
			assert.Empty(t, receipt.ProviderResponseIDs)
			assert.NotContains(t, err.Error(), fileName)
			assert.NotContains(t, err.Error(), fileURI)
			assert.Equal(t, int32(1), source.closeCalls.Load())
		})
	}
}

func TestValidateCreatedWireFileBoundsUpdateAndExpiryToObservation(t *testing.T) {
	data := geminiTinyPNG(t)
	profile := geminiDirectTestProfile(t, TransportFilesAPI)
	record := geminiCapability(t, profile, data, "synthetic.png", "image/png")
	expected := verifiedFile{metadata: document.AuthorizedUploadMetadata{
		Filename: "synthetic.png", MediaType: record.MediaType, ByteLength: record.SourceBytes, SHA256: record.SourceSHA256,
	}}
	startedAt := time.Now().UTC().Truncate(time.Second)
	completedAt := startedAt.Add(time.Second)
	file := func(updated, expires time.Time) wireFile {
		return wireFile{
			Name: "files/file-123", MIMEType: record.MediaType, SizeBytes: strconv.FormatInt(record.SourceBytes, 10),
			CreateTime: startedAt.Format(time.RFC3339Nano), UpdateTime: updated.Format(time.RFC3339Nano),
			ExpirationTime: expires.Format(time.RFC3339Nano), SHA256Hash: base64.StdEncoding.EncodeToString([]byte(record.SourceSHA256)),
			URI: origin + "/v1beta/files/file-123", State: "ACTIVE", Source: "UPLOADED",
		}
	}

	_, ok := validateCreatedWireFile(file(completedAt.Add(fileClockSkew+time.Second), startedAt.Add(48*time.Hour)), expected, startedAt, completedAt)
	assert.False(t, ok, "an update outside the finalize observation window must fail closed")
	_, ok = validateCreatedWireFile(file(startedAt, completedAt), expected, startedAt, completedAt)
	assert.False(t, ok, "a file already expired at finalize completion must fail closed")
}

func TestValidatePolledWireFileBoundsUpdateAndExpiryToObservation(t *testing.T) {
	data := geminiTinyPNG(t)
	profile := geminiDirectTestProfile(t, TransportFilesAPI)
	record := geminiCapability(t, profile, data, "synthetic.png", "image/png")
	expected := verifiedFile{metadata: document.AuthorizedUploadMetadata{
		Filename: "synthetic.png", MediaType: record.MediaType, ByteLength: record.SourceBytes, SHA256: record.SourceSHA256,
	}}
	startedAt := time.Now().UTC().Truncate(time.Second)
	completedAt := startedAt.Add(time.Second)
	createdAt := startedAt.Add(-time.Hour)
	file := func(updated, expires time.Time) wireFile {
		return wireFile{
			Name: "files/file-123", MIMEType: record.MediaType, SizeBytes: strconv.FormatInt(record.SourceBytes, 10),
			CreateTime: createdAt.Format(time.RFC3339Nano), UpdateTime: updated.Format(time.RFC3339Nano),
			ExpirationTime: expires.Format(time.RFC3339Nano), SHA256Hash: base64.StdEncoding.EncodeToString([]byte(record.SourceSHA256)),
			URI: origin + "/v1beta/files/file-123", State: "PROCESSING", Source: "UPLOADED",
		}
	}

	staleUpdate := startedAt.Add(-fileClockSkew - time.Second)
	futureExpiry := startedAt.Add(24 * time.Hour)
	current := validatedProviderFile{file: file(staleUpdate, futureExpiry), created: createdAt, updated: staleUpdate, expiresAt: futureExpiry}
	_, ok := validatePolledWireFile(file(staleUpdate, futureExpiry), expected, current, startedAt, completedAt)
	assert.False(t, ok, "a stale poll update outside the observation window must fail closed")

	current.updated = startedAt
	current.file = file(startedAt, futureExpiry)
	_, ok = validatePolledWireFile(file(completedAt.Add(fileClockSkew+time.Second), futureExpiry), expected, current, startedAt, completedAt)
	assert.False(t, ok, "a future poll update outside the observation window must fail closed")

	expiredAt := completedAt
	current = validatedProviderFile{file: file(startedAt, expiredAt), created: createdAt, updated: startedAt, expiresAt: expiredAt}
	_, ok = validatePolledWireFile(file(startedAt, expiredAt), expected, current, startedAt, completedAt)
	assert.False(t, ok, "a file already expired at poll completion must fail closed")
}

// TestEmbedFilesAPIRejectsUnsafeUploadURLsWithoutFollowUp catches resumable
// destinations that escape the exact sealed Google upload boundary.
func TestEmbedFilesAPIRejectsUnsafeUploadURLsWithoutFollowUp(t *testing.T) {
	for _, testCase := range []struct {
		name      string
		uploadURL string
	}{
		{name: "host", uploadURL: "https://provider.invalid/upload/v1beta/files?upload_id=synthetic&upload_protocol=resumable"},
		{name: "userinfo", uploadURL: "https://synthetic@generativelanguage.googleapis.com/upload/v1beta/files?upload_id=synthetic&upload_protocol=resumable"},
		{name: "port", uploadURL: "https://generativelanguage.googleapis.com:444/upload/v1beta/files?upload_id=synthetic&upload_protocol=resumable"},
		{name: "path", uploadURL: "https://generativelanguage.googleapis.com/upload/v1beta/other?upload_id=synthetic&upload_protocol=resumable"},
		{name: "query", uploadURL: "https://generativelanguage.googleapis.com/upload/v1beta/files?upload_id=synthetic&upload_protocol=raw"},
		{name: "fragment", uploadURL: "https://generativelanguage.googleapis.com/upload/v1beta/files?upload_id=synthetic&upload_protocol=resumable#outside"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			data := geminiTinyPNG(t)
			profile := geminiDirectTestProfile(t, TransportFilesAPI)
			record := geminiCapability(t, profile, data, "synthetic.png", "image/png")
			source := newGeminiLifecycleUpload(data, record)
			var requests atomic.Int32
			client := newGeminiTestClient(t, profile, syntheticSecrets{"secret:gemini": "synthetic-key"}, roundTripFunc(func(request *http.Request) (*http.Response, error) {
				requests.Add(1)
				response := geminiJSONResponse(request, `{}`)
				response.Header.Set("X-Goog-Upload-Url", testCase.uploadURL)
				return response, nil
			}))
			receipt := Receipt{}
			_, err := client.embed(t.Context(), directInputs(source), geminiDirectAuthorization(profile.Descriptor, int64(len(data))), &receipt)
			require.ErrorIs(t, err, ErrPermanentResponse)
			assert.Equal(t, int32(1), requests.Load())
			assert.Equal(t, 1, receipt.RequestCount)
			assert.Empty(t, receipt.ProviderResponseIDs)
			assert.NotContains(t, err.Error(), testCase.uploadURL)
			assert.Equal(t, int32(1), source.closeCalls.Load())
		})
	}
}

// TestEmbedFilesAPIRejectsMismatchedCreatedFileWithoutUnsafeCleanup catches a
// provider-controlled file identity or state that is trusted for follow-up.
func TestEmbedFilesAPIRejectsMismatchedCreatedFileWithoutUnsafeCleanup(t *testing.T) {
	data := geminiTinyPNG(t)
	profile := geminiDirectTestProfile(t, TransportFilesAPI)
	record := geminiCapability(t, profile, data, "synthetic.png", "image/png")
	fileName := "files/file-123"
	fileURI := origin + "/v1beta/" + fileName
	valid := geminiFileJSON(record, fileName, fileURI, "ACTIVE", newGeminiFileTimeline())
	hash := base64.StdEncoding.EncodeToString([]byte(record.SourceSHA256))

	for _, testCase := range []struct {
		name string
		file string
	}{
		{name: "name", file: strings.Replace(valid, `"name":"files/file-123"`, `"name":"files/UPPER"`, 1)},
		{name: "uri", file: strings.Replace(valid, `"uri":"`+fileURI+`"`, `"uri":"https://provider.invalid/v1beta/files/file-123"`, 1)},
		{name: "mime", file: strings.Replace(valid, `"mimeType":"image/png"`, `"mimeType":"image/jpeg"`, 1)},
		{name: "size", file: strings.Replace(valid, `"sizeBytes":"`+strconv.FormatInt(record.SourceBytes, 10)+`"`, `"sizeBytes":"1"`, 1)},
		{name: "hash", file: strings.Replace(valid, `"sha256Hash":"`+hash+`"`, `"sha256Hash":"c3ludGhldGlj"`, 1)},
		{name: "state", file: strings.Replace(valid, `"state":"ACTIVE"`, `"state":"READY"`, 1)},
		{name: "timestamps", file: strings.Replace(valid, `"updateTime":"`, `"updateTime":"not-a-time`, 1)},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			source := newGeminiLifecycleUpload(data, record)
			var sequence atomic.Int32
			client := newGeminiTestClient(t, profile, syntheticSecrets{"secret:gemini": "synthetic-key"}, roundTripFunc(func(request *http.Request) (*http.Response, error) {
				switch sequence.Add(1) {
				case 1:
					response := geminiJSONResponse(request, `{}`)
					response.Header.Set("X-Goog-Upload-Url", origin+"/upload/v1beta/files?upload_id=synthetic-session-123&upload_protocol=resumable")
					return response, nil
				case 2:
					response := geminiJSONResponse(request, `{"file":`+testCase.file+`}`)
					response.Header.Set("X-Goog-Upload-Status", "final")
					return response, nil
				default:
					return nil, errors.New("unsafe created-file follow-up request")
				}
			}))
			receipt := Receipt{}
			_, err := client.embed(t.Context(), directInputs(source), geminiDirectAuthorization(profile.Descriptor, int64(len(data))), &receipt)
			require.ErrorIs(t, err, ErrPermanentResponse)
			assert.Equal(t, int32(2), sequence.Load())
			assert.Equal(t, 2, receipt.RequestCount)
			assert.Empty(t, receipt.ProviderResponseIDs)
			assert.NotContains(t, err.Error(), fileName)
			assert.NotContains(t, err.Error(), fileURI)
			assert.Equal(t, int32(1), source.closeCalls.Load())
		})
	}
}

// TestEmbedFilesAPIBoundsEveryLifecycleBody catches a start, finalize, poll,
// or delete response that bypasses the fixed raw-response byte ceiling.
func TestEmbedFilesAPIBoundsEveryLifecycleBody(t *testing.T) {
	for _, stage := range []string{"start", "finalize", "poll", "delete"} {
		t.Run(stage, func(t *testing.T) {
			data := geminiTinyPNG(t)
			profile := geminiDirectTestProfile(t, TransportFilesAPI)
			record := geminiCapability(t, profile, data, "synthetic.png", "image/png")
			source := newGeminiLifecycleUpload(data, record)
			fileName := "files/file-123"
			fileURI := origin + "/v1beta/" + fileName
			state := "ACTIVE"
			if stage == "poll" {
				state = "PROCESSING"
			}
			fileJSON := geminiFileJSON(record, fileName, fileURI, state, newGeminiFileTimeline())
			oversized := strings.Repeat("x", int(profile.MaxResponseBytes)+1)
			var sequence atomic.Int32
			client := newGeminiTestClient(t, profile, syntheticSecrets{"secret:gemini": "synthetic-key"}, roundTripFunc(func(request *http.Request) (*http.Response, error) {
				switch sequence.Add(1) {
				case 1:
					if stage == "start" {
						return geminiJSONResponse(request, oversized), nil
					}
					response := geminiJSONResponse(request, `{}`)
					response.Header.Set("X-Goog-Upload-Url", origin+"/upload/v1beta/files?upload_id=synthetic-session-123&upload_protocol=resumable")
					return response, nil
				case 2:
					responseBody := `{"file":` + fileJSON + `}`
					if stage == "finalize" {
						responseBody = oversized
					}
					response := geminiJSONResponse(request, responseBody)
					response.Header.Set("X-Goog-Upload-Status", "final")
					return response, nil
				case 3:
					if stage == "poll" {
						return geminiJSONResponse(request, oversized), nil
					}
					require.Equal(t, embedPath, request.URL.Path)
					return geminiJSONResponse(request, `{"embedding":{"values":[`+testVectorJSON(128)+`]}}`), nil
				case 4:
					require.Equal(t, http.MethodDelete, request.Method)
					if stage == "delete" {
						return geminiJSONResponse(request, oversized), nil
					}
					return geminiJSONResponse(request, `{}`), nil
				default:
					return nil, errors.New("oversized lifecycle response triggered extra egress")
				}
			}))
			receipt := Receipt{}
			result, err := client.embed(t.Context(), directInputs(source), geminiDirectAuthorization(profile.Descriptor, int64(len(data))), &receipt)
			wantRequests := map[string]int32{"start": 1, "finalize": 2, "poll": 4, "delete": 4}[stage]
			assert.Equal(t, wantRequests, sequence.Load())
			assert.Equal(t, int(wantRequests), receipt.RequestCount)
			assert.Empty(t, receipt.ProviderResponseIDs)
			if stage == "delete" {
				require.NoError(t, err)
				require.Len(t, result.Vectors, 1)
				assert.Equal(t, []string{"provider file deletion attempt failed"}, receipt.Warnings)
			} else {
				require.Error(t, err)
				assert.Empty(t, receipt.Warnings)
				assert.NotContains(t, err.Error(), fileName)
				assert.NotContains(t, err.Error(), fileURI)
				assert.NotContains(t, err.Error(), oversized)
			}
			assert.Equal(t, int32(1), source.closeCalls.Load())
		})
	}
}

// TestEmbedFilesAPIAttemptsCleanupAfterPostFinalizeFailures catches poll,
// embed, receipt, or delete failures that lose cleanup or request accounting.
func TestEmbedFilesAPIAttemptsCleanupAfterPostFinalizeFailures(t *testing.T) {
	for _, failure := range []string{"poll", "poll receipt", "embed", "embed receipt", "delete"} {
		t.Run(failure, func(t *testing.T) {
			data := geminiTinyPNG(t)
			profile := geminiDirectTestProfile(t, TransportFilesAPI)
			record := geminiCapability(t, profile, data, "synthetic.png", "image/png")
			source := newGeminiLifecycleUpload(data, record)
			fileName := "files/file-123"
			fileURI := origin + "/v1beta/" + fileName
			timeline := newGeminiFileTimeline()
			state := "ACTIVE"
			if strings.HasPrefix(failure, "poll") {
				state = "PROCESSING"
			}
			finalized := geminiFileJSON(record, fileName, fileURI, state, timeline)
			active := geminiFileJSON(record, fileName, fileURI, "ACTIVE", timeline)
			var sequence atomic.Int32
			client := newGeminiTestClient(t, profile, syntheticSecrets{"secret:gemini": "synthetic-key"}, roundTripFunc(func(request *http.Request) (*http.Response, error) {
				switch sequence.Add(1) {
				case 1:
					response := geminiJSONResponse(request, `{}`)
					response.Header.Set("X-Goog-Upload-Url", origin+"/upload/v1beta/files?upload_id=synthetic-session-123&upload_protocol=resumable")
					return response, nil
				case 2:
					response := geminiJSONResponse(request, `{"file":`+finalized+`}`)
					response.Header.Set("X-Goog-Upload-Status", "final")
					return response, nil
				case 3:
					switch failure {
					case "poll":
						return &http.Response{StatusCode: http.StatusInternalServerError, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("")), Request: request}, nil
					case "poll receipt":
						response := geminiJSONResponse(request, active)
						response.Header.Set("X-Goog-Request-Id", "invalid response id")
						return response, nil
					case "embed":
						return &http.Response{StatusCode: http.StatusInternalServerError, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("")), Request: request}, nil
					case "embed receipt":
						response := geminiJSONResponse(request, `{"embedding":{"values":[`+testVectorJSON(128)+`]}}`)
						response.Header.Set("X-Goog-Request-Id", "invalid response id")
						return response, nil
					case "delete":
						return geminiJSONResponse(request, `{"embedding":{"values":[`+testVectorJSON(128)+`]}}`), nil
					default:
						return nil, errors.New("unreachable post-finalize failure")
					}
				case 4:
					require.Equal(t, http.MethodDelete, request.Method)
					assert.Equal(t, "/v1beta/"+fileName, request.URL.Path)
					if failure == "delete" {
						return &http.Response{StatusCode: http.StatusInternalServerError, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("")), Request: request}, nil
					}
					return geminiJSONResponse(request, `{}`), nil
				default:
					return nil, errors.New("post-finalize failure triggered extra egress")
				}
			}))
			receipt := Receipt{}
			result, err := client.embed(t.Context(), directInputs(source), geminiDirectAuthorization(profile.Descriptor, int64(len(data))), &receipt)
			assert.Equal(t, int32(4), sequence.Load())
			assert.Equal(t, 4, receipt.RequestCount)
			assert.Empty(t, receipt.ProviderResponseIDs)
			if failure == "delete" {
				require.NoError(t, err)
				require.Len(t, result.Vectors, 1)
				assert.Equal(t, []string{"provider file deletion attempt failed"}, receipt.Warnings)
			} else {
				require.Error(t, err)
				assert.Empty(t, receipt.Warnings)
				assert.NotContains(t, err.Error(), fileName)
				assert.NotContains(t, err.Error(), fileURI)
				assert.NotContains(t, err.Error(), "invalid response id")
			}
			assert.Equal(t, int32(1), source.closeCalls.Load())
		})
	}
}

// TestEmbedRejectsUnsupportedOrOverLimitFilesBeforeCredentialResolution
// catches a local capability or E1 bound bypass that exposes a credential for
// a file the fixed Gemini profile cannot send.
func TestEmbedRejectsUnsupportedOrOverLimitFilesBeforeCredentialResolution(t *testing.T) {
	profile := geminiDirectTestProfile(t, TransportInline)
	image := geminiTinyPNG(t)
	imageRecord := geminiCapability(t, profile, image, "synthetic.png", "image/png")
	text := []byte("synthetic private text")
	textRecord := geminiCapability(t, profile, text, "synthetic.txt", "text/plain")

	for _, testCase := range []struct {
		name          string
		input         document.EmbeddingInput
		authorization document.EmbeddingAuthorization
	}{
		{name: "unsupported eligible family", input: directInputs(newGeminiLifecycleUpload(text, textRecord))[0], authorization: geminiDirectAuthorization(profile.Descriptor, int64(len(text)))},
		{name: "over authorized bytes", input: directInputs(newGeminiLifecycleUpload(image, imageRecord))[0], authorization: geminiDirectAuthorization(profile.Descriptor, int64(len(image)-1))},
		{name: "query file", input: document.EmbeddingInput{Key: "query-file", Role: document.EmbeddingRoleQuery, Kind: document.EmbeddingInputOriginalFile, Source: newGeminiLifecycleUpload(image, imageRecord)}, authorization: geminiDirectAuthorization(profile.Descriptor, int64(len(image)))},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			secrets := &countingSecrets{value: "synthetic-key"}
			var requests atomic.Int32
			client := newGeminiTestClient(t, profile, secrets, roundTripFunc(func(*http.Request) (*http.Response, error) {
				requests.Add(1)
				return nil, errors.New("egress must not start")
			}))
			_, err := client.Embed(t.Context(), []document.EmbeddingInput{testCase.input}, testCase.authorization)
			require.Error(t, err)
			assert.Zero(t, secrets.calls.Load())
			assert.Zero(t, requests.Load())
		})
	}
}

// TestEmbedClosesEveryEnrolledSourceExactlyOnceOnAllExitPaths catches source
// leaks and double-closes when one member fails before or after preparation.
func TestEmbedClosesEveryEnrolledSourceExactlyOnceOnAllExitPaths(t *testing.T) {
	data := geminiTinyPNG(t)
	profile := geminiDirectTestProfile(t, TransportInline)
	record := geminiCapability(t, profile, data, "synthetic.png", "image/png")
	for _, testCase := range []struct {
		name   string
		mutate func([]*geminiLifecycleUpload, *document.EmbeddingAuthorization)
	}{
		{name: "invalid authorization", mutate: func(_ []*geminiLifecycleUpload, authorization *document.EmbeddingAuthorization) {
			authorization.ProviderID = "wrong-provider"
		}},
		{name: "live metadata drift", mutate: func(sources []*geminiLifecycleUpload, _ *document.EmbeddingAuthorization) {
			changed := sources[0].metadata[0]
			changed.ProviderMetadataChecksum = strings.Repeat("f", 64)
			sources[0].metadata = append(sources[0].metadata, changed)
		}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			sources := []*geminiLifecycleUpload{newGeminiLifecycleUpload(data, record), newGeminiLifecycleUpload(data, record)}
			authorization := geminiDirectAuthorization(profile.Descriptor, int64(2*len(data)))
			testCase.mutate(sources, &authorization)
			secrets := &countingSecrets{value: "synthetic-key"}
			client := newGeminiTestClient(t, profile, secrets, roundTripFunc(func(*http.Request) (*http.Response, error) {
				return nil, errors.New("request must not run")
			}))
			_, err := client.Embed(t.Context(), directInputs(sources[0], sources[1]), authorization)
			require.Error(t, err)
			for _, source := range sources {
				assert.Equal(t, int32(1), source.closeCalls.Load())
			}
			assert.Zero(t, secrets.calls.Load())
		})
	}

	t.Run("repeated source identity", func(t *testing.T) {
		source := newGeminiLifecycleUpload(data, record)
		secrets := &countingSecrets{value: "synthetic-key"}
		var requests atomic.Int32
		client := newGeminiTestClient(t, profile, secrets, roundTripFunc(func(*http.Request) (*http.Response, error) {
			requests.Add(1)
			return nil, errors.New("request must not run")
		}))
		_, err := client.Embed(t.Context(), directInputs(source, source), geminiDirectAuthorization(profile.Descriptor, int64(2*len(data))))
		require.Error(t, err)
		assert.Equal(t, int32(1), source.closeCalls.Load())
		assert.Zero(t, source.metadataCalls.Load())
		assert.Zero(t, source.capabilityCalls.Load())
		assert.Zero(t, source.readPasses.Load())
		assert.Zero(t, secrets.calls.Load())
		assert.Zero(t, requests.Load())
	})

	t.Run("unsafe source identity", func(t *testing.T) {
		source := newGeminiLifecycleUpload(data, record)
		valueSource := geminiNonComparableUpload{geminiLifecycleUpload: source, identity: []byte("unsafe")}
		secrets := &countingSecrets{value: "synthetic-key"}
		var requests atomic.Int32
		client := newGeminiTestClient(t, profile, secrets, roundTripFunc(func(*http.Request) (*http.Response, error) {
			requests.Add(1)
			return nil, errors.New("request must not run")
		}))
		_, err := client.Embed(t.Context(), directInputs(valueSource), geminiDirectAuthorization(profile.Descriptor, int64(len(data))))
		require.Error(t, err)
		assert.Equal(t, int32(1), source.closeCalls.Load())
		assert.Zero(t, source.metadataCalls.Load())
		assert.Zero(t, source.capabilityCalls.Load())
		assert.Zero(t, source.readPasses.Load())
		assert.Zero(t, secrets.calls.Load())
		assert.Zero(t, requests.Load())
	})

	t.Run("dynamically unsafe source identity", func(t *testing.T) {
		source := newGeminiLifecycleUpload(data, record)
		valueSource := geminiInterfaceIdentityUpload{geminiLifecycleUpload: source, identity: []byte("unsafe")}
		secrets := &countingSecrets{value: "synthetic-key"}
		var requests atomic.Int32
		client := newGeminiTestClient(t, profile, secrets, roundTripFunc(func(*http.Request) (*http.Response, error) {
			requests.Add(1)
			return nil, errors.New("request must not run")
		}))
		_, err := client.Embed(t.Context(), directInputs(valueSource), geminiDirectAuthorization(profile.Descriptor, int64(len(data))))
		require.Error(t, err)
		assert.Equal(t, int32(1), source.closeCalls.Load())
		assert.Zero(t, source.metadataCalls.Load())
		assert.Zero(t, source.capabilityCalls.Load())
		assert.Zero(t, source.readPasses.Load())
		assert.Zero(t, secrets.calls.Load())
		assert.Zero(t, requests.Load())
	})

	t.Run("repeated comparable value identity", func(t *testing.T) {
		source := newGeminiLifecycleUpload(data, record)
		valueSource := geminiValueUpload{geminiLifecycleUpload: source}
		secrets := &countingSecrets{value: "synthetic-key"}
		var requests atomic.Int32
		client := newGeminiTestClient(t, profile, secrets, roundTripFunc(func(*http.Request) (*http.Response, error) {
			requests.Add(1)
			return nil, errors.New("request must not run")
		}))
		_, err := client.Embed(t.Context(), directInputs(valueSource, valueSource), geminiDirectAuthorization(profile.Descriptor, int64(2*len(data))))
		require.Error(t, err)
		assert.Equal(t, int32(1), source.closeCalls.Load())
		assert.Zero(t, source.metadataCalls.Load())
		assert.Zero(t, source.capabilityCalls.Load())
		assert.Zero(t, source.readPasses.Load())
		assert.Zero(t, secrets.calls.Load())
		assert.Zero(t, requests.Load())
	})

	t.Run("repeated source before trailing source", func(t *testing.T) {
		repeated := newGeminiLifecycleUpload(data, record)
		trailing := newGeminiLifecycleUpload(data, record)
		secrets := &countingSecrets{value: "synthetic-key"}
		var requests atomic.Int32
		client := newGeminiTestClient(t, profile, secrets, roundTripFunc(func(*http.Request) (*http.Response, error) {
			requests.Add(1)
			return nil, errors.New("request must not run")
		}))
		_, err := client.Embed(t.Context(), directInputs(repeated, repeated, trailing), geminiDirectAuthorization(profile.Descriptor, int64(3*len(data))))
		require.Error(t, err)
		assert.Equal(t, int32(1), repeated.closeCalls.Load())
		assert.Equal(t, int32(1), trailing.closeCalls.Load())
		assert.Zero(t, repeated.metadataCalls.Load())
		assert.Zero(t, repeated.capabilityCalls.Load())
		assert.Zero(t, trailing.metadataCalls.Load())
		assert.Zero(t, trailing.capabilityCalls.Load())
		assert.Zero(t, repeated.readPasses.Load())
		assert.Zero(t, trailing.readPasses.Load())
		assert.Zero(t, secrets.calls.Load())
		assert.Zero(t, requests.Load())
	})

	t.Run("unsafe source before trailing source", func(t *testing.T) {
		unsafe := newGeminiLifecycleUpload(data, record)
		trailing := newGeminiLifecycleUpload(data, record)
		valueSource := geminiNonComparableUpload{geminiLifecycleUpload: unsafe, identity: []byte("unsafe")}
		secrets := &countingSecrets{value: "synthetic-key"}
		var requests atomic.Int32
		client := newGeminiTestClient(t, profile, secrets, roundTripFunc(func(*http.Request) (*http.Response, error) {
			requests.Add(1)
			return nil, errors.New("request must not run")
		}))
		_, err := client.Embed(t.Context(), directInputs(valueSource, trailing), geminiDirectAuthorization(profile.Descriptor, int64(2*len(data))))
		require.Error(t, err)
		assert.Equal(t, int32(1), unsafe.closeCalls.Load())
		assert.Equal(t, int32(1), trailing.closeCalls.Load())
		assert.Zero(t, unsafe.metadataCalls.Load())
		assert.Zero(t, unsafe.capabilityCalls.Load())
		assert.Zero(t, trailing.metadataCalls.Load())
		assert.Zero(t, trailing.capabilityCalls.Load())
		assert.Zero(t, unsafe.readPasses.Load())
		assert.Zero(t, trailing.readPasses.Load())
		assert.Zero(t, secrets.calls.Load())
		assert.Zero(t, requests.Load())
	})

	for _, testCase := range []struct {
		name   string
		source func() document.AuthorizedUpload
	}{
		{name: "nil source before trailing source", source: func() document.AuthorizedUpload { return nil }},
		{name: "typed nil source before trailing source", source: func() document.AuthorizedUpload {
			var source *geminiLifecycleUpload
			return source
		}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			trailing := newGeminiLifecycleUpload(data, record)
			secrets := &countingSecrets{value: "synthetic-key"}
			var requests atomic.Int32
			client := newGeminiTestClient(t, profile, secrets, roundTripFunc(func(*http.Request) (*http.Response, error) {
				requests.Add(1)
				return nil, errors.New("request must not run")
			}))
			_, err := client.Embed(t.Context(), directInputs(testCase.source(), trailing), geminiDirectAuthorization(profile.Descriptor, int64(2*len(data))))
			require.Error(t, err)
			assert.Equal(t, "gemini embed: original upload source is nil", err.Error())
			assert.Equal(t, int32(1), trailing.closeCalls.Load())
			assert.Zero(t, trailing.metadataCalls.Load())
			assert.Zero(t, trailing.capabilityCalls.Load())
			assert.Zero(t, trailing.readPasses.Load())
			assert.Zero(t, secrets.calls.Load())
			assert.Zero(t, requests.Load())
		})
	}
}

// TestEmbedEnrollsEveryAttachedSourceBeforeSemanticValidation catches an
// ownership scan that filters by declared kind before acquiring close
// responsibility for every non-nil source identity.
func TestEmbedEnrollsEveryAttachedSourceBeforeSemanticValidation(t *testing.T) {
	t.Parallel()
	profile := geminiDirectTestProfile(t, TransportInline)
	data := geminiTinyPNG(t)
	record := geminiCapability(t, profile, data, "synthetic.png", "image/png")
	for _, testCase := range []struct {
		name  string
		input func(document.AuthorizedUpload) []document.EmbeddingInput
	}{
		{
			name: "query text",
			input: func(source document.AuthorizedUpload) []document.EmbeddingInput {
				return []document.EmbeddingInput{{Key: "query", Role: document.EmbeddingRoleQuery,
					Kind: document.EmbeddingInputQueryText, Text: "question", Source: source}}
			},
		},
		{
			name: "rendition chunk",
			input: func(source document.AuthorizedUpload) []document.EmbeddingInput {
				return []document.EmbeddingInput{{Key: "chunk", Role: document.EmbeddingRoleDocument,
					Kind: document.EmbeddingInputRenditionChunk, Text: "passage", Source: source}}
			},
		},
		{
			name: "unknown kind",
			input: func(source document.AuthorizedUpload) []document.EmbeddingInput {
				return []document.EmbeddingInput{{Key: "unknown", Role: document.EmbeddingRoleDocument,
					Kind: document.EmbeddingInputKind("unknown"), Source: source}}
			},
		},
		{
			name: "repeated invalid identity",
			input: func(source document.AuthorizedUpload) []document.EmbeddingInput {
				return []document.EmbeddingInput{
					{Key: "query", Role: document.EmbeddingRoleQuery, Kind: document.EmbeddingInputQueryText, Text: "question", Source: source},
					{Key: "chunk", Role: document.EmbeddingRoleDocument, Kind: document.EmbeddingInputRenditionChunk, Text: "passage", Source: source},
				}
			},
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			source := newGeminiLifecycleUpload(data, record)
			secrets := &countingSecrets{value: "synthetic-key"}
			var requests atomic.Int32
			client := newGeminiTestClient(t, profile, secrets, roundTripFunc(func(*http.Request) (*http.Response, error) {
				requests.Add(1)
				return nil, errors.New("request must not run")
			}))

			_, err := client.Embed(t.Context(), testCase.input(source), geminiDirectAuthorization(profile.Descriptor, int64(len(data))))
			require.Error(t, err)
			assert.Equal(t, int32(1), source.closeCalls.Load())
			assert.Zero(t, source.metadataCalls.Load())
			assert.Zero(t, source.capabilityCalls.Load())
			assert.Zero(t, source.readPasses.Load())
			assert.Zero(t, secrets.calls.Load())
			assert.Zero(t, requests.Load())
		})
	}
}

// TestEmbedClearsEarlierPreparedFileWhenLaterPreparationFails catches cleanup
// ownership that begins only after every input has finished preparation.
func TestEmbedClearsEarlierPreparedFileWhenLaterPreparationFails(t *testing.T) {
	t.Parallel()
	profile := geminiDirectTestProfile(t, TransportInline)
	data := geminiTinyPNG(t)
	record := geminiCapability(t, profile, data, "synthetic.png", "image/png")
	earlier := newGeminiLifecycleUpload(data, record)
	earlier.captureReadBuffer = true
	later := newGeminiLifecycleUpload(data, record)
	drifted := later.metadata[0]
	drifted.ProviderMetadataChecksum = strings.Repeat("f", 64)
	later.metadata = append(later.metadata, drifted)
	secrets := &countingSecrets{value: "synthetic-key"}
	client := newGeminiTestClient(t, profile, secrets, roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("request must not run")
	}))

	_, err := client.Embed(t.Context(), directInputs(earlier, later), geminiDirectAuthorization(profile.Descriptor, int64(2*len(data))))
	require.Error(t, err)
	require.Len(t, earlier.capturedReadBuffer, len(data))
	assert.Equal(t, make([]byte, len(data)), earlier.capturedReadBuffer)
	assert.Zero(t, later.readPasses.Load())
	assert.Zero(t, secrets.calls.Load())
}

// TestEmbedFilesAPIPreflightsFileDataBeforeSecret catches a Files API path
// that discovers its eventual embedding envelope is over the request bound
// only after reading private bytes, resolving credentials, or uploading.
func TestEmbedFilesAPIPreflightsFileDataBeforeSecret(t *testing.T) {
	t.Parallel()
	profile := geminiDirectTestProfile(t, TransportFilesAPI)
	data := geminiTinyPNG(t)
	record := geminiCapability(t, profile, data, "synthetic.png", "image/png")
	source := newGeminiLifecycleUpload(data, record)
	secrets := &countingSecrets{value: "synthetic-key"}
	var requests atomic.Int32
	client := newGeminiTestClient(t, profile, secrets, roundTripFunc(func(*http.Request) (*http.Response, error) {
		requests.Add(1)
		return nil, errors.New("request must not run")
	}))
	minimum, err := minimumFilesAPIRequestCapacity(profile.Descriptor.Dimension)
	require.NoError(t, err)
	client.profile.MaxRequestBytes = minimum - 7 // image/png is six bytes shorter than the longest supported MIME.

	_, err = client.Embed(t.Context(), directInputs(source), geminiDirectAuthorization(profile.Descriptor, int64(len(data))))
	require.Error(t, err)
	assert.Equal(t, int32(1), source.closeCalls.Load())
	assert.Zero(t, source.readPasses.Load())
	assert.Zero(t, secrets.calls.Load())
	assert.Zero(t, requests.Load())
}

// TestEmbedFilesAPIBoundsRawFinalizeBeforeSecret catches a request-wide bound
// that applies to JSON envelopes but not the raw upload/finalize body.
func TestEmbedFilesAPIBoundsRawFinalizeBeforeSecret(t *testing.T) {
	t.Parallel()
	profile := geminiDirectTestProfile(t, TransportFilesAPI)
	minimum, err := minimumFilesAPIRequestCapacity(profile.Descriptor.Dimension)
	require.NoError(t, err)
	profile.MaxRequestBytes = minimum
	profile = rebindGeminiProfile(t, profile)
	data := mediatest.JPEG(64, 64, nil)
	require.Greater(t, int64(len(data)), profile.MaxRequestBytes)
	record := geminiCapability(t, profile, data, "synthetic.jpg", "image/jpeg")
	source := newGeminiLifecycleUpload(data, record)
	secrets := &countingSecrets{value: "synthetic-key"}
	var requests atomic.Int32
	client := newGeminiTestClient(t, profile, secrets, roundTripFunc(func(*http.Request) (*http.Response, error) {
		requests.Add(1)
		return nil, errors.New("request must not run")
	}))

	_, err = client.Embed(t.Context(), directInputs(source), geminiDirectAuthorization(profile.Descriptor, int64(len(data))))
	require.Error(t, err)
	assert.Equal(t, int32(1), source.closeCalls.Load())
	assert.Zero(t, source.readPasses.Load())
	assert.Zero(t, secrets.calls.Load())
	assert.Zero(t, requests.Load())
}

// TestEmbedPreservesEverySupportedDirectMIMEAcrossTransports proves each E14
// direct modality reaches both wire transports with its exact capability MIME.
func TestEmbedPreservesEverySupportedDirectMIMEAcrossTransports(t *testing.T) {
	t.Parallel()
	fixtures := []struct {
		name, filename, mediaType string
		data                      []byte
	}{
		{name: "jpeg", filename: "synthetic.jpg", mediaType: "image/jpeg", data: mediatest.JPEG(2, 2, color.NRGBA{R: 0xff, A: 0xff})},
		{name: "png", filename: "synthetic.png", mediaType: "image/png", data: mediatest.PNG(2, 2, color.NRGBA{G: 0xff, A: 0xff})},
		{name: "wav", filename: "synthetic.wav", mediaType: "audio/wav", data: mediatest.WAV()},
		{name: "mp3", filename: "synthetic.mp3", mediaType: "audio/mpeg", data: mediatest.MP3()},
		{name: "mp4", filename: "synthetic.mp4", mediaType: "video/mp4", data: mediatest.H265MP4()},
		{name: "mov", filename: "synthetic.mov", mediaType: "video/quicktime", data: mediatest.H264MOV()},
		{name: "pdf", filename: "synthetic.pdf", mediaType: "application/pdf", data: mediatest.PDF()},
	}
	for _, fixture := range fixtures {
		t.Run(fixture.name, func(t *testing.T) {
			t.Parallel()
			for _, transport := range []Transport{TransportInline, TransportFilesAPI} {
				t.Run(string(transport), func(t *testing.T) {
					t.Parallel()
					profile := geminiDirectTestProfile(t, transport)
					record := geminiCapability(t, profile, fixture.data, fixture.filename, fixture.mediaType)
					assert.Equal(t, fixture.mediaType, record.MediaType)
					source := newGeminiLifecycleUpload(fixture.data, record)
					fileName := "files/synthetic-" + fixture.name
					fileURI := origin + "/v1beta/" + fileName
					timeline := newGeminiFileTimeline()
					var requests atomic.Int32
					client := newGeminiTestClient(t, profile, syntheticSecrets{"secret:gemini": "synthetic-key"}, roundTripFunc(func(request *http.Request) (*http.Response, error) {
						requestNumber := requests.Add(1)
						if transport == TransportInline {
							require.Equal(t, int32(1), requestNumber)
							body, err := io.ReadAll(request.Body)
							require.NoError(t, err)
							want := `{"model":"models/gemini-embedding-2","content":{"parts":[{"inlineData":{"mimeType":"` + fixture.mediaType + `","data":"` + base64.StdEncoding.EncodeToString(fixture.data) + `"}}]},"outputDimensionality":128}`
							assert.Equal(t, want, string(body))
							return geminiJSONResponse(request, `{"embedding":{"values":[`+testVectorJSON(128)+`]}}`), nil
						}
						switch requestNumber {
						case 1:
							require.Equal(t, http.MethodPost, request.Method)
							assert.Equal(t, fixture.mediaType, request.Header.Get("X-Goog-Upload-Header-Content-Type"))
							response := geminiJSONResponse(request, `{}`)
							response.Header.Set("X-Goog-Upload-Url", origin+"/upload/v1beta/files?upload_id=synthetic-session-123&upload_protocol=resumable")
							return response, nil
						case 2:
							body, err := io.ReadAll(request.Body)
							require.NoError(t, err)
							assert.Equal(t, fixture.data, body)
							response := geminiJSONResponse(request, `{"file":`+geminiFileJSON(record, fileName, fileURI, "ACTIVE", timeline)+`}`)
							response.Header.Set("X-Goog-Upload-Status", "final")
							return response, nil
						case 3:
							body, err := io.ReadAll(request.Body)
							require.NoError(t, err)
							want := `{"model":"models/gemini-embedding-2","content":{"parts":[{"fileData":{"mimeType":"` + fixture.mediaType + `","fileUri":"` + fileURI + `"}}]},"outputDimensionality":128}`
							assert.Equal(t, want, string(body))
							return geminiJSONResponse(request, `{"embedding":{"values":[`+testVectorJSON(128)+`]}}`), nil
						case 4:
							require.Equal(t, http.MethodDelete, request.Method)
							return &http.Response{StatusCode: http.StatusNoContent, Body: http.NoBody, Request: request}, nil
						default:
							return nil, errors.New("unexpected modality wire request")
						}
					}))

					_, err := client.Embed(t.Context(), directInputs(source), geminiDirectAuthorization(profile.Descriptor, int64(len(fixture.data))))
					require.NoError(t, err)
					if transport == TransportInline {
						assert.Equal(t, int32(1), requests.Load())
					} else {
						assert.Equal(t, int32(4), requests.Load())
					}
				})
			}
		})
	}
}

func TestFinalizeFileUploadRejectsRawBodyOverRequestBound(t *testing.T) {
	t.Parallel()
	profile := geminiDirectTestProfile(t, TransportFilesAPI)
	var requests atomic.Int32
	client := newGeminiTestClient(t, profile, syntheticSecrets{"secret:gemini": "synthetic-key"}, roundTripFunc(func(*http.Request) (*http.Response, error) {
		requests.Add(1)
		return nil, errors.New("request must not run")
	}))
	uploadURL, err := validateUploadURL(origin + "/upload/v1beta/files?upload_id=synthetic-session-123&upload_protocol=resumable")
	require.NoError(t, err)
	file := verifiedFile{data: make([]byte, profile.MaxRequestBytes+1)}

	_, _, err = client.finalizeFileUpload(t.Context(), uploadURL, file, "synthetic-key", nil)
	require.Error(t, err)
	assert.Zero(t, requests.Load())
}

// TestEmbedCancellationStopsBlockedReadUploadAndPolling catches cancellation
// paths that leave a source read, upload request, or file-state poll running.
func TestEmbedCancellationStopsBlockedReadUploadAndPolling(t *testing.T) {
	data := geminiTinyPNG(t)

	t.Run("blocked source read", func(t *testing.T) {
		profile := geminiDirectTestProfile(t, TransportInline)
		record := geminiCapability(t, profile, data, "synthetic.png", "image/png")
		source := newGeminiLifecycleUpload(data, record)
		source.blockRead = true
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		result := make(chan error, 1)
		client := newGeminiTestClient(t, profile, &countingSecrets{value: "synthetic-key"}, roundTripFunc(func(*http.Request) (*http.Response, error) {
			return nil, errors.New("request must not run")
		}))
		go func() {
			_, err := client.Embed(ctx, directInputs(source), geminiDirectAuthorization(profile.Descriptor, int64(len(data))))
			result <- err
		}()
		select {
		case <-source.readStarted:
		case <-time.After(time.Second):
			t.Fatal("source read did not start")
		}
		cancel()
		require.ErrorIs(t, <-result, context.Canceled)
		assert.Equal(t, int32(1), source.closeCalls.Load())
	})

	t.Run("blocked upload", func(t *testing.T) {
		profile := geminiDirectTestProfile(t, TransportFilesAPI)
		record := geminiCapability(t, profile, data, "synthetic.png", "image/png")
		source := newGeminiLifecycleUpload(data, record)
		uploadStarted := make(chan struct{})
		ctx, cancel := context.WithCancel(t.Context())
		client := newGeminiTestClient(t, profile, syntheticSecrets{"secret:gemini": "synthetic-key"}, roundTripFunc(func(request *http.Request) (*http.Response, error) {
			if request.Header.Get("X-Goog-Upload-Command") == "start" {
				response := geminiJSONResponse(request, `{}`)
				response.Header.Set("X-Goog-Upload-Url", origin+"/upload/v1beta/files?upload_id=synthetic-session-123&upload_protocol=resumable")
				return response, nil
			}
			close(uploadStarted)
			<-request.Context().Done()
			return nil, request.Context().Err()
		}))
		result := make(chan error, 1)
		go func() {
			_, err := client.Embed(ctx, directInputs(source), geminiDirectAuthorization(profile.Descriptor, int64(len(data))))
			result <- err
		}()
		<-uploadStarted
		cancel()
		require.ErrorIs(t, <-result, context.Canceled)
		assert.Equal(t, int32(1), source.closeCalls.Load())
	})

	t.Run("poll wait", func(t *testing.T) {
		profile := geminiDirectTestProfile(t, TransportFilesAPI)
		profile.PollInterval = maximumPoll
		profile = rebindGeminiProfile(t, profile)
		record := geminiCapability(t, profile, data, "synthetic.png", "image/png")
		source := newGeminiLifecycleUpload(data, record)
		fileName := "files/file-123"
		fileURI := origin + "/v1beta/" + fileName
		processing := geminiFileJSON(record, fileName, fileURI, "PROCESSING", newGeminiFileTimeline())
		pollSeen := make(chan struct{})
		var pollOnce sync.Once
		ctx, cancel := context.WithCancel(t.Context())
		client := newGeminiTestClient(t, profile, syntheticSecrets{"secret:gemini": "synthetic-key"}, roundTripFunc(func(request *http.Request) (*http.Response, error) {
			switch request.Method {
			case http.MethodPost:
				if request.Header.Get("X-Goog-Upload-Command") == "start" {
					response := geminiJSONResponse(request, `{}`)
					response.Header.Set("X-Goog-Upload-Url", origin+"/upload/v1beta/files?upload_id=synthetic-session-123&upload_protocol=resumable")
					return response, nil
				}
				response := geminiJSONResponse(request, `{"file":`+processing+`}`)
				response.Header.Set("X-Goog-Upload-Status", "final")
				return response, nil
			case http.MethodGet:
				pollOnce.Do(func() { close(pollSeen) })
				return geminiJSONResponse(request, processing), nil
			case http.MethodDelete:
				return geminiJSONResponse(request, `{}`), nil
			default:
				return nil, errors.New("unexpected request")
			}
		}))
		result := make(chan error, 1)
		go func() {
			_, err := client.Embed(ctx, directInputs(source), geminiDirectAuthorization(profile.Descriptor, int64(len(data))))
			result <- err
		}()
		<-pollSeen
		cancel()
		select {
		case err := <-result:
			require.ErrorIs(t, err, context.Canceled)
		case <-time.After(time.Second):
			t.Fatal("polling did not stop after cancellation")
		}
	})
}

func TestEmbedFilesAPIStopsAfterBoundedPollAttemptsAndCleansUp(t *testing.T) {
	data := geminiTinyPNG(t)
	profile := geminiDirectTestProfile(t, TransportFilesAPI)
	profile.MaxPollAttempts = 2
	profile = rebindGeminiProfile(t, profile)
	record := geminiCapability(t, profile, data, "synthetic.png", "image/png")
	source := newGeminiLifecycleUpload(data, record)
	fileName := "files/file-123"
	fileURI := origin + "/v1beta/" + fileName
	processing := geminiFileJSON(record, fileName, fileURI, "PROCESSING", newGeminiFileTimeline())
	var requests atomic.Int32
	client := newGeminiTestClient(t, profile, syntheticSecrets{"secret:gemini": "synthetic-key"}, roundTripFunc(func(request *http.Request) (*http.Response, error) {
		requests.Add(1)
		switch request.Method {
		case http.MethodPost:
			if request.Header.Get("X-Goog-Upload-Command") == "start" {
				response := geminiJSONResponse(request, `{}`)
				response.Header.Set("X-Goog-Upload-Url", origin+"/upload/v1beta/files?upload_id=synthetic-session-123&upload_protocol=resumable")
				return response, nil
			}
			response := geminiJSONResponse(request, `{"file":`+processing+`}`)
			response.Header.Set("X-Goog-Upload-Status", "final")
			return response, nil
		case http.MethodGet:
			return geminiJSONResponse(request, processing), nil
		case http.MethodDelete:
			return geminiJSONResponse(request, `{}`), nil
		default:
			return nil, errors.New("unexpected bounded-poll request")
		}
	}))
	ctx, cancel := context.WithTimeout(t.Context(), 80*time.Millisecond)
	defer cancel()
	receipt := Receipt{}

	_, err := client.embed(ctx, directInputs(source), geminiDirectAuthorization(profile.Descriptor, int64(len(data))), &receipt)
	require.ErrorIs(t, err, ErrPermanentResponse)
	assert.Equal(t, int32(5), requests.Load())
	assert.Equal(t, 5, receipt.RequestCount)
	assert.Equal(t, int32(1), source.closeCalls.Load())
}

func geminiDirectTestProfile(t *testing.T, transport Transport) Profile {
	t.Helper()
	profile := geminiTestProfile(t, 128)
	profile.Transport = transport
	profile.CapabilityProfileFingerprint = testCapabilityProfile
	profile.DisclosureFingerprint = testDisclosurePolicy
	profile.PollInterval = minimumPoll
	profile.CleanupTimeout = time.Second
	profile.Descriptor.InputKinds = []document.EmbeddingInputKind{
		document.EmbeddingInputOriginalFile, document.EmbeddingInputRenditionChunk,
	}
	return rebindGeminiProfile(t, profile)
}

func rebindGeminiProfile(t *testing.T, profile Profile) Profile {
	t.Helper()
	profile.Descriptor.PolicyFingerprint = strings.Repeat("0", sha256.Size*2)
	profile.Descriptor.Fingerprint = ""
	descriptor, err := document.NewEmbeddingDescriptor(profile.Descriptor)
	require.NoError(t, err)
	profile.Descriptor = descriptor
	fingerprint, err := PolicyFingerprint(profile)
	require.NoError(t, err)
	profile.Descriptor.PolicyFingerprint = fingerprint
	profile.Descriptor.Fingerprint = ""
	profile.Descriptor, err = document.NewEmbeddingDescriptor(profile.Descriptor)
	require.NoError(t, err)
	return profile
}

func geminiCapability(t *testing.T, profile Profile, data []byte, filename, mediaType string) media.CapabilityRecord {
	t.Helper()
	digest := sha256.Sum256(data)
	maxDuration := int64(180_000)
	if strings.HasPrefix(mediaType, "video/") {
		maxDuration = 120_000
	}
	record, err := media.InspectCapability(bytes.NewReader(data), media.InspectionPolicy{
		Filename: filename, DeclaredMediaType: mediaType, ExpectedBytes: int64(len(data)),
		ExpectedSHA256: hex.EncodeToString(digest[:]), DescriptorFingerprint: profile.Descriptor.Fingerprint,
		ProfileFingerprint: profile.CapabilityProfileFingerprint, DisclosureFingerprint: profile.DisclosureFingerprint,
		InputKind: document.RenditionInputOriginalFile, MaxSourceBytes: profile.MaxInputBytes,
		MaxExpandedBytes: 1 << 20, MaxEntryBytes: 1 << 20, MaxEntries: 100, MaxNestingDepth: 1,
		MaxTextLines: 1_000, MaxCharacters: 1 << 20, MaxRecords: 1_000, MaxPages: 6,
		MaxSlides: 100, MaxSheets: 100, MaxCells: 10_000, MaxSpineItems: 1_000, MaxResources: 10_000,
		MaxPixels: 16_000_000, MaxFrames: 32, MaxDurationMS: maxDuration,
	})
	require.NoError(t, err)
	require.True(t, record.Eligible, record.Reason)
	return record
}

func directInputs(sources ...document.AuthorizedUpload) []document.EmbeddingInput {
	inputs := make([]document.EmbeddingInput, len(sources))
	for index, source := range sources {
		inputs[index] = document.EmbeddingInput{Key: "direct-" + string(rune('a'+index)), Role: document.EmbeddingRoleDocument,
			Kind: document.EmbeddingInputOriginalFile, Source: source}
	}
	return inputs
}

func geminiDirectAuthorization(descriptor document.EmbeddingDescriptor, maxInputBytes int64) document.EmbeddingAuthorization {
	return document.EmbeddingAuthorization{ProviderID: descriptor.ID, DescriptorFingerprint: descriptor.Fingerprint,
		PolicyFingerprint: descriptor.PolicyFingerprint, MaxBatchItems: 8, MaxInputBytes: maxInputBytes,
		MaxResponseBytes: 8 << 10}
}

type geminiFileTimeline struct {
	created time.Time
	updated time.Time
	expires time.Time
}

func newGeminiFileTimeline() geminiFileTimeline {
	created := time.Now().UTC().Truncate(time.Second)
	return geminiFileTimeline{created: created, updated: created.Add(time.Second), expires: created.Add(48 * time.Hour)}
}

func geminiFileJSON(record media.CapabilityRecord, name, uri, state string, timeline geminiFileTimeline) string {
	return `{"name":"` + name + `","displayName":"","mimeType":"` + record.MediaType + `","sizeBytes":"` + strconv.FormatInt(record.SourceBytes, 10) +
		`","createTime":"` + timeline.created.Format(time.RFC3339Nano) + `","updateTime":"` + timeline.updated.Format(time.RFC3339Nano) +
		`","expirationTime":"` + timeline.expires.Format(time.RFC3339Nano) + `","sha256Hash":"` +
		base64.StdEncoding.EncodeToString([]byte(record.SourceSHA256)) + `","uri":"` + uri + `","downloadUri":"","state":"` + state + `","source":"UPLOADED"}`
}

func geminiTinyPNG(t *testing.T) []byte {
	t.Helper()
	data, err := base64.StdEncoding.DecodeString("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNk+A8AAQUBAScY42YAAAAASUVORK5CYII=")
	require.NoError(t, err)
	return data
}

type geminiLifecycleUpload struct {
	reader *bytes.Reader
	record media.CapabilityRecord

	metadata           []document.AuthorizedUploadMetadata
	metadataCalls      atomic.Int32
	capabilityCalls    atomic.Int32
	closeCalls         atomic.Int32
	readPasses         atomic.Int32
	blockRead          bool
	captureReadBuffer  bool
	capturedReadBuffer []byte
	readStarted        chan struct{}
	released           chan struct{}
	startOnce          sync.Once
	releaseOnce        sync.Once
}

type geminiValueUpload struct {
	*geminiLifecycleUpload
}

type geminiNonComparableUpload struct {
	*geminiLifecycleUpload

	identity []byte
}

type geminiInterfaceIdentityUpload struct {
	*geminiLifecycleUpload

	identity any
}

func newGeminiLifecycleUpload(data []byte, record media.CapabilityRecord) *geminiLifecycleUpload {
	metadata := document.AuthorizedUploadMetadata{Filename: record.Policy.Filename, MediaFamily: record.MediaFamily,
		MediaType: record.MediaType, ByteLength: record.SourceBytes, SHA256: record.SourceSHA256,
		CapabilityRecordChecksum: record.Checksum, ProviderMetadataChecksum: strings.Repeat("b", 64),
		InputKind: document.RenditionInputOriginalFile}
	return &geminiLifecycleUpload{reader: bytes.NewReader(data), record: record, metadata: []document.AuthorizedUploadMetadata{metadata},
		readStarted: make(chan struct{}), released: make(chan struct{})}
}

func (upload *geminiLifecycleUpload) Read(buffer []byte) (int, error) {
	upload.startOnce.Do(func() {
		upload.readPasses.Add(1)
		close(upload.readStarted)
	})
	if upload.blockRead {
		<-upload.released
		return 0, errors.New("synthetic source closed")
	}
	read, err := upload.reader.Read(buffer)
	if upload.captureReadBuffer && read > 0 && upload.capturedReadBuffer == nil {
		upload.capturedReadBuffer = buffer[:read]
	}
	return read, err //nolint:wrapcheck // Preserve io.EOF for the real reader contract.
}

func (upload *geminiLifecycleUpload) Close() error {
	upload.closeCalls.Add(1)
	upload.releaseOnce.Do(func() { close(upload.released) })
	return nil
}

func (upload *geminiLifecycleUpload) Metadata() document.AuthorizedUploadMetadata {
	call := int(upload.metadataCalls.Add(1)) - 1
	return upload.metadata[min(call, len(upload.metadata)-1)]
}

func (upload *geminiLifecycleUpload) CapabilityRecord() media.CapabilityRecord {
	upload.capabilityCalls.Add(1)
	return upload.record
}

var _ document.AuthorizedUpload = (*geminiLifecycleUpload)(nil)

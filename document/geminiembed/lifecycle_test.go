package geminiembed

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/media/mediatest"
)

func TestValidateUploadURLRejectsUnsealedOrNoncanonicalDestinations(t *testing.T) {
	for _, valid := range []string{
		origin + "/upload/v1beta/files?upload_id=synthetic-session-123&upload_protocol=resumable",
		origin + "/upload/v1beta/files?upload_protocol=resumable&upload_id=synthetic-session-123",
		"https://generativelanguage.googleapis.com:443/upload/v1beta/files?upload_id=synthetic-session-123&upload_protocol=resumable",
	} {
		parsed, err := validateUploadURL(valid)
		require.NoError(t, err)
		assert.Equal(t, valid, parsed.String())
	}

	for _, value := range []string{
		"https://provider.invalid/upload/v1beta/files?upload_id=synthetic&upload_protocol=resumable",
		"https://synthetic@generativelanguage.googleapis.com/upload/v1beta/files?upload_id=synthetic&upload_protocol=resumable",
		"https://generativelanguage.googleapis.com:444/upload/v1beta/files?upload_id=synthetic&upload_protocol=resumable",
		"https://generativelanguage.googleapis.com/upload/v1beta/other?upload_id=synthetic&upload_protocol=resumable",
		"https://generativelanguage.googleapis.com/upload/v1beta/files?upload_id=synthetic&upload_protocol=raw",
		"https://generativelanguage.googleapis.com/upload/v1beta/files?upload_protocol=resumable&upload_id=synthetic&extra=true",
		"https://generativelanguage.googleapis.com/upload/v1beta/files?upload_id=synthetic%2Fother&upload_protocol=resumable",
		"https://generativelanguage.googleapis.com/upload/v1beta/files?upload_id=synthetic&upload_protocol=resumable#fragment",
	} {
		t.Run(value, func(t *testing.T) {
			_, err := validateUploadURL(value)
			require.Error(t, err)
		})
	}
}

func TestFilesUnsafeUploadURLStopsBeforeRawTransferWithoutRetentionClaim(t *testing.T) {
	for _, uploadURL := range []string{
		"https://provider.invalid/upload/v1beta/files?upload_id=synthetic&upload_protocol=resumable",
		origin + "/upload/v1beta/files?upload_id=synthetic&upload_protocol=raw",
		origin + "/upload/v1beta/files?upload_id=synthetic%2Fother&upload_protocol=resumable",
	} {
		t.Run(uploadURL, func(t *testing.T) {
			profile, _, source, _ := newFilesExecutionFixture(t, "unsafe-url.png")
			var requests atomic.Int32
			client := newGeminiTestClient(t, profile, syntheticSecrets{"secret:gemini": "synthetic-key"}, roundTripFunc(func(request *http.Request) (*http.Response, error) {
				requests.Add(1)
				response := geminiJSONResponse(request, "")
				response.Header.Set("X-Goog-Upload-Url", uploadURL)
				return response, nil
			}))

			execution, err := client.EmbedWithReceipt(t.Context(), directGeminiInputs(source), directGeminiAuthorization(profile, 1))

			require.ErrorIs(t, err, ErrPermanentResponse)
			require.NotErrorIs(t, err, ErrRemoteRetentionUnconfirmed)
			assert.Equal(t, int32(1), requests.Load())
			assert.Zero(t, execution.Receipt.UnconfirmedFileRetentions)
		})
	}
}

func TestFilesInvalidCreatedIdentityNeverAuthorizesFollowUp(t *testing.T) {
	profile, data, source, fixture := newFilesExecutionFixture(t, "invalid-created.png")
	invalid := strings.Replace(fixture.fileJSON("ACTIVE"), `"name":"files/file-123"`, `"name":"files/UPPER"`, 1)
	var requests atomic.Int32
	client := newGeminiTestClient(t, profile, syntheticSecrets{"secret:gemini": "synthetic-key"}, roundTripFunc(func(request *http.Request) (*http.Response, error) {
		switch requests.Add(1) {
		case 1:
			return filesStartResponse(request), nil
		case 2:
			response := geminiJSONResponse(request, `{"file":`+invalid+`}`)
			response.Header.Set("X-Goog-Upload-Status", "final")
			return response, nil
		default:
			return nil, errors.New("invalid identity authorized a follow-up request")
		}
	}))

	execution, err := client.EmbedWithReceipt(t.Context(), directGeminiInputs(source), directGeminiAuthorization(profile, 1))

	require.ErrorIs(t, err, ErrPermanentResponse)
	require.ErrorIs(t, err, ErrRemoteRetentionUnconfirmed)
	assert.Empty(t, execution.Result)
	assert.Equal(t, int32(2), requests.Load())
	assert.Equal(t, 1, execution.Receipt.UnconfirmedFileRetentions)
	assert.Equal(t, []string{"provider file retention is unconfirmed"}, execution.Receipt.Warnings)
	assertReceiptHasNoFileEvidence(t, execution.Receipt, data, fixture)
}

func TestValidateCreatedAndPolledFilesRequirePinnedIdentityAndTimes(t *testing.T) {
	data := mediatest.PNG(2, 2, nil)
	fixture := newSuccessfulFilesLifecycle(t, data, "image/png", "identity.png")
	expected := verifiedFile{metadata: document.AuthorizedUploadMetadata{
		MediaType: "image/png", ByteLength: int64(len(data)), SHA256: sha256Hex(data),
	}}
	var created wireFile
	require.NoError(t, json.Unmarshal([]byte(fixture.fileJSON("PROCESSING")), &created, json.RejectUnknownMembers(true)))
	startedAt := fixture.timeline.created.Add(-time.Second)
	completedAt := fixture.timeline.created.Add(time.Second)
	current, ok := validateCreatedWireFile(created, expected, startedAt, completedAt)
	require.True(t, ok)

	invalidCreated := map[string]func(*wireFile){
		"name":    func(file *wireFile) { file.Name = "files/UPPER" },
		"uri":     func(file *wireFile) { file.URI = origin + "/v1beta/files/foreign" },
		"mime":    func(file *wireFile) { file.MIMEType = "image/jpeg" },
		"source":  func(file *wireFile) { file.Source = "GENERATED" },
		"size":    func(file *wireFile) { file.SizeBytes = "1" },
		"hash":    func(file *wireFile) { file.SHA256Hash = "c3ludGhldGlj" },
		"state":   func(file *wireFile) { file.State = "READY" },
		"display": func(file *wireFile) { file.DisplayName = strings.Repeat("x", 513) },
		"create time": func(file *wireFile) {
			file.CreateTime = completedAt.Add(fileClockSkew + time.Second).Format(time.RFC3339Nano)
		},
		"update time": func(file *wireFile) {
			file.UpdateTime = startedAt.Add(-fileClockSkew - time.Second).Format(time.RFC3339Nano)
		},
		"expiry": func(file *wireFile) {
			file.ExpirationTime = fixture.timeline.created.Add(retentionCeiling + time.Second).Format(time.RFC3339Nano)
		},
	}
	for name, mutate := range invalidCreated {
		t.Run("created "+name, func(t *testing.T) {
			candidate := created
			mutate(&candidate)
			_, valid := validateCreatedWireFile(candidate, expected, startedAt, completedAt)
			assert.False(t, valid)
		})
	}

	polled := created
	polled.State = "ACTIVE"
	polled.UpdateTime = fixture.timeline.created.Add(time.Second).Format(time.RFC3339Nano)
	pollStarted := fixture.timeline.created
	pollCompleted := fixture.timeline.created.Add(2 * time.Second)
	_, ok = validatePolledWireFile(polled, expected, current, pollStarted, pollCompleted)
	require.True(t, ok)
	invalidPoll := map[string]func(*wireFile){
		"name": func(file *wireFile) { file.Name = "files/foreign"; file.URI = origin + "/v1beta/files/foreign" },
		"creation changed": func(file *wireFile) {
			file.CreateTime = fixture.timeline.created.Add(time.Second).Format(time.RFC3339Nano)
		},
		"expiration changed": func(file *wireFile) {
			file.ExpirationTime = fixture.timeline.expires.Add(-time.Second).Format(time.RFC3339Nano)
		},
		"update regressed": func(file *wireFile) {
			file.UpdateTime = fixture.timeline.created.Add(-time.Second).Format(time.RFC3339Nano)
		},
		"update in future": func(file *wireFile) {
			file.UpdateTime = pollCompleted.Add(fileClockSkew + time.Second).Format(time.RFC3339Nano)
		},
	}
	for name, mutate := range invalidPoll {
		t.Run("poll "+name, func(t *testing.T) {
			candidate := polled
			mutate(&candidate)
			_, valid := validatePolledWireFile(candidate, expected, current, pollStarted, pollCompleted)
			assert.False(t, valid)
		})
	}
}

func TestFilesMalformedFinalizeReceiptIDStillDeletesValidatedFile(t *testing.T) {
	profile, _, source, fixture := newFilesExecutionFixture(t, "invalid-id.png")
	var requests atomic.Int32
	client := newGeminiTestClient(t, profile, syntheticSecrets{"secret:gemini": "synthetic-key"}, roundTripFunc(func(request *http.Request) (*http.Response, error) {
		switch requests.Add(1) {
		case 1:
			return filesStartResponse(request), nil
		case 2:
			response := geminiJSONResponse(request, `{"file":`+fixture.fileJSON("ACTIVE")+`}`)
			response.Header.Set("X-Goog-Upload-Status", "final")
			response.Header.Set("X-Goog-Request-Id", "invalid response id")
			return response, nil
		case 3:
			require.Equal(t, http.MethodDelete, request.Method)
			return geminiJSONResponse(request, `{}`), nil
		default:
			return nil, errors.New("unexpected receipt-rejection request")
		}
	}))

	execution, err := client.EmbedWithReceipt(t.Context(), directGeminiInputs(source), directGeminiAuthorization(profile, 1))

	require.ErrorIs(t, err, ErrPermanentResponse)
	require.NotErrorIs(t, err, ErrRemoteRetentionUnconfirmed)
	assert.Empty(t, execution.Result)
	assert.Equal(t, int32(3), requests.Load())
	assert.Zero(t, execution.Receipt.UnconfirmedFileRetentions)
	assert.Empty(t, execution.Receipt.Warnings)
}

func TestFilesForeignPolledIdentityNeverReplacesPinnedDeleteAuthority(t *testing.T) {
	profile, _, source, fixture := newFilesExecutionFixture(t, "foreign-poll.png")
	foreignName := "files/foreign-456"
	foreignURI := origin + "/v1beta/" + foreignName
	foreign := fileTestJSON(fixture.data, fixture.mediaType, foreignName, foreignURI, "ACTIVE", fixture.timeline)
	var requests atomic.Int32
	client := newGeminiTestClient(t, profile, syntheticSecrets{"secret:gemini": "synthetic-key"}, roundTripFunc(func(request *http.Request) (*http.Response, error) {
		switch requests.Add(1) {
		case 1:
			return filesStartResponse(request), nil
		case 2:
			response := geminiJSONResponse(request, `{"file":`+fixture.fileJSON("PROCESSING")+`}`)
			response.Header.Set("X-Goog-Upload-Status", "final")
			return response, nil
		case 3:
			return geminiJSONResponse(request, foreign), nil
		case 4:
			require.Equal(t, http.MethodDelete, request.Method)
			assert.Equal(t, fixture.fileURI, request.URL.String())
			return geminiJSONResponse(request, `{}`), nil
		default:
			return nil, errors.New("foreign poll identity authorized extra egress")
		}
	}))

	execution, err := client.EmbedWithReceipt(t.Context(), directGeminiInputs(source), directGeminiAuthorization(profile, 1))

	require.ErrorIs(t, err, ErrPermanentResponse)
	require.NotErrorIs(t, err, ErrRemoteRetentionUnconfirmed)
	assert.Equal(t, int32(4), requests.Load())
	assert.Zero(t, execution.Receipt.UnconfirmedFileRetentions)
}

func TestFilesUnknownFinalizeAfterRawTransferReportsRetentionWithoutDelete(t *testing.T) {
	profile, _, source, _ := newFilesExecutionFixture(t, "unknown-finalize.png")
	var requests atomic.Int32
	client := newGeminiTestClient(t, profile, syntheticSecrets{"secret:gemini": "synthetic-key"}, roundTripFunc(func(request *http.Request) (*http.Response, error) {
		switch requests.Add(1) {
		case 1:
			return filesStartResponse(request), nil
		case 2:
			return nil, errors.New("synthetic raw transport failure")
		default:
			return nil, errors.New("unknown finalize authorized cleanup")
		}
	}))

	execution, err := client.EmbedWithReceipt(t.Context(), directGeminiInputs(source), directGeminiAuthorization(profile, 1))

	require.ErrorIs(t, err, ErrRemoteRetentionUnconfirmed)
	assert.Empty(t, execution.Result)
	assert.Equal(t, int32(2), requests.Load())
	assert.Equal(t, 1, execution.Receipt.UnconfirmedFileRetentions)
}

func TestFilesPollAttemptBoundCleansUpValidatedIdentity(t *testing.T) {
	profile := geminiTestProfile(t, 128)
	profile.Transport = TransportFilesAPI
	profile.MaxPollAttempts = 2
	profile.PollInterval = minimumPoll
	profile.Descriptor = geminiDescriptorFor(t, profile)
	data := mediatest.PNG(2, 2, nil)
	source := authorizeGeminiFixture(t, profile, data, "poll-bound.png", "image/png")
	fixture := newSuccessfulFilesLifecycle(t, data, "image/png", "poll-bound.png")
	var requests atomic.Int32
	client := newGeminiTestClient(t, profile, syntheticSecrets{"secret:gemini": "synthetic-key"}, roundTripFunc(func(request *http.Request) (*http.Response, error) {
		requests.Add(1)
		switch request.Method {
		case http.MethodPost:
			if request.Header.Get("X-Goog-Upload-Command") == "start" {
				return filesStartResponse(request), nil
			}
			response := geminiJSONResponse(request, `{"file":`+fixture.fileJSON("PROCESSING")+`}`)
			response.Header.Set("X-Goog-Upload-Status", "final")
			return response, nil
		case http.MethodGet:
			return geminiJSONResponse(request, fixture.fileJSON("PROCESSING")), nil
		case http.MethodDelete:
			return geminiJSONResponse(request, `{}`), nil
		default:
			return nil, errors.New("unexpected bounded-poll request")
		}
	}))

	execution, err := client.EmbedWithReceipt(t.Context(), directGeminiInputs(source), directGeminiAuthorization(profile, 1))

	require.ErrorIs(t, err, ErrPermanentResponse)
	require.NotErrorIs(t, err, ErrRemoteRetentionUnconfirmed)
	assert.Empty(t, execution.Result)
	assert.Equal(t, int32(5), requests.Load())
	assert.Equal(t, 5, execution.Receipt.RequestCount)
}

func TestFilesCleanupTimeoutIsDetachedAndBounded(t *testing.T) {
	profile := geminiTestProfile(t, 128)
	profile.Transport = TransportFilesAPI
	profile.CleanupTimeout = 20 * time.Millisecond
	profile.Descriptor = geminiDescriptorFor(t, profile)
	data := mediatest.PNG(2, 2, nil)
	source := authorizeGeminiFixture(t, profile, data, "cleanup-timeout.png", "image/png")
	fixture := newSuccessfulFilesLifecycle(t, data, "image/png", "cleanup-timeout.png")
	deleteStarted := make(chan struct{})
	client := newGeminiTestClient(t, profile, syntheticSecrets{"secret:gemini": "synthetic-key"}, activeFilesTransport(t, fixture, func(request *http.Request) (*http.Response, error) {
		close(deleteStarted)
		<-request.Context().Done()
		return nil, request.Context().Err()
	}))
	done := make(chan error, 1)
	go func() {
		_, err := client.Embed(t.Context(), directGeminiInputs(source), directGeminiAuthorization(profile, 1))
		done <- err
	}()

	awaitSignal(t, deleteStarted)
	err := awaitError(t, done)

	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.ErrorIs(t, err, ErrRemoteRetentionUnconfirmed)
}

func TestFilesCleanupRejectsLateSuccessAfterTimeout(t *testing.T) {
	profile := geminiTestProfile(t, 128)
	profile.Transport = TransportFilesAPI
	profile.CleanupTimeout = 20 * time.Millisecond
	profile.Descriptor = geminiDescriptorFor(t, profile)
	data := mediatest.PNG(2, 2, nil)
	source := authorizeGeminiFixture(t, profile, data, "late-cleanup.png", "image/png")
	fixture := newSuccessfulFilesLifecycle(t, data, "image/png", "late-cleanup.png")
	deleteReadStarted := make(chan struct{})
	client := newGeminiTestClient(t, profile, syntheticSecrets{"secret:gemini": "synthetic-key"}, activeFilesTransport(t, fixture, func(request *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}},
			Body: &lateSuccessBody{ctx: request.Context(), started: deleteReadStarted, payload: []byte(`{}`)}, Request: request}, nil
	}))

	_, err := client.Embed(t.Context(), directGeminiInputs(source), directGeminiAuthorization(profile, 1))

	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.ErrorIs(t, err, ErrRemoteRetentionUnconfirmed)
	awaitSignal(t, deleteReadStarted)
}

func TestFilesStartFailureDoesNotClaimSourceRetention(t *testing.T) {
	profile, _, source, _ := newFilesExecutionFixture(t, "start-failure.png")
	client := newGeminiTestClient(t, profile, syntheticSecrets{"secret:gemini": "synthetic-key"}, roundTripFunc(func(request *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusServiceUnavailable, Header: make(http.Header), Body: http.NoBody, Request: request}, nil
	}))

	execution, err := client.EmbedWithReceipt(t.Context(), directGeminiInputs(source), directGeminiAuthorization(profile, 1))

	require.ErrorIs(t, err, ErrTransientResponse)
	require.NotErrorIs(t, err, ErrRemoteRetentionUnconfirmed)
	assert.Zero(t, execution.Receipt.UnconfirmedFileRetentions)
	assert.Empty(t, execution.Receipt.Warnings)
}

func TestFilesTransportFailuresPreserveSanitizedCauseIdentities(t *testing.T) {
	for _, stage := range []string{"start", "finalize", "poll", "delete", "primary and cleanup"} {
		t.Run(stage, func(t *testing.T) {
			profile, data, source, fixture := newFilesExecutionFixture(t, "transport-cause.png")
			sensitiveURL := origin + filesUploadPath + "?upload_id=private-session&upload_protocol=resumable"
			sensitiveKey := "synthetic-sensitive-key"
			sensitiveData := base64.StdEncoding.EncodeToString(data)
			primary := &sensitiveTransportError{text: strings.Join([]string{sensitiveURL, sensitiveKey, sensitiveData, "primary", stage}, " ")}
			cleanup := &sensitiveCleanupError{text: strings.Join([]string{sensitiveURL, sensitiveKey, sensitiveData, "cleanup"}, " ")}
			finalState := "ACTIVE"
			if stage == "poll" || stage == "primary and cleanup" {
				finalState = "PROCESSING"
			}
			var requests atomic.Int32
			client := newGeminiTestClient(t, profile, syntheticSecrets{"secret:gemini": "synthetic-key"}, roundTripFunc(func(request *http.Request) (*http.Response, error) {
				switch requests.Add(1) {
				case 1:
					if stage == "start" {
						return nil, primary
					}
					return filesStartResponse(request), nil
				case 2:
					if stage == "finalize" {
						return nil, primary
					}
					response := geminiJSONResponse(request, `{"file":`+fixture.fileJSON(finalState)+`}`)
					response.Header.Set("X-Goog-Upload-Status", "final")
					return response, nil
				case 3:
					if stage == "poll" || stage == "primary and cleanup" {
						return nil, primary
					}
					return geminiJSONResponse(request, `{"embedding":{"values":[`+testVectorJSON(128)+`]}}`), nil
				case 4:
					if stage == "delete" {
						return nil, primary
					}
					if stage == "primary and cleanup" {
						return nil, cleanup
					}
					return geminiJSONResponse(request, `{}`), nil
				default:
					return nil, errors.New("unexpected transport-cause lifecycle request")
				}
			}))

			execution, err := client.EmbedWithReceipt(t.Context(), directGeminiInputs(source), directGeminiAuthorization(profile, 1))

			require.ErrorIs(t, err, primary)
			var primaryIdentity *sensitiveTransportError
			require.ErrorAs(t, err, &primaryIdentity)
			assert.Same(t, primary, primaryIdentity)
			if stage == "primary and cleanup" {
				require.ErrorIs(t, err, cleanup)
				var cleanupIdentity *sensitiveCleanupError
				require.ErrorAs(t, err, &cleanupIdentity)
				assert.Same(t, cleanup, cleanupIdentity)
			}
			assert.Empty(t, execution.Result)
			for _, forbidden := range []string{sensitiveURL, sensitiveKey, sensitiveData} {
				assert.NotContains(t, err.Error(), forbidden)
				assert.NotContains(t, fmt.Sprintf("%#v", execution.Receipt), forbidden)
			}
			wantRequests := map[string]int32{"start": 1, "finalize": 2, "poll": 4, "delete": 4, "primary and cleanup": 4}[stage]
			assert.Equal(t, wantRequests, requests.Load())
			wantRetention := 0
			if stage == "finalize" || stage == "delete" || stage == "primary and cleanup" {
				wantRetention = 1
				require.ErrorIs(t, err, ErrRemoteRetentionUnconfirmed)
			}
			assert.Equal(t, wantRetention, execution.Receipt.UnconfirmedFileRetentions)
			assertReceiptHasNoFileEvidence(t, execution.Receipt, data, fixture)
		})
	}
}

func TestFilesCanceledTransportPreservesContextAndSanitizedCause(t *testing.T) {
	profile, data, source, _ := newFilesExecutionFixture(t, "canceled-transport.png")
	sensitiveURL := origin + filesUploadPath + "?upload_id=private-session&upload_protocol=resumable"
	sensitiveKey := "synthetic-sensitive-key"
	sensitiveData := base64.StdEncoding.EncodeToString(data)
	cause := &sensitiveTransportError{text: strings.Join([]string{sensitiveURL, sensitiveKey, sensitiveData}, " ")}
	finalizeStarted := make(chan struct{})
	var requests atomic.Int32
	client := newGeminiTestClient(t, profile, syntheticSecrets{"secret:gemini": "synthetic-key"}, roundTripFunc(func(request *http.Request) (*http.Response, error) {
		switch requests.Add(1) {
		case 1:
			return filesStartResponse(request), nil
		case 2:
			close(finalizeStarted)
			<-request.Context().Done()
			return nil, cause
		default:
			return nil, errors.New("canceled finalize authorized follow-up")
		}
	}))
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct {
		execution Execution
		err       error
	}, 1)
	go func() {
		execution, err := client.EmbedWithReceipt(ctx, directGeminiInputs(source), directGeminiAuthorization(profile, 1))
		done <- struct {
			execution Execution
			err       error
		}{execution: execution, err: err}
	}()
	awaitSignal(t, finalizeStarted)
	cancel()
	var outcome struct {
		execution Execution
		err       error
	}
	select {
	case outcome = <-done:
	case <-time.After(time.Second):
		t.Fatal("canceled finalize did not finish")
	}

	require.ErrorIs(t, outcome.err, context.Canceled)
	require.ErrorIs(t, outcome.err, cause)
	var identity *sensitiveTransportError
	require.ErrorAs(t, outcome.err, &identity)
	assert.Same(t, cause, identity)
	require.ErrorIs(t, outcome.err, ErrRemoteRetentionUnconfirmed)
	for _, forbidden := range []string{sensitiveURL, sensitiveKey, sensitiveData} {
		assert.NotContains(t, outcome.err.Error(), forbidden)
		assert.NotContains(t, fmt.Sprintf("%#v", outcome.execution.Receipt), forbidden)
	}
	assert.Equal(t, int32(2), requests.Load())
}

func TestFilesEmbeddingEnvelopeCapacityFailsBeforeSourceRead(t *testing.T) {
	profile := geminiTestProfile(t, 128)
	profile.Transport = TransportFilesAPI
	profile.Descriptor = geminiDescriptorFor(t, profile)
	data := mediatest.PNG(2, 2, nil)
	metadata, proof := issuedGeminiAuthority(t, profile, data, "envelope-capacity.png", "image/png")
	source := newProofUpload(data, metadata, proof)
	client, secrets, requests := noEgressGeminiClient(t, profile)
	minimum, err := minimumFilesAPIRequestCapacity(profile.Descriptor.Dimension)
	require.NoError(t, err)
	require.Less(t, int64(len(data)), minimum-7)
	client.profile.MaxRequestBytes = minimum - 7

	_, err = client.Embed(t.Context(), directGeminiInputs(source), directGeminiAuthorization(profile, 1))

	require.ErrorContains(t, err, "request")
	assert.Zero(t, source.readPasses.Load())
	assert.Zero(t, secrets.calls.Load())
	assert.Zero(t, requests.Load())
}

func TestFilesLifecycleBoundsEveryResponseBody(t *testing.T) {
	for _, stage := range []string{"start", "finalize", "poll", "delete"} {
		t.Run(stage, func(t *testing.T) {
			profile, _, source, fixture := newFilesExecutionFixture(t, "bounded-response.png")
			oversized := strings.Repeat("x", int(profile.MaxResponseBytes)+1)
			finalState := "ACTIVE"
			if stage == "poll" {
				finalState = "PROCESSING"
			}
			var requests atomic.Int32
			client := newGeminiTestClient(t, profile, syntheticSecrets{"secret:gemini": "synthetic-key"}, roundTripFunc(func(request *http.Request) (*http.Response, error) {
				switch requests.Add(1) {
				case 1:
					if stage == "start" {
						return geminiJSONResponse(request, oversized), nil
					}
					return filesStartResponse(request), nil
				case 2:
					body := `{"file":` + fixture.fileJSON(finalState) + `}`
					if stage == "finalize" {
						body = oversized
					}
					response := geminiJSONResponse(request, body)
					response.Header.Set("X-Goog-Upload-Status", "final")
					return response, nil
				case 3:
					if stage == "poll" {
						return geminiJSONResponse(request, oversized), nil
					}
					return geminiJSONResponse(request, `{"embedding":{"values":[`+testVectorJSON(128)+`]}}`), nil
				case 4:
					if stage == "delete" {
						return geminiJSONResponse(request, oversized), nil
					}
					return geminiJSONResponse(request, `{}`), nil
				default:
					return nil, errors.New("oversized response triggered extra egress")
				}
			}))

			execution, err := client.EmbedWithReceipt(t.Context(), directGeminiInputs(source), directGeminiAuthorization(profile, 1))

			require.Error(t, err)
			assert.Empty(t, execution.Result)
			wantRequests := map[string]int32{"start": 1, "finalize": 2, "poll": 4, "delete": 4}[stage]
			assert.Equal(t, wantRequests, requests.Load())
			wantRetention := 0
			if stage == "finalize" || stage == "delete" {
				wantRetention = 1
				require.ErrorIs(t, err, ErrRemoteRetentionUnconfirmed)
			} else {
				require.NotErrorIs(t, err, ErrRemoteRetentionUnconfirmed)
			}
			assert.Equal(t, wantRetention, execution.Receipt.UnconfirmedFileRetentions)
			assert.NotContains(t, err.Error(), oversized)
		})
	}
}

func TestFilesLifecycleRejectsUnknownResponseMembers(t *testing.T) {
	for _, stage := range []string{"start", "finalize", "poll", "delete"} {
		t.Run(stage, func(t *testing.T) {
			profile, _, source, fixture := newFilesExecutionFixture(t, "strict-response.png")
			finalState := "ACTIVE"
			if stage == "poll" {
				finalState = "PROCESSING"
			}
			var requests atomic.Int32
			client := newGeminiTestClient(t, profile, syntheticSecrets{"secret:gemini": "synthetic-key"}, roundTripFunc(func(request *http.Request) (*http.Response, error) {
				switch requests.Add(1) {
				case 1:
					if stage == "start" {
						response := geminiJSONResponse(request, `{"unknown":true}`)
						response.Header.Set("X-Goog-Upload-Url", origin+filesUploadPath+"?upload_id=synthetic&upload_protocol=resumable")
						return response, nil
					}
					return filesStartResponse(request), nil
				case 2:
					body := `{"file":` + fixture.fileJSON(finalState) + `}`
					if stage == "finalize" {
						body = strings.TrimSuffix(body, "}") + `,"unknown":true}`
					}
					response := geminiJSONResponse(request, body)
					response.Header.Set("X-Goog-Upload-Status", "final")
					return response, nil
				case 3:
					if stage == "poll" {
						body := strings.TrimSuffix(fixture.fileJSON("ACTIVE"), "}") + `,"unknown":true}`
						return geminiJSONResponse(request, body), nil
					}
					return geminiJSONResponse(request, `{"embedding":{"values":[`+testVectorJSON(128)+`]}}`), nil
				case 4:
					if stage == "delete" {
						return geminiJSONResponse(request, `{"unknown":true}`), nil
					}
					return geminiJSONResponse(request, `{}`), nil
				default:
					return nil, errors.New("unknown response member triggered extra egress")
				}
			}))

			execution, err := client.EmbedWithReceipt(t.Context(), directGeminiInputs(source), directGeminiAuthorization(profile, 1))

			require.ErrorIs(t, err, ErrPermanentResponse)
			assert.Empty(t, execution.Result)
			wantRetention := 0
			if stage == "finalize" || stage == "delete" {
				wantRetention = 1
				require.ErrorIs(t, err, ErrRemoteRetentionUnconfirmed)
			} else {
				require.NotErrorIs(t, err, ErrRemoteRetentionUnconfirmed)
			}
			assert.Equal(t, wantRetention, execution.Receipt.UnconfirmedFileRetentions)
		})
	}
}

func newFilesExecutionFixture(t *testing.T, filename string) (Profile, []byte, document.AuthorizedUpload, *successfulFilesLifecycle) {
	t.Helper()
	profile := geminiTestProfile(t, 128)
	profile.Transport = TransportFilesAPI
	profile.Descriptor = geminiDescriptorFor(t, profile)
	data := mediatest.PNG(2, 2, nil)
	source := authorizeGeminiFixture(t, profile, data, filename, "image/png")
	return profile, data, source, newSuccessfulFilesLifecycle(t, data, "image/png", filename)
}

func filesStartResponse(request *http.Request) *http.Response {
	response := geminiJSONResponse(request, "")
	response.Header.Set("X-Goog-Upload-Url", origin+"/upload/v1beta/files?upload_id=synthetic-session-123&upload_protocol=resumable")
	return response
}

func activeFilesTransport(t *testing.T, fixture *successfulFilesLifecycle, cleanup roundTripFunc) roundTripFunc {
	t.Helper()
	var requests atomic.Int32
	return func(request *http.Request) (*http.Response, error) {
		switch requests.Add(1) {
		case 1:
			return filesStartResponse(request), nil
		case 2:
			response := geminiJSONResponse(request, `{"file":`+fixture.fileJSON("ACTIVE")+`}`)
			response.Header.Set("X-Goog-Upload-Status", "final")
			return response, nil
		case 3:
			return geminiJSONResponse(request, `{"embedding":{"values":[`+testVectorJSON(128)+`]}}`), nil
		case 4:
			require.Equal(t, http.MethodDelete, request.Method)
			return cleanup(request)
		default:
			return nil, errors.New("unexpected active Files lifecycle request")
		}
	}
}

func assertReceiptHasNoFileEvidence(t *testing.T, receipt Receipt, data []byte, fixture *successfulFilesLifecycle) {
	t.Helper()
	encoded := fmt.Sprintf("%#v", receipt)
	for _, forbidden := range []string{
		fixture.filename, fixture.fileName, fixture.fileURI, string(data),
		sha256Hex(data), base64.StdEncoding.EncodeToString(data),
	} {
		assert.NotContains(t, encoded, forbidden)
	}
}

func sha256Hex(data []byte) string {
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}

type sensitiveTransportError struct{ text string }

func (failure *sensitiveTransportError) Error() string { return failure.text }

type sensitiveCleanupError struct{ text string }

func (failure *sensitiveCleanupError) Error() string { return failure.text }

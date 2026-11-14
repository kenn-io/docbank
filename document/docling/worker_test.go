package docling

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/internal/providerutil"
	"go.kenn.io/docbank/document/media/mediatest"
	"go.kenn.io/docbank/document/workerprotocol"
	"go.kenn.io/docbank/internal/conversionworker"
	"hegel.dev/go/hegel"
)

type workerServer struct {
	posts, lookups, results, readiness atomic.Int32
	accepted, offline, lostResponse    atomic.Bool
	pending                            atomic.Bool
	key                                string
	result                             any
}

// workerSubmitResponseBody delays delivery until after the executor's response check.
type workerSubmitResponseBody struct {
	io.ReadCloser

	closeStarted chan struct{}
	release      <-chan struct{}
	closeOnce    sync.Once
	closeErr     error
}

func (body *workerSubmitResponseBody) Close() error {
	body.closeOnce.Do(func() {
		close(body.closeStarted)
		<-body.release
		body.closeErr = body.ReadCloser.Close()
	})
	return body.closeErr
}

func workerHTTP(t *testing.T, result any) (*workerServer, string, *http.Client) {
	t.Helper()
	state := &workerServer{result: result}
	id := strings.Repeat("a", 32)
	origin, client := inMemoryServer(t, http.HandlerFunc(func(out http.ResponseWriter, r *http.Request) {
		out.Header().Set(workerprotocol.ProtocolHeader, workerprotocol.Contract)
		switch {
		case strings.HasPrefix(r.URL.Path, workerprotocol.LookupPath):
			state.lookups.Add(1)
			if state.accepted.Load() && strings.TrimPrefix(r.URL.Path, workerprotocol.LookupPath) == state.key {
				writeJSON(t, out, doclingTask(id, "success"))
				return
			}
			http.NotFound(out, r)
		case r.URL.Path == workerprotocol.ReadyPath:
			state.readiness.Add(1)
			if state.offline.Load() {
				out.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			writeJSON(t, out, map[string]string{"protocol": workerprotocol.Contract})
		case r.URL.Path == workerprotocol.SubmitPath:
			assert.Len(t, r.Header.Get("Idempotency-Key"), 64)
			assert.Len(t, r.Header.Get(workerprotocol.SourceSHA256Header), 64)
			_, err := io.Copy(io.Discard, r.Body)
			assert.NoError(t, err)
			state.posts.Add(1)
			state.key = r.Header.Get("Idempotency-Key")
			state.accepted.Store(true)
			if state.lostResponse.Swap(false) {
				out.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			writeJSON(t, out, doclingTask(id, "pending"))
		case r.URL.Path == workerprotocol.PollPath+id:
			if state.pending.Load() {
				writeJSON(t, out, doclingTask(id, "pending"))
				return
			}
			writeJSON(t, out, doclingTask(id, "success"))
		case r.URL.Path == workerprotocol.ResultPath+id:
			state.results.Add(1)
			writeJSON(t, out, state.result)
		default:
			http.NotFound(out, r)
		}
	}))
	return state, origin, client
}
func workerFixture(t *testing.T) fixture {
	t.Helper()
	f := newFixture(t, "pdf", "application/pdf", "report.pdf", []byte("synthetic PDF source"))
	descriptor, err := WorkerDocumentDescriptor()
	require.NoError(t, err)
	f.descriptor = descriptor
	f.authorization.ProviderID = descriptor.ID
	f.authorization.DescriptorFingerprint = descriptor.Fingerprint
	f.authorization.PolicyFingerprint = descriptor.PolicyFingerprint
	return f
}
func workerProfile(origin string, descriptor document.RenditionDescriptor) Profile {
	return Profile{Origin: origin, Descriptor: descriptor, SecretBinding: "worker", RequestTimeout: time.Second, TotalTimeout: time.Minute, PollInterval: time.Millisecond, MaxPollAttempts: 4, MaxDocumentBytes: 1 << 20, MaxResponseBytes: 1 << 20}
}
func workerSnapshot(t *testing.T, f fixture) document.RenditionExecutionSnapshotV1 {
	t.Helper()
	evidence, err := document.NewEvidencePolicy(100_000)
	require.NoError(t, err)
	rendition, err := document.NewRenditionPolicy(document.RenditionLimits{MaxDocumentChars: 100_000, MaxUnitRunes: 1000, MaxSegmentRunes: 100})
	require.NoError(t, err)
	identity, err := document.NewRenditionExecutionIdentityV1(f.metadata, f.authorization, evidence, rendition)
	require.NoError(t, err)
	snapshot, err := document.NewRenditionExecutionSnapshotV1(identity, f.authorization)
	require.NoError(t, err)
	return snapshot
}
func workerDocumentResult() any {
	var result any
	_ = json.Unmarshal(recordedPagesResponse, &result)
	return result
}
func TestWorkerDocumentCheckpointsAndResumesWithoutUpload(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := workerFixture(t)
		state, origin, httpClient := workerHTTP(t, workerDocumentResult())
		client, err := NewWorker(workerProfile(origin, f.descriptor), testSecrets{"worker": "synthetic-worker-key"}, httpClient)
		require.NoError(t, err)
		var handle document.RenditionResumeHandle
		stop := errors.New("synthetic checkpoint interruption")
		_, err = document.RenderRenditionWithResume(t.Context(), client, f.upload(), f.authorization, nil, func(h document.RenditionResumeHandle) error { handle = h; return stop })
		require.ErrorIs(t, err, stop)
		require.NotEmpty(t, handle.Value)
		require.LessOrEqual(t, len(handle.Value), 512)
		require.Zero(t, state.results.Load())
		snapshot := workerSnapshot(t, f)
		time.Sleep(11 * time.Minute)
		client, err = NewWorker(workerProfile(origin, f.descriptor), testSecrets{"worker": "synthetic-worker-key"}, httpClient)
		require.NoError(t, err)
		result, err := document.ResumeRendition(t.Context(), client, snapshot, handle, nil)
		require.NoError(t, err)
		require.NoError(t, document.ValidateRenditionResult(f.descriptor, f.authorization, result))
		require.Equal(t, int64(len(f.source)), result.Receipt.Usage.InputBytes)
		require.Equal(t, int32(1), state.posts.Load())
		require.Equal(t, "first page", result.Evidence.Units[0].Text)
	})
}
func TestWorkerReconcilesLostResponseBeforeReadiness(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := workerFixture(t)
		state, origin, httpClient := workerHTTP(t, workerDocumentResult())
		state.lostResponse.Store(true)
		client, err := NewWorker(workerProfile(origin, f.descriptor), testSecrets{"worker": "synthetic-worker-key"}, httpClient)
		require.NoError(t, err)
		_, err = document.RenderRendition(t.Context(), client, f.upload(), f.authorization)
		require.Error(t, err)
		require.True(t, document.IsRenditionProviderErrorRetryable(err))
		state.offline.Store(true)
		time.Sleep(time.Minute)
		f.authorization.AuthorizedAt = time.Now().UTC().Format(providerutil.TimestampForm)
		f.authorization.ExpiresAt = time.Now().UTC().Add(10 * time.Minute).Format(providerutil.TimestampForm)
		result, err := document.RenderRendition(t.Context(), client, f.upload(), f.authorization)
		require.NoError(t, err)
		require.Equal(t, int32(1), state.posts.Load())
		require.Equal(t, int32(1), state.readiness.Load())
		require.Equal(t, "first page", result.Evidence.Units[0].Text)
	})
}

func TestWorkerAcceptedResponseAfterExpiryIsNotCheckpointed(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := workerFixture(t)
		started := time.Now().UTC()
		expires := started.Add(10 * time.Second)
		f.authorization.AuthorizedAt = started.Add(-time.Second).Format(providerutil.TimestampForm)
		f.authorization.ExpiresAt = expires.Format(providerutil.TimestampForm)
		state, origin, httpClient := workerHTTP(t, workerDocumentResult())
		closeStarted, releaseClose := make(chan struct{}), make(chan struct{})
		defer func() {
			select {
			case <-releaseClose:
			default:
				close(releaseClose)
			}
		}()
		base := httpClient.Transport
		httpClient.Transport = roundTripperFunc(func(request *http.Request) (*http.Response, error) {
			response, err := base.RoundTrip(request)
			if err == nil && request.Method == http.MethodPost && request.URL.Path == workerprotocol.SubmitPath {
				response.Body = &workerSubmitResponseBody{ReadCloser: response.Body, closeStarted: closeStarted, release: releaseClose}
			}
			return response, err
		})
		profile := workerProfile(origin, f.descriptor)
		profile.RequestTimeout = time.Minute
		client, err := NewWorker(profile, testSecrets{"worker": "synthetic-worker-key"}, httpClient)
		if err != nil {
			t.Fatal(err)
		}
		var handle document.RenditionResumeHandle
		finished := make(chan error, 1)
		go func() {
			_, renderErr := document.RenderRenditionWithResume(t.Context(), client, f.upload(), f.authorization, nil,
				func(value document.RenditionResumeHandle) error { handle = value; return nil })
			finished <- renderErr
		}()

		select {
		case <-closeStarted:
		case renderErr := <-finished:
			t.Fatalf("submission ended before its accepted response was processed: %v", renderErr)
		}
		require.True(t, state.accepted.Load(), "the worker must accept the submission before expiry")
		require.Equal(t, int32(1), state.posts.Load())
		require.True(t, time.Now().Before(expires), "the response must pass the request check before expiry")
		timer := time.NewTimer(time.Until(expires) + time.Nanosecond)
		defer timer.Stop()
		<-timer.C
		require.False(t, time.Now().Before(expires))
		close(releaseClose)

		err = <-finished
		require.ErrorIs(t, err, document.ErrRenditionAuthorizationExpired)
		require.Empty(t, handle.Value, "an expired authorization must not persist a resume handle")
		require.Equal(t, int32(1), state.posts.Load())

		refreshed := f.authorization
		refreshed.AuthorizedAt = time.Now().UTC().Format(providerutil.TimestampForm)
		refreshed.ExpiresAt = time.Now().UTC().Add(time.Minute).Format(providerutil.TimestampForm)
		result, err := document.RenderRendition(t.Context(), client, f.upload(), refreshed)
		require.NoError(t, err)
		require.NoError(t, document.ValidateRenditionResult(f.descriptor, refreshed, result))
		require.Equal(t, int32(2), state.lookups.Load(), "the fresh retry must look up the accepted key")
		require.Equal(t, int32(1), state.readiness.Load(), "a recovered task must be found before readiness")
		require.Equal(t, int32(1), state.posts.Load(), "the fresh retry must not submit the source again")
	})
}
func TestWorkerOfflineDoesNotReadOrTransferSource(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := workerFixture(t)
		state, origin, httpClient := workerHTTP(t, workerDocumentResult())
		state.offline.Store(true)
		client, err := NewWorker(workerProfile(origin, f.descriptor), testSecrets{"worker": "synthetic-worker-key"}, httpClient)
		require.NoError(t, err)
		var reads int
		upload := &testUpload{metadata: f.metadata, Reader: readerFunc(func([]byte) (int, error) { reads++; return 0, io.EOF })}
		_, err = document.RenderRendition(t.Context(), client, upload, f.authorization)
		require.Error(t, err)
		require.True(t, document.IsRenditionProviderErrorRetryable(err))
		require.Zero(t, reads)
		require.Zero(t, state.posts.Load())
	})
}
func TestWorkerRejectsEncryptedPDFBeforeNetwork(t *testing.T) {
	source, err := mediatest.EncryptedPDF("open", "owner", 128)
	require.NoError(t, err)
	f := workerFixture(t)
	native := newFixture(t, "pdf", "application/pdf", "report.pdf", source)
	native.descriptor = f.descriptor
	native.authorization.ProviderID = f.descriptor.ID
	native.authorization.DescriptorFingerprint = f.descriptor.Fingerprint
	native.authorization.PolicyFingerprint = f.descriptor.PolicyFingerprint
	upload := mediatest.AuthorizePDF(t, source, native.metadata, &native.authorization)
	var requests atomic.Int32
	origin, httpClient := inMemoryServer(t, http.HandlerFunc(func(out http.ResponseWriter, r *http.Request) { requests.Add(1); http.NotFound(out, r) }))
	client, err := NewWorker(workerProfile(origin, f.descriptor), testSecrets{"worker": "synthetic-worker-key"}, httpClient)
	require.NoError(t, err)
	_, err = document.RenderRendition(t.Context(), client, upload, native.authorization)
	require.Error(t, err)
	require.Zero(t, requests.Load())
}
func TestWorkerASRResumesProvenDurationWithoutSource(t *testing.T) {
	for _, kind := range []struct {
		mediaType, filename string
		source              []byte
	}{{"audio/wav", "asr-spoken.wav", asrSpokenWAV}, {"audio/mpeg", "asr-spoken.mp3", asrSpokenMP3}} {
		t.Run(kind.mediaType, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				descriptor, err := WorkerASRDescriptor(100_000)
				require.NoError(t, err)
				f := newASRFixture(t, kind.mediaType, kind.filename, kind.source, descriptor)
				state, origin, httpClient := workerHTTP(t, asrResultResponse(kind.filename, "1.10.0", 3.06))
				client, err := NewWorkerASR(ASRProfile{Profile: workerProfile(origin, descriptor), MaxTranscriptChars: 100_000}, testSecrets{"worker": "synthetic-worker-key"}, httpClient)
				require.NoError(t, err)
				var handle document.RenditionResumeHandle
				stop := errors.New("synthetic checkpoint failure")
				_, err = document.RenderRenditionWithResume(t.Context(), client, f.upload(), f.authorization, nil, func(h document.RenditionResumeHandle) error { handle = h; return stop })
				require.ErrorIs(t, err, stop)
				snapshot := workerSnapshot(t, f.fixture)
				time.Sleep(11 * time.Minute)
				result, err := document.ResumeRendition(t.Context(), client, snapshot, handle, nil)
				require.NoError(t, err)
				require.Equal(t, int64(3060), result.Evidence.Units[0].Locator.End)
				require.Equal(t, int64(len(kind.source)), result.Receipt.Usage.InputBytes)
				require.Equal(t, int32(1), state.posts.Load())
				require.NoError(t, document.ValidateRenditionResult(descriptor, f.authorization, result))
			})
		})
	}
}
func TestWorkerSubmissionKeyIgnoresOnlyAuthorizationInterval(t *testing.T) {
	f := workerFixture(t)
	fields := [][2]string{{"to_formats", "md"}, {"to_formats", "json"}, {"target_type", "inbody"}}
	first, err := workerprotocol.SubmissionKey(f.metadata, f.authorization, fields)
	require.NoError(t, err)
	hegel.Test(t, func(ht *hegel.T) {
		shift := hegel.Draw(ht, hegel.Integers[int64](-1<<63, 1<<63-1))
		auth := f.authorization
		start, err := time.Parse(providerutil.TimestampForm, auth.AuthorizedAt)
		require.NoError(ht, err)
		end, err := time.Parse(providerutil.TimestampForm, auth.ExpiresAt)
		require.NoError(ht, err)
		auth.AuthorizedAt = start.Add(time.Duration(shift)).Format(providerutil.TimestampForm)
		auth.ExpiresAt = end.Add(time.Duration(shift)).Format(providerutil.TimestampForm)
		second, err := workerprotocol.SubmissionKey(f.metadata, auth, fields)
		require.NoError(ht, err)
		require.Equal(ht, first, second)
	})
}
func TestWorkerSubmissionKeyBindsRequestFingerprint(t *testing.T) {
	f := workerFixture(t)
	fields := [][2]string{{"to_formats", "json"}, {"target_type", "inbody"}}
	first, err := workerprotocol.SubmissionKey(f.metadata, f.authorization, fields)
	require.NoError(t, err)
	changed := f.authorization
	changed.RenditionRequestFingerprint = strings.Repeat("5", 64)
	different, err := workerprotocol.SubmissionKey(f.metadata, changed, fields)
	require.NoError(t, err)
	require.NotEqual(t, first, different)
}

func TestWorkerActualGatewayLostReplyAndResume(t *testing.T) {
	for _, testCase := range []struct {
		name, status, wantError string
		pageError, audio        bool
	}{
		{name: "complete PDF", status: "success"},
		{name: "partial PDF", status: "partial_success", pageError: true},
		{name: "partial PDF without omissions", status: "partial_success", wantError: "no page omissions"},
		{name: "complete ASR", status: "success", audio: true},
		{name: "partial ASR", status: "partial_success", audio: true, wantError: "ASR returned partial success"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Setenv("DOCBANK_LOCK_DIR", t.TempDir())
			f := workerFixture(t)
			upload := f.upload
			backendResult := workerDocumentResult()
			if testCase.audio {
				descriptor, err := WorkerASRDescriptor(100_000)
				require.NoError(t, err)
				audio := newASRFixture(t, "audio/wav", "asr-spoken.wav", asrSpokenWAV, descriptor)
				f, upload = audio.fixture, audio.upload
				backendResult = asrResultResponse("asr-spoken.wav", "1.10.0", 3.06)
			} else if testCase.pageError {
				backendResult.(map[string]any)["errors"] = []any{map[string]any{"page_no": 1}}
			}
			var backendPosts atomic.Int32
			backend := httptest.NewServer(http.HandlerFunc(func(out http.ResponseWriter, r *http.Request) {
				out.Header().Set("Content-Type", "application/json")
				switch r.URL.Path {
				case "/ready":
					out.WriteHeader(http.StatusOK)
				case workerprotocol.SubmitPath:
					_, err := io.Copy(io.Discard, r.Body)
					assert.NoError(t, err)
					backendPosts.Add(1)
					writeJSON(t, out, doclingTask("backend-task", "pending"))
				case workerprotocol.PollPath + "backend-task":
					writeJSON(t, out, doclingTask("backend-task", testCase.status))
				case workerprotocol.ResultPath + "backend-task":
					writeJSON(t, out, backendResult)
				default:
					http.NotFound(out, r)
				}
			}))
			t.Cleanup(backend.Close)
			options := conversionworker.Options{Directory: filepath.Join(t.TempDir(), "spool"), APIKey: "synthetic-worker-key", BackendURL: backend.URL, BackendKey: "synthetic-backend-key", Client: backend.Client()}
			gateway, err := conversionworker.Open(t.Context(), options)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, gateway.Close(context.WithoutCancel(t.Context()))) })
			var loseReply atomic.Bool
			loseReply.Store(true)
			origin, transport := inMemoryServer(t, http.HandlerFunc(func(out http.ResponseWriter, r *http.Request) {
				if r.URL.Path == workerprotocol.SubmitPath && loseReply.Swap(false) {
					// The real gateway commits input, but the client sees only a server failure.
					recorder := httptest.NewRecorder()
					gateway.ServeHTTP(recorder, r)
					assert.Equal(t, http.StatusAccepted, recorder.Code)
					out.Header().Set(workerprotocol.ProtocolHeader, workerprotocol.Contract)
					out.WriteHeader(http.StatusServiceUnavailable)
					return
				}
				gateway.ServeHTTP(out, r)
			}))
			var client document.ResumableRenditionProvider
			if testCase.audio {
				client, err = NewWorkerASR(ASRProfile{Profile: workerProfile(origin, f.descriptor), MaxTranscriptChars: 100_000}, testSecrets{"worker": "synthetic-worker-key"}, transport)
			} else {
				client, err = NewWorker(workerProfile(origin, f.descriptor), testSecrets{"worker": "synthetic-worker-key"}, transport)
			}
			require.NoError(t, err)
			_, err = document.RenderRendition(t.Context(), client, upload(), f.authorization)
			require.True(t, document.IsRenditionProviderErrorRetryable(err), "%v", err)
			require.False(t, loseReply.Load(), "the gateway must have accepted before the reply is lost")
			worked, err := gateway.RunOnce(t.Context())
			require.NoError(t, err)
			require.True(t, worked)
			backend.Close()
			// Restart the durable spool. Retained lookup/result must not need Docling.
			require.NoError(t, gateway.Close(t.Context()))
			gateway, err = conversionworker.Open(t.Context(), options)
			require.NoError(t, err)
			var handle document.RenditionResumeHandle
			stop := errors.New("synthetic publication interruption")
			_, err = document.RenderRenditionWithResume(t.Context(), client, upload(), f.authorization, nil, func(h document.RenditionResumeHandle) error { handle = h; return stop })
			require.ErrorIs(t, err, stop)
			result, err := document.ResumeRendition(t.Context(), client, workerSnapshot(t, f), handle, nil)
			require.Equal(t, int32(1), backendPosts.Load())
			if testCase.wantError != "" {
				require.ErrorContains(t, err, testCase.wantError)
				return
			}
			require.NoError(t, err)
			require.NoError(t, document.ValidateRenditionResult(f.descriptor, f.authorization, result))
			if testCase.status == "partial_success" {
				require.Equal(t, document.EvidencePartial, result.Evidence.Completeness)
				require.Contains(t, result.Receipt.Warnings, "partial_success")
				require.Len(t, result.Evidence.Units[0].Omissions, 1)
				require.Equal(t, "provider_output", result.Evidence.Units[0].Omissions[0].Field)
			}
		})
	}
}

func TestWorkerPendingExpiryKeepsHistoricalResume(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := workerFixture(t)
		f.authorization.ExpiresAt = time.Now().UTC().Add(time.Second).Format(providerutil.TimestampForm)
		state, origin, transport := workerHTTP(t, workerDocumentResult())
		state.pending.Store(true)
		profile := workerProfile(origin, f.descriptor)
		profile.PollInterval = 2 * time.Second
		client, err := NewWorker(profile, testSecrets{"worker": "synthetic-worker-key"}, transport)
		require.NoError(t, err)
		var handle document.RenditionResumeHandle
		_, err = document.RenderRenditionWithResume(t.Context(), client, f.upload(), f.authorization, nil, func(h document.RenditionResumeHandle) error { handle = h; return nil })
		require.NotEmpty(t, handle.Value)
		require.ErrorIs(t, err, document.ErrRenditionAuthorizationExpired)
		state.pending.Store(false)
		result, err := document.ResumeRendition(t.Context(), client, workerSnapshot(t, f), handle, nil)
		require.NoError(t, err)
		require.NoError(t, document.ValidateRenditionResult(f.descriptor, f.authorization, result))
		require.Equal(t, int32(1), state.posts.Load())
	})
}

func TestWorkerResumeEnvelopeRoundTrips(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		raw := hegel.Draw(ht, hegel.Lists(hegel.Integers[byte](0, 255)))
		raw = raw[:min(len(raw), 1024)]
		hash := providerutil.SHA256Hex(raw)
		started := hegel.Draw(ht, hegel.Integers[int64](1, math.MaxInt64))
		accepted := hegel.Draw(ht, hegel.Integers[int64](started, math.MaxInt64))
		duration := hegel.Draw(ht, hegel.Integers[int64](0, (24*time.Hour).Milliseconds()))
		original := workerResume{id: hash[:32], authorization: hash, filenameSHA256: hash, started: time.Unix(0, started).UTC(), accepted: time.Unix(0, accepted).UTC(), durationMS: duration}
		encoded := encodeWorkerResume(original)
		require.LessOrEqual(ht, len(encoded), 512)
		decoded, err := parseWorkerResume(encoded)
		require.NoError(ht, err)
		require.Equal(ht, original, decoded)
	})
}
func TestWorkerResumeEnvelopeRejectsNoncanonicalParts(t *testing.T) {
	valid := encodeWorkerResume(workerResume{id: strings.Repeat("a", 32), authorization: strings.Repeat("b", 64), filenameSHA256: strings.Repeat("c", 64), started: time.Unix(0, 1).UTC(), accepted: time.Unix(0, 2).UTC()})
	for _, bad := range []string{"", strings.Repeat("x", 513), valid + ".extra", strings.Replace(valid, "cw1", "cw2", 1), strings.Replace(valid, ".1.2.", ".01.2.", 1), strings.Replace(valid, ".1.2.", ".2.1.", 1), strings.ToUpper(valid)} {
		_, err := parseWorkerResume(bad)
		require.Error(t, err, "%q", bad)
	}
}
func TestWorkerConstructorBoundsCoverIntegerDomain(t *testing.T) {
	descriptor, err := WorkerDocumentDescriptor()
	require.NoError(t, err)
	hegel.Test(t, func(ht *hegel.T) {
		source := hegel.Draw(ht, hegel.Integers[int64](math.MinInt64, math.MaxInt64))
		response := hegel.Draw(ht, hegel.Integers[int64](math.MinInt64, math.MaxInt64))
		profile := workerProfile("http://127.0.0.1:5002", descriptor)
		profile.MaxDocumentBytes = source
		profile.MaxResponseBytes = response
		_, err := NewWorker(profile, testSecrets{"worker": "synthetic-worker-key"}, &http.Client{})
		valid := (source == 0 || source > 0 && source <= 100<<20) && (response == 0 || response > 0 && response <= 128<<20)
		if valid {
			require.NoError(ht, err)
		} else {
			require.Error(ht, err)
		}
	})
}

func TestWorkerTaskRejectsInvalidBackendStatus(t *testing.T) {
	for _, testCase := range []struct{ status, backend string }{
		{status: "success", backend: "pending"},
		{status: "success", backend: "failure"},
		{status: "success", backend: "unknown"},
		{status: "pending", backend: "partial_success"},
	} {
		t.Run(testCase.status+"/"+testCase.backend, func(t *testing.T) {
			body, err := json.Marshal(map[string]string{
				"task_id": strings.Repeat("a", 32), "task_status": testCase.status,
				"backend_task_status": testCase.backend,
			})
			require.NoError(t, err)
			_, err = parseTaskForProtocol(body, true)
			require.ErrorContains(t, err, "worker backend task status is invalid")
		})
	}
}

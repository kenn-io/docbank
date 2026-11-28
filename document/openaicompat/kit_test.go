package openaicompat

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/internal/providerutil"
	"go.kenn.io/docbank/document/internal/uploadcapability"
	"go.kenn.io/docbank/document/media"
	"go.kenn.io/kit/embedclient"
	"go.kenn.io/kit/embedconfig"
	"go.kenn.io/kit/embedserver"
)

type kitBackend struct{ inputs []embedclient.Input }

func (b *kitBackend) Encode(_ context.Context, _ int, inputs []embedclient.Input) ([][]float32, error) {
	b.inputs = append(b.inputs, inputs...)
	out := make([][]float32, len(inputs))
	for i := range out {
		out[i] = []float32{1, 0, 0}
	}
	return out, nil
}

type kitUpload struct {
	io.Reader

	metadata document.AuthorizedUploadMetadata
	proof    document.UploadCapability
}

func (u *kitUpload) Close() error                                { return nil }
func (u *kitUpload) Metadata() document.AuthorizedUploadMetadata { return u.metadata }
func (u *kitUpload) CapabilityProof() document.UploadCapability  { return u.proof }

func TestKitMediaAndQuery(t *testing.T) {
	contract := modelInput(t, document.ModelInputContractConfig{Profile: document.ModelInputProfileNomic})
	profile := testProfile(t, contract)
	profile.API = embedconfig.APIKit
	profile.Descriptor.InputKinds = []document.EmbeddingInputKind{document.EmbeddingInputOriginalFile}
	profile.Descriptor = descriptorFor(t, profile)
	backend := &kitBackend{}
	server := httptest.NewServer(embedserver.Handler(backend, []string{"synthetic-model"}))
	defer server.Close()
	profile.Origin = server.URL
	profile.Descriptor = descriptorFor(t, profile)
	client := newTestClient(t, profile, nil, http.DefaultTransport)
	for _, tc := range []struct {
		name, family, media string
		pages, duration     int64
		accept              bool
	}{
		{"PNG", "image", "image/png", 0, 0, true}, {"JPEG", "image", "image/jpeg", 0, 0, true},
		{"WebP", "image", "image/webp", 0, 0, true}, {"GIF", "image", "image/gif", 0, 0, false},
		{"six pages", "pdf", "application/pdf", 6, 0, true}, {"two minutes", "video", "video/mp4", 0, 120000, true},
		{"seven pages", "pdf", "application/pdf", 7, 0, false}, {"video over limit", "video", "video/mp4", 0, 120001, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data := []byte("synthetic media")
			hash := providerutil.SHA256Hex(data)
			checksum := strings.Repeat("a", 64)
			metadata := document.AuthorizedUploadMetadata{MediaFamily: tc.family, MediaType: tc.media, ByteLength: int64(len(data)), SHA256: hash, CapabilityRecordChecksum: checksum, ProviderMetadataChecksum: checksum, InputKind: document.RenditionInputOriginalFile}
			facts := document.UploadCapabilityFacts{MediaFamily: tc.family, MediaType: tc.media, SourceBytes: int64(len(data)), SourceSHA256: hash, Checksum: checksum, DescriptorFingerprint: profile.Descriptor.Fingerprint, Pages: tc.pages, DurationMS: tc.duration, Pixels: 1, Frames: 1}
			upload := &kitUpload{Reader: bytes.NewReader(data), metadata: metadata, proof: uploadcapability.New(facts, nil)}
			before := len(backend.inputs)
			_, err := client.Embed(t.Context(), []document.EmbeddingInput{{Key: "file", Role: document.EmbeddingRoleDocument, Kind: document.EmbeddingInputOriginalFile, Source: upload}}, testAuthorization(profile.Descriptor))
			if !tc.accept {
				require.ErrorIs(t, err, ErrPermanentResponse)
				require.Len(t, backend.inputs, before)
				return
			}
			require.NoError(t, err)
			require.Len(t, backend.inputs, before+1)
			require.Equal(t, embedclient.Input{Role: embedconfig.RoleDocument, MediaType: tc.media, Data: data}, backend.inputs[before])
		})
	}
	_, err := client.Embed(t.Context(), []document.EmbeddingInput{{Key: "query", Role: document.EmbeddingRoleQuery, Kind: document.EmbeddingInputQueryText, Text: "receipt"}}, testAuthorization(profile.Descriptor))
	require.NoError(t, err)
	require.Equal(t, embedclient.Input{Role: embedconfig.RoleQuery, Text: "search_query: receipt"}, backend.inputs[len(backend.inputs)-1])
}

type blockingKitUpload struct {
	metadata         document.AuthorizedUploadMetadata
	proof            document.UploadCapability
	started, stopped chan struct{}
}

func (u *blockingKitUpload) Read([]byte) (int, error) {
	close(u.started)
	<-u.stopped
	return 0, io.EOF
}
func (u *blockingKitUpload) Close() error                                { close(u.stopped); return nil }
func (u *blockingKitUpload) Metadata() document.AuthorizedUploadMetadata { return u.metadata }
func (u *blockingKitUpload) CapabilityProof() document.UploadCapability  { return u.proof }

func TestKitCancellationInterruptsUpload(t *testing.T) {
	contract := modelInput(t, document.ModelInputContractConfig{Profile: document.ModelInputProfileNomic})
	profile := testProfile(t, contract)
	profile.API = embedconfig.APIKit
	profile.Descriptor.InputKinds = []document.EmbeddingInputKind{document.EmbeddingInputOriginalFile}
	profile.Descriptor = descriptorFor(t, profile)
	checksum := strings.Repeat("a", 64)
	metadata := document.AuthorizedUploadMetadata{MediaFamily: "image", MediaType: "image/png", ByteLength: 1, SHA256: checksum, CapabilityRecordChecksum: checksum, ProviderMetadataChecksum: checksum, InputKind: document.RenditionInputOriginalFile}
	facts := document.UploadCapabilityFacts{MediaFamily: "image", MediaType: "image/png", SourceBytes: 1, SourceSHA256: checksum, Checksum: checksum, DescriptorFingerprint: profile.Descriptor.Fingerprint, Pixels: 1, Frames: 1}
	upload := &blockingKitUpload{metadata: metadata, proof: uploadcapability.New(facts, nil), started: make(chan struct{}), stopped: make(chan struct{})}
	client := newTestClient(t, profile, nil, roundTripFunc(func(*http.Request) (*http.Response, error) { t.Error("unexpected request"); return nil, io.EOF }))
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		_, err := client.Embed(ctx, []document.EmbeddingInput{{Key: "file", Role: document.EmbeddingRoleDocument, Kind: document.EmbeddingInputOriginalFile, Source: upload}}, testAuthorization(profile.Descriptor))
		done <- err
	}()
	<-upload.started
	cancel()
	require.ErrorIs(t, <-done, context.Canceled)
}

func TestKitMediaIdentity(t *testing.T) {
	profile := testProfile(t, modelInput(t, document.ModelInputContractConfig{Profile: document.ModelInputProfileNomic}))
	profile.Descriptor = descriptorFor(t, profile)
	base := newTestClient(t, profile, nil, http.DefaultTransport)
	baseIdentity, err := base.VectorIdentity()
	require.NoError(t, err)
	basePolicy := profile.Descriptor.PolicyFingerprint
	profile.API = embedconfig.APIKit
	profile.Descriptor = descriptorFor(t, profile)
	client := newTestClient(t, profile, nil, http.DefaultTransport)
	identity, err := client.VectorIdentity()
	require.NoError(t, err)
	require.NotEqual(t, basePolicy, profile.Descriptor.PolicyFingerprint)
	require.NotEqual(t, baseIdentity, identity)
}

func TestKitVideoRequestOmitsAudioExtraction(t *testing.T) {
	var raw []byte
	vertexServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var err error
		raw, err = io.ReadAll(r.Body)
		if !assert.NoError(t, err) {
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, err = w.Write([]byte(`{"embedding":{"values":[1,0,0]}}`))
		assert.NoError(t, err)
	}))
	defer vertexServer.Close()
	vertex, err := embedclient.New(embedclient.Options{Model: embedconfig.Model{Name: "gemini-embedding-2", Dimensions: 3, Metric: embedconfig.MetricCosine, Normalization: embedconfig.NormalizationNone}, Deployment: embedconfig.Deployment{API: embedconfig.APIVertex, BaseURL: vertexServer.URL + "/publishers/google/models"}, Roles: embedconfig.Roles{QueryPrefix: "query: "}})
	require.NoError(t, err)
	kitServer := httptest.NewServer(embedserver.Handler(vertex, []string{"synthetic-model"}))
	defer kitServer.Close()
	profile := testProfile(t, modelInput(t, document.ModelInputContractConfig{Profile: document.ModelInputProfileNomic}))
	profile.API, profile.Origin = embedconfig.APIKit, kitServer.URL
	profile.Descriptor.InputKinds = []document.EmbeddingInputKind{document.EmbeddingInputOriginalFile}
	profile.Descriptor = descriptorFor(t, profile)
	client := newTestClient(t, profile, nil, http.DefaultTransport)
	data := []byte{1, 2, 3}
	hash, checksum := providerutil.SHA256Hex(data), strings.Repeat("a", 64)
	metadata := document.AuthorizedUploadMetadata{MediaFamily: "video", MediaType: "video/mp4", ByteLength: int64(len(data)), SHA256: hash, CapabilityRecordChecksum: checksum, ProviderMetadataChecksum: checksum, InputKind: document.RenditionInputOriginalFile}
	facts := document.UploadCapabilityFacts{MediaFamily: "video", MediaType: "video/mp4", SourceBytes: int64(len(data)), SourceSHA256: hash, Checksum: checksum, DescriptorFingerprint: profile.Descriptor.Fingerprint, DurationMS: 1000}
	upload := &kitUpload{Reader: bytes.NewReader(data), metadata: metadata, proof: uploadcapability.New(facts, nil)}
	_, err = client.Embed(t.Context(), []document.EmbeddingInput{{Key: "video", Role: document.EmbeddingRoleDocument, Kind: document.EmbeddingInputOriginalFile, Source: upload}}, testAuthorization(profile.Descriptor))
	require.NoError(t, err)
	require.NotContains(t, string(raw), "audio_track_extraction")
	require.NotContains(t, string(raw), "audioTrackExtraction")
	var body map[string]any
	require.NoError(t, json.Unmarshal(raw, &body))
	require.Equal(t, map[string]any{"parts": []any{map[string]any{"inlineData": map[string]any{"mimeType": "video/mp4", "data": "AQID"}}}}, body["content"])
}

func TestKitResponseContract(t *testing.T) {
	profile := testProfile(t, modelInput(t, document.ModelInputContractConfig{Profile: document.ModelInputProfileNomic}))
	profile.API = embedconfig.APIKit
	profile.Descriptor = descriptorFor(t, profile)
	for _, tc := range []struct {
		name, body string
		want       error
	}{
		{"optional fields", `{"model":"synthetic-model","object":"custom","usage":{"custom_tokens":42},"data":[{"index":0,"object":"custom","embedding":[1,0,0]}]}`, nil},
		{"wrong model", `{"model":"wrong","data":[{"index":0,"embedding":[1,0,0]}]}`, ErrMalformedResponse},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := newTestClient(t, profile, nil, roundTripFunc(func(request *http.Request) (*http.Response, error) {
				return jsonResponse(request, http.StatusOK, []byte(tc.body)), nil
			}))
			_, err := client.Embed(t.Context(), testInputs()[:1], testAuthorization(profile.Descriptor))
			if tc.want != nil {
				require.ErrorIs(t, err, tc.want)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestKitUpstreamCredentials(t *testing.T) {
	profile := testProfile(t, modelInput(t, document.ModelInputContractConfig{Profile: document.ModelInputProfileNomic}))
	profile.API = embedconfig.APIKit
	profile.Descriptor = descriptorFor(t, profile)
	for _, tc := range []struct {
		name   string
		status int
		body   string
		want   error
		api    embedconfig.API
	}{
		{"credentials", http.StatusBadGateway, `{"error":{"code":"upstream_credentials","message":"private"}}`, ErrUnauthorized, embedconfig.APIKit},
		{"unavailable", http.StatusBadGateway, `{"error":{"code":"unavailable","message":"private"}}`, ErrTransientResponse, embedconfig.APIKit},
		{"openai", http.StatusBadGateway, `{"error":{"code":"upstream_credentials"}}`, ErrTransientResponse, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			profile := profile
			profile.API = tc.api
			profile.Descriptor = descriptorFor(t, profile)
			client := newTestClient(t, profile, nil, roundTripFunc(func(request *http.Request) (*http.Response, error) {
				response := jsonResponse(request, tc.status, []byte(tc.body))
				if tc.name == "unavailable" {
					response.Header.Set("Retry-After", "2")
				}
				return response, nil
			}))
			_, err := client.Embed(t.Context(), testInputs()[:1], testAuthorization(profile.Descriptor))
			require.ErrorIs(t, err, tc.want)
			require.NotContains(t, err.Error(), "private")
			if tc.name == "unavailable" {
				delay, ok := RetryAfter(err)
				require.True(t, ok)
				require.Equal(t, 2*time.Second, delay)
			}
		})
	}
}

func TestKitRejectsMismatchedCapability(t *testing.T) {
	profile := testProfile(t, modelInput(t, document.ModelInputContractConfig{Profile: document.ModelInputProfileNomic}))
	profile.API = embedconfig.APIKit
	profile.Descriptor.InputKinds = []document.EmbeddingInputKind{document.EmbeddingInputOriginalFile}
	profile.Descriptor = descriptorFor(t, profile)
	data := []byte("synthetic")
	hash, checksum := providerutil.SHA256Hex(data), strings.Repeat("a", 64)
	metadata := document.AuthorizedUploadMetadata{MediaFamily: "image", MediaType: "image/png", ByteLength: int64(len(data)), SHA256: hash, CapabilityRecordChecksum: checksum, ProviderMetadataChecksum: checksum, InputKind: document.RenditionInputOriginalFile}
	facts := document.UploadCapabilityFacts{MediaFamily: "image", MediaType: "image/png", SourceBytes: int64(len(data)), SourceSHA256: hash, Checksum: checksum, DescriptorFingerprint: profile.Descriptor.Fingerprint, Pixels: 1, Frames: 1}
	for name, mutate := range map[string]func(*document.UploadCapabilityFacts){
		"descriptor": func(f *document.UploadCapabilityFacts) { f.DescriptorFingerprint = strings.Repeat("b", 64) },
		"metadata":   func(f *document.UploadCapabilityFacts) { f.SourceSHA256 = strings.Repeat("b", 64) },
	} {
		t.Run(name, func(t *testing.T) {
			changed := facts
			mutate(&changed)
			upload := &kitUpload{Reader: bytes.NewReader(data), metadata: metadata, proof: uploadcapability.New(changed, nil)}
			client := newTestClient(t, profile, nil, roundTripFunc(func(*http.Request) (*http.Response, error) { t.Error("unexpected request"); return nil, io.EOF }))
			_, err := client.Embed(t.Context(), []document.EmbeddingInput{{Key: "file", Role: document.EmbeddingRoleDocument, Kind: document.EmbeddingInputOriginalFile, Source: upload}}, testAuthorization(profile.Descriptor))
			require.ErrorIs(t, err, ErrPermanentResponse)
		})
	}
}

type kitCanceledResponse struct {
	ctx     context.Context
	started chan struct{}
}

func (body *kitCanceledResponse) Read([]byte) (int, error) {
	close(body.started)
	<-body.ctx.Done()
	return 0, body.ctx.Err()
}
func (*kitCanceledResponse) Close() error { return nil }

func TestKitGatewayCancellation(t *testing.T) {
	profile := testProfile(t, modelInput(t, document.ModelInputContractConfig{Profile: document.ModelInputProfileNomic}))
	profile.API = embedconfig.APIKit
	profile.Descriptor = descriptorFor(t, profile)
	started := make(chan struct{})
	client := newTestClient(t, profile, nil, roundTripFunc(func(request *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusBadGateway, Body: &kitCanceledResponse{ctx: request.Context(), started: started}, Header: make(http.Header)}, nil
	}))
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		_, err := client.Embed(ctx, testInputs()[:1], testAuthorization(profile.Descriptor))
		done <- err
	}()
	<-started
	cancel()
	require.ErrorIs(t, <-done, context.Canceled)
}

type kitReadCounter struct {
	io.Reader

	reads int
}

func (reader *kitReadCounter) Read(p []byte) (int, error) {
	reader.reads++
	return reader.Reader.Read(p)
}

func TestKitMediaByteLimits(t *testing.T) {
	for _, tc := range []struct {
		name string
		size int64
		want error
		read bool
	}{
		{"source at limit", media.MaxBytes, nil, true},
		{"source over limit", media.MaxBytes + 1, ErrPermanentResponse, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			profile := testProfile(t, modelInput(t, document.ModelInputContractConfig{Profile: document.ModelInputProfileNomic}))
			profile.API = embedconfig.APIKit
			profile.MaxInputBytes, profile.MaxRequestBytes = media.MaxBytes+1, 32<<20
			profile.Descriptor.InputKinds = []document.EmbeddingInputKind{document.EmbeddingInputOriginalFile}
			profile.Descriptor = descriptorFor(t, profile)
			var data []byte
			hash := strings.Repeat("a", 64)
			if tc.read {
				data = bytes.Repeat([]byte("x"), int(tc.size))
				hash = providerutil.SHA256Hex(data)
			}
			checksum := strings.Repeat("b", 64)
			metadata := document.AuthorizedUploadMetadata{MediaFamily: "image", MediaType: "image/png", ByteLength: tc.size, SHA256: hash, CapabilityRecordChecksum: checksum, ProviderMetadataChecksum: checksum, InputKind: document.RenditionInputOriginalFile}
			facts := document.UploadCapabilityFacts{MediaFamily: "image", MediaType: "image/png", SourceBytes: tc.size, SourceSHA256: hash, Checksum: checksum, DescriptorFingerprint: profile.Descriptor.Fingerprint, Pixels: 1, Frames: 1}
			reader := &kitReadCounter{Reader: bytes.NewReader(data)}
			upload := &kitUpload{Reader: reader, metadata: metadata, proof: uploadcapability.New(facts, nil)}
			requests := 0
			client := newTestClient(t, profile, nil, roundTripFunc(func(request *http.Request) (*http.Response, error) {
				requests++
				return jsonResponse(request, http.StatusOK, []byte(`{"model":"synthetic-model","data":[{"index":0,"embedding":[1,0,0]}]}`)), nil
			}))
			authorization := testAuthorization(profile.Descriptor)
			authorization.MaxInputBytes = profile.MaxInputBytes
			_, err := client.Embed(t.Context(), []document.EmbeddingInput{{Key: "file", Role: document.EmbeddingRoleDocument, Kind: document.EmbeddingInputOriginalFile, Source: upload}}, authorization)
			if tc.want != nil {
				require.ErrorIs(t, err, tc.want)
				require.Zero(t, requests)
			} else {
				require.NoError(t, err)
				require.Equal(t, 1, requests)
			}
			if tc.read {
				require.Positive(t, reader.reads)
			} else {
				require.Zero(t, reader.reads)
			}
		})
	}
}

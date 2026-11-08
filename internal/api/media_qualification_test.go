package api_test

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
	"uuid"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/media/mediatest"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/daemonconn"
	"go.kenn.io/docbank/internal/processing"
	"go.kenn.io/docbank/internal/store"
	"go.kenn.io/docbank/internal/vectorworker"
)

// This fixture replaces only the external transcription/embedding boundary.
// Admission, retained evidence, embeddings, vector publication and search use
// the production service, workers, store and HTTP handler.
type mediaQualificationRecord struct {
	SourceSHA256     string `json:"source_sha256"`
	ByteLength       int64  `json:"byte_length"`
	ID               string `json:"id"`
	Profile          string `json:"profile"`
	SourceID         string `json:"source_id"`
	SourceVersionID  string `json:"source_version_id"`
	ContentVersionID string `json:"content_version_id"`
	SuppliedInputID  string `json:"supplied_input_id"`
	Origin           string `json:"origin"`
	Text             string `json:"text"`
	StartMS          *int64 `json:"start_ms"`
	EndMS            *int64 `json:"end_ms"`
	occurrenceID     string
	nodeID           int64
}

type mediaQualificationManifest struct {
	Quality       string                     `json:"quality"`
	ModelMetadata map[string]any             `json:"model_metadata,omitempty"`
	URL           string                     `json:"url"`
	APIKey        string                     `json:"api_key"`
	Profile       string                     `json:"profile"`
	VaultUID      string                     `json:"vault_uid"`
	Records       []mediaQualificationRecord `json:"records"`
}

type mediaQualificationFixture struct {
	gate     *api.OperationGate
	manifest mediaQualificationManifest
	catalog  *testStore
	service  *processing.Service
	encoder  *mediaQualificationEncoder
	server   *httptest.Server
	stop     chan struct{}
	stopOnce sync.Once
}

type mediaQualificationEncoder struct {
	endpoint         string
	client           *http.Client
	descriptor       document.EmbeddingDescriptor
	mu               sync.Mutex
	entered, release chan struct{}
	armed            bool
	calls            int
}

func (p *mediaQualificationEncoder) Descriptor() document.EmbeddingDescriptor { return p.descriptor }
func (p *mediaQualificationEncoder) Embed(ctx context.Context, inputs []document.EmbeddingInput, _ document.EmbeddingAuthorization) (document.EmbeddingResult, error) {
	if len(inputs) > 0 && inputs[0].Role == document.EmbeddingRoleQuery {
		p.mu.Lock()
		p.calls++
		var release chan struct{}
		if p.armed {
			p.armed = false
			close(p.entered)
			release = p.release
		}
		p.mu.Unlock()
		if release != nil {
			select {
			case <-release:
			case <-ctx.Done():
				return document.EmbeddingResult{}, ctx.Err()
			}
		}
	}
	if p.endpoint != "" {
		texts := make([]string, len(inputs))
		for i, input := range inputs {
			texts[i] = input.Text
		}
		var response struct {
			Vectors [][]float32 `json:"vectors"`
		}
		if err := p.request(ctx, http.MethodPost, "/embed", struct {
			Texts []string `json:"texts"`
		}{texts}, &response); err != nil {
			return document.EmbeddingResult{}, err
		}
		if len(response.Vectors) != len(inputs) {
			return document.EmbeddingResult{}, errors.New("encoder vector count mismatch")
		}
		result := document.EmbeddingResult{Vectors: make([]document.EmbeddingVector, len(inputs))}
		for i, values := range response.Vectors {
			result.Vectors[i] = document.EmbeddingVector{Key: inputs[i].Key, Values: values}
		}
		return result, nil
	}
	vectors := make([]document.EmbeddingVector, len(inputs))
	for i, input := range inputs {
		values := []float32{0, 0, 1}
		text := strings.ToLower(input.Text)
		if strings.Contains(text, "telescope") || strings.Contains(text, "observatory") {
			values = []float32{1, 0, 0}
			if strings.Contains(text, "monday") {
				values = []float32{0, 1, 0}
			}
		}
		vectors[i] = document.EmbeddingVector{Key: input.Key, Values: values}
	}
	return document.EmbeddingResult{Vectors: vectors}, nil
}

func (p *mediaQualificationEncoder) request(ctx context.Context, method, path string, body, output any) error {
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(ctx, method, p.endpoint+path, reader)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	response, err := p.client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("fixture encoder returned %d", response.StatusCode)
	}
	return json.UnmarshalRead(io.LimitReader(response.Body, 8<<20), output)
}

func (p *mediaQualificationEncoder) barrier(ctx context.Context, action string) error {
	p.mu.Lock()
	switch action {
	case "arm":
		if p.release != nil {
			p.mu.Unlock()
			return errors.New("barrier already armed")
		}
		p.entered, p.release, p.armed = make(chan struct{}), make(chan struct{}), true
		p.mu.Unlock()
		return nil
	case "release":
		if p.release == nil {
			p.mu.Unlock()
			return errors.New("barrier not armed")
		}
		close(p.release)
		p.release = nil
		p.armed = false
		p.mu.Unlock()
		return nil
	case "wait":
		entered := p.entered
		p.mu.Unlock()
		if entered == nil {
			return errors.New("barrier not armed")
		}
		select {
		case <-entered:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	default:
		p.mu.Unlock()
		return errors.New("unknown barrier action")
	}
}

type mediaQualificationTokenizer struct{}

func (mediaQualificationTokenizer) Identity() document.TokenizerIdentity {
	return document.TokenizerIdentity{Name: "qualification", Revision: "v1"}
}
func (mediaQualificationTokenizer) PrefixTokenCountsMonotonic() bool { return true }
func (mediaQualificationTokenizer) Tokenize(text string, limit int) ([]document.TokenBoundary, error) {
	runes := []rune(text)
	if len(runes) > limit {
		return nil, document.ErrTokenizerLimit
	}
	result := make([]document.TokenBoundary, len(runes))
	for i := range result {
		result[i] = document.TokenBoundary{Start: i, End: i + 1}
	}
	return result, nil
}

type mediaQualificationTranscriber struct{ descriptor document.RenditionDescriptor }

func (p mediaQualificationTranscriber) Descriptor() document.RenditionDescriptor { return p.descriptor }
func (p mediaQualificationTranscriber) Render(_ context.Context, _ document.AuthorizedUpload, auth document.RenditionAuthorization) (document.RenditionResult, error) {
	fingerprint, err := auth.Fingerprint()
	if err != nil {
		return document.RenditionResult{}, err
	}
	now := time.Now().UTC().Format("2006-01-02T15:04:05.000000000Z")
	return document.RenditionResult{
		Evidence: document.SourceEvidenceV1{ContractVersion: document.SourceEvidenceContractV1, Completeness: document.EvidenceComplete, Family: "audio", UnitKind: document.EvidenceUnitSegment, Units: []document.SourceEvidenceUnitV1{{Order: 0, Text: "The observatory receives its new telescope on Friday at 3 PM.", Locator: document.SourceEvidenceLocatorV1{Kind: document.EvidenceLocatorSegment, IndexOrigin: document.EvidenceIndexOriginZero, Start: 1000, End: 4000}}}},
		Receipt:  document.RenditionReceipt{ProviderID: p.descriptor.ID, DescriptorFingerprint: p.descriptor.Fingerprint, PolicyFingerprint: p.descriptor.PolicyFingerprint, RenditionRequestFingerprint: auth.RenditionRequestFingerprint, AuthorizationFingerprint: fingerprint, SourceSHA256: auth.SourceSHA256, OperationID: "synthetic-transcription", StartedAt: now, CompletedAt: now, Usage: document.RenditionUsage{Requests: 1, InputBytes: auth.SourceBytes}},
	}, nil
}

type mediaQualificationOptions struct {
	TranscriptA string
	ChunkTokens int
}

func newMediaQualificationFixture(t *testing.T, options ...mediaQualificationOptions) *mediaQualificationFixture {
	t.Helper()
	require.LessOrEqual(t, len(options), 1)
	option := mediaQualificationOptions{ChunkTokens: 512}
	if len(options) != 0 {
		option = options[0]
		if option.ChunkTokens == 0 {
			option.ChunkTokens = 512
		}
	}
	f := &mediaQualificationFixture{stop: make(chan struct{})}
	var worker renditionTestWorker
	encoder := &mediaQualificationEncoder{}
	contract, err := document.NewModelInputContract(document.ModelInputContractConfig{Profile: document.ModelInputProfileCustom, CompatibilityID: "qualification-v1", Document: document.ModelInputEncoder{Mode: document.ModelInputModeText, Template: "document: {{content}}"}, Query: document.ModelInputEncoder{Mode: document.ModelInputModeText, Template: "query: {{content}}"}})
	require.NoError(t, err)
	encoder.descriptor, err = document.NewEmbeddingDescriptor(document.EmbeddingDescriptor{ID: "synthetic.media-qualification", ContractVersion: document.EmbeddingProviderContractVersion, PolicyFingerprint: processingTestHash("qualification-policy"), TrustBoundary: document.EmbeddingTrustLocalProcess, Model: "deterministic-contract-only", ModelRevision: "v1", Dimension: 3, Metric: document.VectorMetricCosine, Normalization: document.VectorNormalizationNone, ScalarEncoding: "float32", DocumentFormatter: "qualification/document-v1", QueryFormatter: "qualification/query-v1", InputKinds: []document.EmbeddingInputKind{document.EmbeddingInputRenditionChunk}, CompatibilityID: contract.CompatibilityID, SupportsTextQuery: true, ModelInput: contract, SupportedRequestModes: []document.ModelInputMode{document.ModelInputModeText}})
	require.NoError(t, err)
	f.manifest.Quality = "deterministic-contract"
	if endpoint := os.Getenv("DOCBANK_MEDIA_QUALIFICATION_ENCODER_URL"); endpoint != "" {
		parsed, err := url.Parse(endpoint)
		require.NoError(t, err)
		require.Equal(t, "http", parsed.Scheme)
		require.Equal(t, "127.0.0.1", parsed.Hostname())
		encoder.endpoint = strings.TrimRight(endpoint, "/")
		encoder.client = &http.Client{Timeout: 30 * time.Second}
		var metadata map[string]any
		require.NoError(t, encoder.request(t.Context(), http.MethodGet, "/health", nil, &metadata))
		f.manifest.Quality, f.manifest.ModelMetadata = "real-model", metadata
		descriptor := encoder.descriptor
		descriptor.Fingerprint = ""
		descriptor.Dimension = 384
		descriptor.Normalization = document.VectorNormalizationUnitLength
		descriptor.ModelInput, err = document.NewModelInputContract(document.ModelInputContractConfig{Profile: document.ModelInputProfileCustom, CompatibilityID: "qualification-minilm-identity-v1", Document: document.ModelInputEncoder{Mode: document.ModelInputModeText, Template: "{{content}}"}, Query: document.ModelInputEncoder{Mode: document.ModelInputModeText, Template: "{{content}}"}})
		require.NoError(t, err)
		descriptor.CompatibilityID = descriptor.ModelInput.CompatibilityID
		descriptor.DocumentFormatter, descriptor.QueryFormatter = "identity", "identity"
		descriptor.Model = "all-MiniLM-L6-v2"
		descriptor.ModelRevision = fmt.Sprint(metadata["revision"])
		encoder.descriptor, err = document.NewEmbeddingDescriptor(descriptor)
		require.NoError(t, err)
	}
	f.encoder = encoder
	f.manifest.APIKey = uuid.New().String() // Fixture-only key; never a daemon runtime record.
	f.server, f.catalog = newTestServer(t, func(deps *api.Deps) {
		deps.Cfg.Server.APIKey = f.manifest.APIKey
		gate := api.NewOperationGate()
		f.gate = gate
		deps.Gate = gate
		name, supplied, err := processing.NewSuppliedMediaProfile(deps.Store, deps.Blobs, "daemon:operator")
		require.NoError(t, err)
		descriptor, err := document.NewRenditionDescriptor(document.RenditionDescriptor{ID: "synthetic.media-transcription", ContractVersion: document.RenditionProviderContractVersion, PolicyFingerprint: processingTestHash("qualification-transcription"), TrustBoundary: document.RenditionTrustLocalProcess, SupportedFormats: []document.RenditionFormatCapability{{MediaFamily: "audio", MediaType: "audio/wav", InputKind: document.RenditionInputOriginalFile}}, ReturnsStructured: true, ArtifactRoles: []document.EvidenceArtifactRole{document.EvidenceArtifactStructured}})
		require.NoError(t, err)
		generated := processing.ProfileConfig{Profile: processingTestProfile(descriptor), RenditionProvider: mediaQualificationTranscriber{descriptor: descriptor}}
		for _, cfg := range []*processing.ProfileConfig{&generated} {
			d := encoder.descriptor
			cfg.Profile.Embeddings = []document.EmbeddingBindingV1{{Activation: document.EmbeddingRequired, AuthorizationFingerprint: processingTestHash("qualification-embedding-auth"), CompatibilityID: d.CompatibilityID, CredentialBinding: "credential:synthetic", Descriptor: document.ProviderDescriptorV1{ID: d.ID, Fingerprint: d.Fingerprint}, Dimensions: d.Dimension, DisclosureFingerprint: processingTestHash("qualification-embedding-disclosure"), DocumentFormatter: d.DocumentFormatter, InputKind: document.EmbeddingInputRenditionChunk, MaxBatchItems: 8, MaxInputBytes: 1 << 20, MaxInputTokens: option.ChunkTokens, MaxResponseBytes: 1 << 20, Metric: d.Metric, ModelInput: d.ModelInput, Model: d.Model, Name: "semantic", Normalization: d.Normalization, QueryFormatter: d.QueryFormatter, ScalarEncoding: d.ScalarEncoding, TrustBoundary: string(d.TrustBoundary), Chunk: &document.EmbeddingChunkPolicyV1{ContextFingerprint: processingTestHash("qualification-context"), Formatter: "qualification/v1", MaxTokens: option.ChunkTokens, OverlapTokens: 0, Tokenizer: "qualification", TokenizerRevision: "v1", TruncationPolicy: document.TruncationPolicyReject}}}
			cfg.EmbeddingProviders = map[string]document.EmbeddingProvider{"semantic": encoder}
			cfg.Tokenizers = map[string]document.Tokenizer{"semantic": mediaQualificationTokenizer{}}
			cfg.Profile.Retrieval.LexicalLimit, cfg.Profile.Retrieval.VectorLimit = 100, 100
		}
		supplied, err = processing.WithSuppliedMediaEmbeddings(supplied, generated)
		require.NoError(t, err)
		service, err := processing.NewService(t.Context(), processing.ServiceConfig{Catalog: deps.Store, Blobs: deps.Blobs, Gate: gate, Principal: "daemon:operator", SpoolDirectory: filepath.Join(deps.VaultRoot, "blobs", "tmp"), Profiles: map[string]processing.ProfileConfig{name: supplied, "qualification-generated": generated}})
		require.NoError(t, err)
		t.Cleanup(service.Stop)
		f.service = service
		deps.Processing = service
		worker.bind(t, deps, service, gate, "media-qualification")
	})
	f.manifest.URL, f.manifest.Profile, f.manifest.VaultUID = f.server.URL, processing.SuppliedMediaProfileName, f.catalog.VaultID()
	client := daemonconn.New(f.server.URL, f.manifest.APIKey)
	for i, item := range []struct {
		id, text  string
		generated bool
	}{
		{"a-v1", "The telescope delivery arrives Friday at three.", false},
		{"a-v2", "The telescope delivery now arrives Monday at noon.", false},
		{"b-v1", "The observatory receives its new telescope on Friday at 3 PM.", true},
		{"c-v1", "The grocery delivery arrives Friday at three.", false},
	} {
		if item.id == "a-v1" && option.TranscriptA != "" {
			item.text = option.TranscriptA
		}
		raw := mediatest.WAV()
		raw[len(raw)-1] = byte(i + 1)
		receipt, err := client.SubmitSuppliedMedia(t.Context(), api.MediaSuppliedMetadata{OperationID: uuid.New().String(), Filename: item.id + ".wav", MediaType: "audio/wav", SHA256: processingTestHash(string(raw)), ByteLength: int64(len(raw)), Occurrence: api.MediaOccurrenceBody{Ref: item.id, Revision: "1"}}, bytes.NewReader(raw))
		require.NoError(t, err)
		record := mediaQualificationRecord{SourceSHA256: processingTestHash(string(raw)), ByteLength: int64(len(raw)), ID: item.id, Profile: processing.SuppliedMediaProfileName, SourceID: receipt.SourceID, SourceVersionID: receipt.SourceVersionID, ContentVersionID: receipt.ContentVersionID, Origin: "supplied", Text: item.text, occurrenceID: receipt.OccurrenceID}
		if item.generated {
			record.Profile, record.Origin = "qualification-generated", "generated"
			record.StartMS, record.EndMS = new(int64(1000)), new(int64(4000))
		} else {
			artifact, err := client.ImportMediaArtifact(t.Context(), receipt.SourceID, api.MediaArtifactMetadata{OperationID: uuid.New().String(), OccurrenceID: receipt.OccurrenceID, Kind: "transcript", Filename: item.id + ".txt", MediaType: "text/plain", SHA256: processingTestHash(item.text), ByteLength: int64(len(item.text))}, strings.NewReader(item.text))
			require.NoError(t, err)
			record.SuppliedInputID = artifact.SuppliedInputID
		}
		version, err := f.catalog.ContentVersionByID(t.Context(), record.ContentVersionID)
		require.NoError(t, err)
		record.nodeID = version.NodeID
		selector := processing.Selector{NodeID: version.NodeID, ContentVersionID: version.ID, Profile: record.Profile}
		plan, err := f.service.Plan(t.Context(), selector)
		require.NoError(t, err)
		_, err = f.service.GrantConsent(t.Context(), processing.ConsentGrantRequest{Selector: selector, PlanFingerprint: plan.Fingerprint})
		require.NoError(t, err)
		_, err = client.RetryMedia(t.Context(), record.SourceID, api.MediaRetryBody{OperationID: uuid.New().String(), Processing: &api.MediaProcessingBody{Profile: record.Profile, SuppliedInputID: record.SuppliedInputID}})
		require.NoError(t, err)
		worker.runQueued(t)
		continuations, err := f.catalog.MediaProcessingContinuations(t.Context(), "daemon:operator", "", 250)
		require.NoError(t, err)
		for _, continuation := range continuations {
			require.NoError(t, f.service.ContinueMediaProcessing(t.Context(), continuation))
		}
		transcript, err := f.service.MediaTranscript(t.Context(), processing.MediaTranscriptRequest{SourceID: record.SourceID, SourceVersionID: record.SourceVersionID, ContentVersionID: record.ContentVersionID})
		require.NoError(t, err)
		require.NotNil(t, transcript.Transcript)
		require.Equal(t, record.Origin, transcript.Transcript.Origin)
		f.manifest.Records = append(f.manifest.Records, record)
	}
	// Readiness includes real nonlexical HTTP reads, after continuation has
	// synchronously completed embedding jobs and vector publication.
	for _, record := range f.manifest.Records {
		report, err := client.SearchDocuments(t.Context(), f.request(record.Profile, "semantic", record))
		require.NoError(t, err)
		require.Equal(t, "semantic", report.ActualMode)
		require.Equal(t, "complete", report.Coverage.State)
		require.Len(t, report.Results, 1)
	}
	t.Cleanup(func() { _ = f.encoder.barrier(context.WithoutCancel(t.Context()), "release") })
	return f
}

func (f *mediaQualificationFixture) control(ctx context.Context, action, id string) error {
	if action == "arm" || action == "wait" || action == "release" {
		return f.encoder.barrier(ctx, action)
	}
	if action == "refresh" {
		worker, err := vectorworker.NewIndexWorker(vectorworker.IndexWorkerConfig{Catalog: f.catalog.Store, ReadVectorSet: func(ctx context.Context, member store.VectorIndexMember) ([]byte, error) {
			return f.catalog.ReadVectorIndexVectorSet(ctx, f.catalog.Blobs, member)
		}, Mutate: f.gate.MutateContext, Owner: "qualification-refresh", BuildLease: time.Minute, ReaderLease: time.Minute, IdleDelay: time.Second})
		if err != nil {
			return err
		}
		spaces, err := f.catalog.ListVectorIndexSpaces(ctx)
		if err != nil {
			return err
		}
		for _, space := range spaces {
			if _, err := worker.Rebuild(ctx, space); err != nil && !errors.Is(err, store.ErrNotFound) {
				return err
			}
		}
		return nil
	}
	if action == "stop" {
		f.stopOnce.Do(func() { close(f.stop) })
		return nil
	}
	for _, record := range f.manifest.Records {
		if record.ID == id {
			switch action {
			case "replace":
				node, err := f.catalog.NodeByID(ctx, record.nodeID)
				if err != nil {
					return err
				}
				donor := f.manifest.Records[1]
				if donor.ContentVersionID == record.ContentVersionID {
					donor = f.manifest.Records[0]
				}
				version, err := f.catalog.ContentVersionByID(ctx, donor.ContentVersionID)
				if err != nil {
					return err
				}
				_, replacement, err := f.catalog.ReplaceContent(ctx, node.ID, node.Revision, version.BlobHash, version.Size, version.MimeType)
				if err != nil {
					return err
				}
				return f.catalog.PublishMediaSourceVersion(ctx, store.MediaSourceVersionInput{ID: uuid.New().String(), SourceID: record.SourceID, Revision: 2, ExpectedHeadRevision: 1, ContentVersionID: replacement.ID, CaptureJSON: "{}"})
			case "hide":
				node, err := f.catalog.NodeByID(ctx, record.nodeID)
				if err != nil {
					return err
				}
				_, _, err = f.catalog.Trash(ctx, node.ID, node.Revision)
				return err
			case "revoke":
				_, err := f.service.RevokeMediaOccurrence(ctx, uuid.New().String(), record.occurrenceID, "1")
				return err
			}
		}
	}
	return errors.New("unknown fixture action or record")
}

// TestMediaQualificationServer is an opt-in cross-repository qualification
// process. The test deadline bounds its lifetime; interrupt and stop both run
// testing cleanup, including removal of every synthetic vault and manifest.
func TestMediaQualificationServer(t *testing.T) {
	path := os.Getenv("DOCBANK_MEDIA_QUALIFICATION_MANIFEST")
	if path == "" {
		t.Skip("set explicit fixture manifest output path")
	}
	f := newMediaQualificationFixture(t)
	handler := f.catalog.Server.Handler()
	// httptest binds loopback. The control wrapper exists only in this test binary.
	controlServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/qualification/control" {
			handler.ServeHTTP(w, r)
			return
		}
		if r.Method != http.MethodPost || r.Header.Get("X-Api-Key") != f.manifest.APIKey {
			http.Error(w, "fixture control denied", http.StatusForbidden)
			return
		}
		var request struct {
			Action string `json:"action"`
			ID     string `json:"id"`
		}
		if err := json.UnmarshalRead(io.LimitReader(r.Body, 4096), &request, json.RejectUnknownMembers(true)); err != nil {
			http.Error(w, "invalid control", 400)
			return
		}
		if err := f.control(r.Context(), request.Action, request.ID); err != nil {
			http.Error(w, err.Error(), 409)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	t.Cleanup(controlServer.Close)
	t.Cleanup(func() { _ = f.encoder.barrier(context.WithoutCancel(t.Context()), "release") })
	f.manifest.URL = controlServer.URL
	encoded, err := json.Marshal(f.manifest)
	require.NoError(t, err)
	temporary := path + ".tmp"
	t.Cleanup(func() { _ = os.Remove(temporary) })
	require.NoError(t, os.WriteFile(temporary, encoded, 0o600))
	require.NoError(t, os.Rename(temporary, path))
	t.Cleanup(func() { require.NoError(t, os.Remove(path)) })
	ctx, cancel := signal.NotifyContext(t.Context(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	select {
	case <-f.stop:
	case <-ctx.Done():
	}
}

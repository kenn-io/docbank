package api_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"sync"
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
)

// This fixture replaces only the external transcription/embedding boundary.
// Admission, retained evidence, embeddings, vector publication and search use
// the production service, workers, store and HTTP handler.
type mediaQualificationRecord struct {
	ID               string
	Profile          string
	SourceID         string
	SourceVersionID  string
	ContentVersionID string
	SuppliedInputID  string
	Origin           string
	occurrenceID     string
	nodeID           int64
}

type mediaQualificationManifest struct {
	APIKey   string
	VaultUID string
	Records  []mediaQualificationRecord
}

type mediaQualificationFixture struct {
	manifest           mediaQualificationManifest
	catalog            *testStore
	service            *processing.Service
	encoder            *mediaQualificationEncoder
	unprocessedEncoder *mediaQualificationEncoder
	unprocessedProfile string
	server             *httptest.Server
}

const mediaQualificationUnprocessedProfile = "qualification-new-donor"

type mediaQualificationEncoder struct {
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
	TranscriptA      string
	ChunkTokens      int
	LaneLimit        int
	Distractors      int
	UnprocessedDonor bool
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
	f := &mediaQualificationFixture{}
	var worker renditionTestWorker
	encoder := &mediaQualificationEncoder{}
	contract, err := document.NewModelInputContract(document.ModelInputContractConfig{Profile: document.ModelInputProfileCustom, CompatibilityID: "qualification-v1", Document: document.ModelInputEncoder{Mode: document.ModelInputModeText, Template: "document: {{content}}"}, Query: document.ModelInputEncoder{Mode: document.ModelInputModeText, Template: "query: {{content}}"}})
	require.NoError(t, err)
	encoder.descriptor, err = document.NewEmbeddingDescriptor(document.EmbeddingDescriptor{ID: "synthetic.media-qualification", ContractVersion: document.EmbeddingProviderContractVersion, PolicyFingerprint: processingTestHash("qualification-policy"), TrustBoundary: document.EmbeddingTrustLocalProcess, Model: "deterministic-contract-only", ModelRevision: "v1", Dimension: 3, Metric: document.VectorMetricCosine, Normalization: document.VectorNormalizationNone, ScalarEncoding: "float32", DocumentFormatter: "qualification/document-v1", QueryFormatter: "qualification/query-v1", InputKinds: []document.EmbeddingInputKind{document.EmbeddingInputRenditionChunk}, CompatibilityID: contract.CompatibilityID, SupportsTextQuery: true, ModelInput: contract, SupportedRequestModes: []document.ModelInputMode{document.ModelInputModeText}})
	require.NoError(t, err)
	f.encoder = encoder
	f.manifest.APIKey = uuid.New().String() // Fixture-only key; never a daemon runtime record.
	f.server, f.catalog = newTestServer(t, func(deps *api.Deps) {
		deps.Cfg.Server.APIKey = f.manifest.APIKey
		gate := api.NewOperationGate()
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
			if option.LaneLimit > 0 {
				cfg.Profile.Retrieval.LexicalLimit, cfg.Profile.Retrieval.VectorLimit = option.LaneLimit, option.LaneLimit
			}
		}
		unboundSupplied := supplied
		supplied, err = processing.WithSuppliedMediaEmbeddings(supplied, generated)
		require.NoError(t, err)
		if option.LaneLimit > 0 {
			supplied.Profile.Retrieval.LexicalLimit, supplied.Profile.Retrieval.VectorLimit = option.LaneLimit, option.LaneLimit
		}
		profiles := map[string]processing.ProfileConfig{name: supplied, "qualification-generated": generated, "qualification-unbound": unboundSupplied}
		if option.UnprocessedDonor {
			donorDescriptor := encoder.descriptor
			donorDescriptor.Fingerprint = ""
			donorDescriptor.PolicyFingerprint = processingTestHash("qualification-unprocessed-donor")
			donorDescriptor, err = document.NewEmbeddingDescriptor(donorDescriptor)
			require.NoError(t, err)
			donorEncoder := &mediaQualificationEncoder{descriptor: donorDescriptor}
			f.unprocessedEncoder = donorEncoder
			unprocessedDonor := generated
			unprocessedDonor.Profile.Embeddings = slices.Clone(generated.Profile.Embeddings)
			unprocessedDonor.Profile.Embeddings[0].Descriptor.Fingerprint = donorDescriptor.Fingerprint
			unprocessedDonor.EmbeddingProviders = map[string]document.EmbeddingProvider{"semantic": donorEncoder}
			unprocessed, composeErr := processing.WithSuppliedMediaEmbeddings(supplied, unprocessedDonor)
			require.NoError(t, composeErr)
			profiles[mediaQualificationUnprocessedProfile] = unprocessed
			f.unprocessedProfile = mediaQualificationUnprocessedProfile
		}
		service, err := processing.NewService(t.Context(), processing.ServiceConfig{Catalog: deps.Store, Blobs: deps.Blobs, Gate: gate, Principal: "daemon:operator", SpoolDirectory: filepath.Join(deps.VaultRoot, "blobs", "tmp"), Profiles: profiles})
		require.NoError(t, err)
		t.Cleanup(service.Stop)
		f.service = service
		deps.Processing = service
		worker.bind(t, deps, service, gate, "media-qualification")
	})
	f.manifest.VaultUID = f.catalog.VaultID()
	client := daemonconn.New(f.server.URL, f.manifest.APIKey)
	records := []struct {
		id, text  string
		generated bool
	}{
		{"a-v1", "The telescope delivery arrives Friday at three.", false},
		{"a-v2", "The telescope delivery now arrives Monday at noon.", false},
		{"b-v1", "The observatory receives its new telescope on Friday at 3 PM.", true},
		{"c-v1", "The grocery delivery arrives Friday at three.", false},
	}
	for i := range option.Distractors {
		records = append(records, struct {
			id, text  string
			generated bool
		}{fmt.Sprintf("distractor-%d", i), fmt.Sprintf("The telescope delivery arrives Friday at three, reference %d.", i), false})
	}
	for i, item := range records {
		if item.id == "a-v1" && option.TranscriptA != "" {
			item.text = option.TranscriptA
		}
		raw := mediatest.WAV()
		raw[len(raw)-1] = byte(i + 1)
		receipt, err := client.SubmitSuppliedMedia(t.Context(), api.MediaSuppliedMetadata{OperationID: uuid.New().String(), Filename: item.id + ".wav", MediaType: "audio/wav", SHA256: processingTestHash(string(raw)), ByteLength: int64(len(raw)), Occurrence: api.MediaOccurrenceBody{Ref: item.id, Revision: "1"}}, bytes.NewReader(raw))
		require.NoError(t, err)
		record := mediaQualificationRecord{ID: item.id, Profile: processing.SuppliedMediaProfileName, SourceID: receipt.SourceID, SourceVersionID: receipt.SourceVersionID, ContentVersionID: receipt.ContentVersionID, Origin: "supplied", occurrenceID: receipt.OccurrenceID}
		if item.generated {
			record.Profile, record.Origin = "qualification-generated", "generated"
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
	t.Cleanup(func() { _ = f.encoder.barrier(context.WithoutCancel(t.Context()), "release") })
	return f
}

func (f *mediaQualificationFixture) control(ctx context.Context, action, id string) error {
	if action == "arm" || action == "wait" || action == "release" {
		return f.encoder.barrier(ctx, action)
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

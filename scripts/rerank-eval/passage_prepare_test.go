package rerankeval_test

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"go.kenn.io/docbank/document"
	embeddingeval "go.kenn.io/docbank/document/embedding/eval"
	"go.kenn.io/docbank/document/plaintext"
	"go.kenn.io/docbank/document/providerhttp"
	"go.kenn.io/docbank/document/voyage"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/blob"
	"go.kenn.io/docbank/internal/processing"
	"go.kenn.io/docbank/internal/retrieval"
	"go.kenn.io/docbank/internal/store"
)

type passageFixture struct {
	catalog  *store.Store
	blobs    *blob.Store
	service  *processing.Service
	versions map[string]string
}

func preparePassageCorpus(ctx context.Context, root string, corpus embeddingeval.Corpus, provider document.EmbeddingProvider) (_ *passageFixture, err error) {
	f := &passageFixture{versions: map[string]string{}}
	defer func() {
		if err != nil {
			err = errors.Join(err, f.close())
		}
	}()
	f.catalog, err = store.Open(filepath.Join(root, "catalog.db"))
	if err != nil {
		return nil, err
	}
	f.blobs, err = blob.New(store.NewPackCatalog(f.catalog), filepath.Join(root, "blobs"))
	if err != nil {
		return nil, err
	}
	rendition, err := plaintext.New(plaintext.Profile{MaxDocumentBytes: 16 << 20})
	if err != nil {
		return nil, err
	}
	rd, ed := rendition.Descriptor(), provider.Descriptor()
	h := sha256String
	profile := document.ProcessingProfileV1{ContractVersion: document.ProcessingProfileContractV1,
		Rendition: &document.RenditionBindingV1{
			AdapterContract: "docbank-plaintext/v1", AuthorizationFingerprint: h("local-auth"), CredentialBinding: "credential:none",
			DeploymentFingerprint: h("local-v1"), Descriptor: document.ProviderDescriptorV1{ID: rd.ID, Fingerprint: rd.Fingerprint},
			DisclosureFingerprint: h("local-disclosure"), MaxDocumentBytes: 16 << 20, MaxResponseBytes: 32 << 20,
			MaxUnits: 10000, Name: "plaintext", RequestedArtifacts: []document.EvidenceArtifactRole{document.EvidenceArtifactStructured},
			TrustBoundary: string(rd.TrustBoundary), UploadOptionsFingerprint: h("plain")},
		EvidenceLexical: document.EvidenceLexicalPolicyV1{
			CompletenessFingerprint: h("complete"), LexicalSegmenterFingerprint: h("segments800"), MaxDocumentChars: 16 << 20,
			MaxSegmentRunes: 800, MaxUnitRunes: 16 << 20, NormalizedEvidenceContract: document.NormalizedEvidenceContractV1,
			NormalizerFingerprint: h("normalizer"), RenditionContract: document.RenditionContractV1,
			SanitizerFingerprint: h("sanitizer"), SourceEvidenceContract: document.SourceEvidenceContractV1},
		RetentionDisclosure: document.RetentionDisclosurePolicyV1{AttachmentPolicyFingerprint: h("retain"),
			ConsentFingerprint: h("consent"), RetainSanitizedMarkdown: true, TrustBoundary: string(rd.TrustBoundary)},
		Retrieval: document.RetrievalPolicyV1{LexicalLimit: 100, VectorLimit: 100},
		Embeddings: []document.EmbeddingBindingV1{{
			Activation: document.EmbeddingRequired, AuthorizationFingerprint: h("voyage-auth"), CompatibilityID: ed.CompatibilityID,
			CredentialBinding: "credential:voyage", Descriptor: document.ProviderDescriptorV1{ID: ed.ID, Fingerprint: ed.Fingerprint},
			Dimensions: ed.Dimension, DisclosureFingerprint: h("voyage-disclosure"), DocumentFormatter: ed.DocumentFormatter,
			InputKind: document.EmbeddingInputRenditionChunk, MaxInputTokens: 2048, MaxBatchItems: 128,
			MaxInputBytes: 1 << 20, MaxResponseBytes: 32 << 20, Metric: ed.Metric,
			ModelInput: ed.ModelInput, Model: ed.Model, Name: "voyage", Normalization: ed.Normalization,
			QueryFormatter: ed.QueryFormatter, ScalarEncoding: ed.ScalarEncoding, TrustBoundary: string(ed.TrustBoundary),
			Chunk: &document.EmbeddingChunkPolicyV1{ContextFingerprint: h("context"), Formatter: "rendition-chunk/v1",
				MaxTokens: 1536, OverlapTokens: 128, Tokenizer: "unicode-runes", TokenizerRevision: "v1",
				TruncationPolicy: document.TruncationPolicyReject}}},
	}

	if err = os.MkdirAll(filepath.Join(root, "spool"), 0700); err != nil {
		return nil, err
	}
	f.service, err = processing.NewService(processing.ServiceConfig{
		Catalog: f.catalog, Blobs: f.blobs, Gate: api.NewOperationGate(),
		SpoolDirectory: filepath.Join(root, "spool"), Lifecycle: ctx, Principal: "offline-evaluation", Scope: "passage-evaluation",
		Profiles: map[string]processing.ProfileConfig{"passage": {
			Profile: profile, RenditionProvider: rendition,
			EmbeddingProviders: map[string]document.EmbeddingProvider{"voyage": provider},
			Tokenizers:         map[string]document.Tokenizer{"voyage": document.UnicodeRuneTokenizer{}},
			EmbeddingClassifiers: map[string]func(error) (processing.EmbeddingProviderFailure, time.Duration){
				"voyage": func(error) (processing.EmbeddingProviderFailure, time.Duration) {
					return processing.EmbeddingProviderPermanent, 0
				}},
			EmbeddingDisclosures: map[string]processing.RuntimeDisclosure{"voyage": {
				ImmediateProcessor: "docbank-voyage-embeddings/v1", UltimateProcessor: ed.ID, Endpoint: voyage.DefaultEndpoint,
				Deployment: "2026-10-30", Model: ed.Model, ModelRevision: ed.ModelRevision}},
		}},
	})
	if err != nil {
		return nil, err
	}
	for i, doc := range corpus.Documents {
		written, e := f.blobs.WriteDetailedContext(ctx, strings.NewReader(doc.Text))
		if e != nil {
			return nil, e
		}
		encoding, e := written.EncodingName()
		if e != nil {
			return nil, e
		}
		node, e := f.catalog.CreateFile(ctx, f.catalog.RootID(), fmt.Sprintf("document-%06d.txt", i), written.Hash, written.Size, "text/plain", store.BlobPhysical{Encoding: encoding, StoredBytes: written.StoredSize, Created: written.Created})
		if e != nil {
			return nil, e
		}
		f.versions[doc.ID] = node.CurrentVersionID
		selector := processing.Selector{NodeID: node.ID, ContentVersionID: node.CurrentVersionID, Profile: "passage"}
		plan, e := f.service.Plan(ctx, selector)
		if e != nil {
			return nil, e
		}
		started, e := f.service.Start(ctx, processing.StartRequest{Selector: selector, PlanFingerprint: plan.Fingerprint, Consent: true})
		if e != nil {
			return nil, e
		}
		status, e := f.service.Status(ctx, started.ID)
		if e != nil {
			return nil, e
		}
		if status.CompletedBindings != 1 {
			return nil, fmt.Errorf("embedding preparation incomplete: state=%s phase=%s code=%s", status.State, status.Phase, status.FailureCode)
		}
	}
	return f, nil
}
func (f *passageFixture) prepare(ctx context.Context, q embeddingeval.Query) (*retrieval.PassageEvaluation, error) {
	return f.service.PreparePassageEvaluation(ctx, f.request(q.Text))
}
func (f *passageFixture) request(query string) processing.SearchRequest {
	return processing.SearchRequest{Profile: "passage", BindingID: "voyage", Mode: "hybrid", Query: query, Limit: 20, Fence: processing.SourceFence{VaultUID: f.catalog.VaultID(), ContentVersionIDs: slices.Collect(maps.Values(f.versions))}}
}
func (f *passageFixture) close() error {
	var err error
	if f.service != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		err = f.service.Shutdown(ctx)
	}
	if f.blobs != nil {
		err = errors.Join(err, f.blobs.Close())
	}
	if f.catalog != nil {
		err = errors.Join(err, f.catalog.Close())
	}
	return err
}

func passageEgress() providerhttp.EgressPolicy {
	return providerhttp.EgressPolicy{Scheme: "https", Host: "api.voyageai.com", Port: 443,
		AllowedCIDRs: []netip.Prefix{netip.MustParsePrefix("0.0.0.0/0"), netip.MustParsePrefix("::/0")}, ProxyMode: providerhttp.ProxyDisabled}
}
func passageEmbeddingProfile() (voyage.EmbeddingProfile, error) {
	input, err := document.NewModelInputContract(document.ModelInputContractConfig{
		Profile: document.ModelInputProfileCustom, CompatibilityID: "voyage/passage-eval-20261030/v1",
		Document: document.ModelInputEncoder{Mode: document.ModelInputModeDocument, Template: "{{content}}"},
		Query:    document.ModelInputEncoder{Mode: document.ModelInputModeQuery, Template: "{{content}}"}})
	if err != nil {
		return voyage.EmbeddingProfile{}, err
	}
	p := voyage.EmbeddingProfile{
		Mode: voyage.EmbeddingModeText, Endpoint: voyage.DefaultEndpoint, EgressPolicy: passageEgress(), ModelInput: input,
		SecretBinding: "credential:voyage", DeploymentEpoch: "2026-10-30", ChunkerVersion: "1", MaxRetries: 1,
		MaxBatchItems: 128, MaxInputBytes: 1 << 20, MaxRequestBytes: 2 << 20, MaxResponseBytes: 32 << 20,
		Descriptor: document.EmbeddingDescriptor{
			ID: voyage.EmbeddingProviderID, ContractVersion: document.EmbeddingProviderContractVersion,
			PolicyFingerprint: strings.Repeat("0", 64), TrustBoundary: document.EmbeddingTrustHostedProvider,
			Model: "voyage-4-large", ModelRevision: "2026-10-30", Dimension: 256, Metric: document.VectorMetricCosine,
			Normalization: document.VectorNormalizationUnitLength, ScalarEncoding: voyage.EmbeddingScalarFloat32,
			DocumentFormatter: voyage.EmbeddingDocumentFormatterV1, QueryFormatter: voyage.EmbeddingQueryFormatterV1,
			InputKinds: []document.EmbeddingInputKind{document.EmbeddingInputRenditionChunk}, CompatibilityID: input.CompatibilityID,
			ModelInput: input, SupportsTextQuery: true,
			SupportedRequestModes: []document.ModelInputMode{document.ModelInputModeDocument, document.ModelInputModeQuery}},
	}
	p.Descriptor, err = document.NewEmbeddingDescriptor(p.Descriptor)
	if err != nil {
		return p, err
	}
	p.Descriptor.PolicyFingerprint, err = voyage.EmbeddingPolicyFingerprint(p)
	if err != nil {
		return p, err
	}
	p.Descriptor.Fingerprint = ""
	p.Descriptor, err = document.NewEmbeddingDescriptor(p.Descriptor)
	return p, err
}

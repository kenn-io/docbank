package processing

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"math"
	"mime"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strings"
	"time"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/media"
	"go.kenn.io/docbank/document/upload"
	"go.kenn.io/docbank/internal/blob"
	"go.kenn.io/docbank/internal/maintenance"
	"go.kenn.io/docbank/internal/retrieval"
	"go.kenn.io/docbank/internal/store"
	"go.kenn.io/docbank/internal/vectorworker"
	"go.kenn.io/kit/packstore"
	"uuid"
)

const (
	MaxSourceFenceIDs  = 4096
	MaxRenditionBytes  = int64(64 << 20)
	DefaultSearchLimit = 20
	MaxSearchLimit     = 100
	timestampForm      = "2006-01-02T15:04:05.000000000Z"
)

var (
	ErrForeignVault              = errors.New("processing source fence belongs to another vault")
	ErrProfileNotConfigured      = errors.New("processing profile is not configured")
	ErrPlanChanged               = errors.New("processing plan changed after preview")
	ErrConsentRequired           = errors.New("processing consent is required")
	ErrRenditionFailed           = store.ErrRenditionJobTerminal
	ErrRenditionOperatorRequired = store.ErrRenditionJobOperatorRequired
	ErrPurgePlanChanged          = errors.New("derivative purge plan changed after preview")
	ErrInvalidPurgeRequest       = errors.New("derivative purge request is invalid")
	ErrInvalidConsentExpiry      = errors.New("processing consent expiry is invalid")
)

type ProfileConfig struct {
	Profile              document.ProcessingProfileV1
	RenditionProvider    document.RenditionProvider
	EmbeddingProviders   map[string]document.EmbeddingProvider
	EmbeddingClassifiers map[string]func(error) (EmbeddingProviderFailure, time.Duration)
	Tokenizers           map[string]document.Tokenizer
}

type ServiceConfig struct {
	Catalog        *store.Store
	Blobs          *blob.Store
	Gate           processingOperationGate
	Profiles       map[string]ProfileConfig
	Principal      string
	Scope          string
	SpoolDirectory string
	Clock          func() time.Time
}

type configuredProfile struct {
	portable          document.ProcessingProfileV1
	record            store.ProcessingProfileRecord
	provider          document.RenditionProvider
	embedders         map[string]document.EmbeddingProvider
	embeddingRuntimes map[string]*ProviderEmbeddingRuntime
	tokenizers        map[string]document.Tokenizer
}

type Service struct {
	catalog        *store.Store
	blobs          *blob.Store
	gate           processingOperationGate
	profiles       map[string]configuredProfile
	principal      string
	scope          string
	spoolDirectory string
	clock          func() time.Time
	renditions     *RenditionRuntimeRegistry
	embeddings     *EmbeddingRuntimeRegistry
}

type processingOperationGate interface {
	RenditionMutationGate
	MaintainContext(ctx context.Context, fn func() error) error
}

type Selector struct {
	NodeID           int64
	ContentVersionID string
	Profile          string
}

type FlowHop struct {
	Capability       string
	ProviderID       string
	TrustBoundary    string
	InputClasses     []string
	DiscloseFilename bool
	Filename         string
}

type Estimate struct {
	SourceBytes   int64
	ProviderCalls int
	VectorSpaces  int
}

// ProfileSummary describes one locally executable processing profile without
// exposing provider credentials or deployment configuration.
type ProfileSummary struct {
	Name              string
	Fingerprint       string
	Rendition         bool
	EmbeddingBindings []string
}

type Plan struct {
	Fingerprint        string
	VaultUID           string
	Selector           Selector
	ProfileFingerprint string
	Flow               []FlowHop
	DisclosedClasses   []string
	RetainedClasses    []string
	Estimate           Estimate
	ConsentRequired    bool
	BackupConsequence  string
}

type StartRequest struct {
	Selector        Selector
	PlanFingerprint string
	Consent         bool
}

type ConsentGrantRequest struct {
	Selector        Selector
	PlanFingerprint string
	ExpiresAt       *time.Time
}

type ConsentGrant struct {
	PlanFingerprint    string
	ProfileFingerprint string
	ExpiresAt          *time.Time
}

type ConsentRevocation struct {
	RevokedAt time.Time
}

type DerivativePurgeRequest struct {
	ContentVersionIDs []string
	AttachmentIDs     []string
	BuildIDs          []string
	All               bool
}

type DerivativePurgePlan struct {
	Fingerprint                    string
	VaultUID                       string
	Request                        DerivativePurgeRequest
	ImmutableBackupCopiesUntouched bool
}

type DerivativePurgeJobRequest struct {
	DerivativePurgeRequest

	PlanFingerprint string
}

type DerivativePurgeReceipt struct {
	Outcome                          string
	ID                               string
	PlanFingerprint                  string
	RemovedHeads                     int
	RemovedAttachments               int
	RemovedBuilds                    int
	RemovedArtifacts                 int
	RemovedLexicalSegments           int
	RemovedEmbeddingHeads            int
	RemovedEmbeddingSets             int
	PhysicalDerivativeBlobsReclaimed int
	ReclaimedFiles                   int
	ImmutableBackupCopiesUntouched   bool
}

type Job struct {
	ID                 string
	RenditionJobID     string
	AttachmentID       string
	EmbeddingJobIDs    []string
	ProfileFingerprint string
	ContentVersionID   string
}

type Status struct {
	JobID             string
	State             string
	Phase             string
	FailureCode       string
	EmbeddingJobIDs   []string
	CompletedBindings int
}

type Rendition struct {
	VaultUID           string
	NodeID             int64
	ContentVersionID   string
	ProfileFingerprint string
	AttachmentID       string
	BuildID            string
	ArtifactID         string
	SHA256             string
	Size               int64
	Completeness       string
	Warnings           []string
	Reader             packstore.VerifiedReadCloser
}

type SourceFence struct {
	VaultUID          string
	ContentVersionIDs []string
}

type CoverageClass struct {
	Name, State                  string
	Required                     bool
	Complete, Unavailable, Stale int
	Ineligible, Total            int
}

type Coverage struct {
	VaultUID, ProfileFingerprint, State string
	Renditions                          CoverageClass
	Embeddings                          []CoverageClass
}

type SearchRequest struct {
	Query, Mode, Profile, BindingID string
	Limit                           int
	Fence                           SourceFence
	Explain                         bool
}

type SearchReport = retrieval.Report

func NewService(config ServiceConfig) (*Service, error) {
	if config.Catalog == nil || config.Blobs == nil || renditionInterfaceNil(config.Gate) {
		return nil, errors.New("processing service requires catalog, blob store, and operation gate")
	}
	if !filepath.IsAbs(config.SpoolDirectory) {
		return nil, errors.New("processing service spool directory must be absolute")
	}
	if config.Clock == nil {
		config.Clock = func() time.Time { return time.Now().UTC() }
	}
	if config.Principal == "" {
		config.Principal = "embedded:operator"
	}
	if config.Scope == "" {
		config.Scope = "document-processing"
	}
	service := &Service{catalog: config.Catalog, blobs: config.Blobs, gate: config.Gate,
		profiles:       make(map[string]configuredProfile, len(config.Profiles)),
		principal:      config.Principal,
		scope:          config.Scope,
		spoolDirectory: config.SpoolDirectory, clock: config.Clock,
		renditions: NewRenditionRuntimeRegistry(), embeddings: NewEmbeddingRuntimeRegistry()}
	registeredRenditions := make(map[string]document.RenditionProvider)
	registeredEmbeddings := make(map[string]document.EmbeddingProvider)
	for name, supplied := range config.Profiles {
		if err := validateProfileName(name); err != nil {
			return nil, err
		}
		canonical, fingerprints, err := document.CanonicalProfile(supplied.Profile)
		if err != nil {
			return nil, fmt.Errorf("processing profile %q: %w", name, err)
		}
		var profile document.ProcessingProfileV1
		if err := json.Unmarshal(canonical, &profile, json.RejectUnknownMembers(true)); err != nil {
			return nil, fmt.Errorf("processing profile %q canonical decode: %w", name, err)
		}
		configured := configuredProfile{portable: profile, provider: supplied.RenditionProvider,
			embedders:         make(map[string]document.EmbeddingProvider, len(supplied.EmbeddingProviders)),
			embeddingRuntimes: make(map[string]*ProviderEmbeddingRuntime, len(profile.Embeddings)),
			tokenizers:        make(map[string]document.Tokenizer, len(supplied.Tokenizers)),
			record: store.ProcessingProfileRecord{Fingerprint: fingerprints.Profile,
				CanonicalProfile: jsontext.Value(canonical), RenditionRequestFingerprint: fingerprints.RenditionRequest,
				EvidenceLexicalFingerprint:     fingerprints.EvidenceLexical,
				RetentionDisclosureFingerprint: fingerprints.RetentionDisclosure,
				AttachmentPolicyFingerprint:    profile.RetentionDisclosure.AttachmentPolicyFingerprint,
				ConsentFingerprint:             profile.RetentionDisclosure.ConsentFingerprint,
				TrustBoundary:                  profile.RetentionDisclosure.TrustBoundary}}
		if profile.Rendition != nil {
			configured.record.RenditionDisclosureFingerprint = profile.Rendition.DisclosureFingerprint
			if renditionInterfaceNil(supplied.RenditionProvider) {
				return nil, fmt.Errorf("processing profile %q requires a rendition provider", name)
			}
			descriptor := supplied.RenditionProvider.Descriptor()
			if descriptor.ID != profile.Rendition.Descriptor.ID ||
				descriptor.Fingerprint != profile.Rendition.Descriptor.Fingerprint ||
				string(descriptor.TrustBoundary) != profile.Rendition.TrustBoundary {
				return nil, fmt.Errorf("processing profile %q rendition provider differs from its descriptor", name)
			}
			switch descriptor.TrustBoundary {
			case document.RenditionTrustLocalProcess, document.RenditionTrustOperatorNetwork, document.RenditionTrustHostedProvider:
			default:
				return nil, fmt.Errorf("processing profile %q rendition provider has an invalid trust boundary", name)
			}
			runtime := &providerRenditionRuntime{provider: supplied.RenditionProvider,
				blobs: config.Blobs, spoolDirectory: config.SpoolDirectory, clock: config.Clock}
			if existing, exists := registeredRenditions[descriptor.Fingerprint]; !exists {
				if err := service.renditions.Register(descriptor.Fingerprint, runtime); err != nil {
					return nil, fmt.Errorf("processing profile %q: %w", name, err)
				}
				registeredRenditions[descriptor.Fingerprint] = supplied.RenditionProvider
			} else if !sameProvider(existing, supplied.RenditionProvider) {
				return nil, fmt.Errorf("processing profile %q rendition provider %q conflicts with "+
					"another profile's provider for the same descriptor", name, descriptor.ID)
			}
		}
		for _, binding := range profile.Embeddings {
			provider := supplied.EmbeddingProviders[binding.Name]
			if renditionInterfaceNil(provider) {
				return nil, fmt.Errorf("processing profile %q embedding %q is unavailable", name, binding.Name)
			}
			descriptor := provider.Descriptor()
			if descriptor.ID != binding.Descriptor.ID || descriptor.Fingerprint != binding.Descriptor.Fingerprint ||
				string(descriptor.TrustBoundary) != binding.TrustBoundary {
				return nil, fmt.Errorf("processing profile %q embedding %q differs from its descriptor", name, binding.Name)
			}
			switch descriptor.TrustBoundary {
			case document.EmbeddingTrustLocalProcess, document.EmbeddingTrustOperatorNetwork, document.EmbeddingTrustHostedProvider:
			default:
				return nil, fmt.Errorf("processing profile %q embedding %q has an invalid trust boundary", name, binding.Name)
			}
			classifier := supplied.EmbeddingClassifiers[binding.Name]
			if classifier == nil {
				classifier = classifyEmbeddingProviderError
			}
			runtime, err := NewProviderEmbeddingRuntime(provider, config.Blobs,
				config.SpoolDirectory, classifier)
			if err != nil {
				return nil, err
			}
			if existing, exists := registeredEmbeddings[descriptor.Fingerprint]; !exists {
				if err := service.embeddings.Register(descriptor.Fingerprint, runtime); err != nil {
					return nil, fmt.Errorf("processing profile %q embedding %q: %w", name, binding.Name, err)
				}
				registeredEmbeddings[descriptor.Fingerprint] = provider
			} else if !sameProvider(existing, provider) {
				return nil, fmt.Errorf("processing profile %q embedding %q conflicts with "+
					"another profile's provider for the same descriptor", name, binding.Name)
			}
			configured.embedders[binding.Name] = provider
			configured.embeddingRuntimes[binding.Name] = runtime
			if binding.InputKind == document.EmbeddingInputRenditionChunk {
				tokenizer := supplied.Tokenizers[binding.Name]
				if renditionInterfaceNil(tokenizer) {
					return nil, fmt.Errorf("processing profile %q embedding %q requires a tokenizer", name, binding.Name)
				}
				identity := tokenizer.Identity()
				if binding.Chunk == nil || (identity.Name != binding.Chunk.Tokenizer || identity.Revision != binding.Chunk.TokenizerRevision) {
					return nil, fmt.Errorf("processing profile %q embedding %q tokenizer differs from its binding", name, binding.Name)
				}
				configured.tokenizers[binding.Name] = tokenizer
			}
		}
		service.profiles[name] = configured
	}
	return service, nil
}

func (service *Service) Profiles() []ProfileSummary {
	result := make([]ProfileSummary, 0, len(service.profiles))
	for name, profile := range service.profiles {
		bindings := make([]string, 0, len(profile.portable.Embeddings))
		for _, binding := range profile.portable.Embeddings {
			bindings = append(bindings, binding.Name)
		}
		sort.Strings(bindings)
		result = append(result, ProfileSummary{Name: name, Fingerprint: profile.record.Fingerprint,
			Rendition: profile.portable.Rendition != nil, EmbeddingBindings: bindings})
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Name < result[j].Name })
	return result
}

func (service *Service) Plan(ctx context.Context, selector Selector) (Plan, error) {
	node, version, profile, err := service.resolve(ctx, selector)
	if err != nil {
		return Plan{}, err
	}
	plan, err := service.planForSource(selector, node, version, profile)
	if err != nil {
		return Plan{}, err
	}
	// Current grants are advisory; execution checks them again. Grant changes
	// do not change the reviewed source, disclosure, or plan fingerprint.
	plan.ConsentRequired = false
	for _, request := range service.profileConsentRequests(profile) {
		_, err := service.catalog.AuthorizeProviderOperation(ctx, request)
		if errors.Is(processingConsentBoundaryError(err), ErrConsentRequired) {
			plan.ConsentRequired = true
		} else if err != nil {
			return Plan{}, err
		}
	}
	return plan, nil
}

func (service *Service) planForSource(selector Selector, node store.Node,
	version store.ContentVersion, profile configuredProfile,
) (Plan, error) {
	plan := Plan{VaultUID: service.catalog.VaultID(), Selector: selector,
		ProfileFingerprint: profile.record.Fingerprint, ConsentRequired: true,
		Estimate:          Estimate{SourceBytes: version.Size, VectorSpaces: len(profile.portable.Embeddings)},
		BackupConsequence: "retained derivatives are included in catalog-authorized backups"}
	if profile.portable.Rendition != nil {
		hop := FlowHop{Capability: "rendition",
			ProviderID:       profile.portable.Rendition.Descriptor.ID,
			TrustBoundary:    profile.portable.Rendition.TrustBoundary,
			InputClasses:     []string{string(document.RenditionInputOriginalFile)},
			DiscloseFilename: profile.portable.Rendition.DiscloseFilename}
		if hop.DiscloseFilename {
			hop.Filename = syntheticFilename(node.Name, version.MimeType, true)
			hop.InputClasses = append(hop.InputClasses, "filename")
			plan.DisclosedClasses = append(plan.DisclosedClasses, "filename")
		}
		plan.Flow = append(plan.Flow, hop)
		plan.DisclosedClasses = append(plan.DisclosedClasses, string(document.RenditionInputOriginalFile))
		plan.Estimate.ProviderCalls++
	}
	if profile.portable.Rendition != nil {
		if profile.portable.RetentionDisclosure.RetainSanitizedMarkdown {
			plan.RetainedClasses = append(plan.RetainedClasses, "sanitized_markdown")
		}
		plan.RetainedClasses = append(plan.RetainedClasses, "normalized_evidence")
		if profile.portable.RetentionDisclosure.RetainProviderMarkdown {
			plan.RetainedClasses = append(plan.RetainedClasses, "provider_markdown")
		}
		if profile.portable.RetentionDisclosure.RetainTypedArtifacts {
			plan.RetainedClasses = append(plan.RetainedClasses, "typed_artifacts")
		}
	}
	for _, binding := range profile.portable.Embeddings {
		plan.Flow = append(plan.Flow, FlowHop{Capability: "embedding", ProviderID: binding.Descriptor.ID,
			TrustBoundary: binding.TrustBoundary, InputClasses: []string{string(binding.InputKind)}})
		plan.DisclosedClasses = append(plan.DisclosedClasses, string(binding.InputKind))
		plan.Estimate.ProviderCalls++
		plan.RetainedClasses = append(plan.RetainedClasses, "embedding_vector_set")
		if profile.embedders[binding.Name].Descriptor().SupportsTextQuery {
			plan.Flow = append(plan.Flow, FlowHop{Capability: "query_embedding", ProviderID: binding.Descriptor.ID,
				TrustBoundary: binding.TrustBoundary, InputClasses: []string{"query_text"}})
			plan.DisclosedClasses = append(plan.DisclosedClasses, "query_text")
		}
	}
	plan.DisclosedClasses = sortedUnique(plan.DisclosedClasses)
	plan.RetainedClasses = sortedUnique(plan.RetainedClasses)
	var err error
	plan.Fingerprint, err = planFingerprint(plan)
	return plan, err
}

func (service *Service) Start(ctx context.Context, request StartRequest) (Job, error) {
	node, version, profile, err := service.resolve(ctx, request.Selector)
	if err != nil {
		return Job{}, err
	}
	// Preview validation and execution must use the same source snapshot.
	plan, err := service.planForSource(request.Selector, node, version, profile)
	if err != nil {
		return Job{}, err
	}
	if request.PlanFingerprint == "" || request.PlanFingerprint != plan.Fingerprint {
		return Job{}, ErrPlanChanged
	}
	if request.Consent {
		if err := service.grantProfileConsent(ctx, profile, nil); err != nil {
			return Job{}, err
		}
	}
	principal, scope := service.principal, service.scope
	processingJobID, renditionJobID, attachmentID := "", "", ""
	if profile.portable.Rendition != nil {
		var renditionRun renditionRun
		renditionRun, err = service.runRendition(ctx, node, version, profile, principal, scope)
		if err != nil {
			if errors.Is(err, ErrConsentRequired) {
				// Durable job failures record the consent category, not its original
				// cause. Report this caller's current authority; a renewed grant must
				// not turn previously denied work into a successful result.
				_, consentErr := service.catalog.AuthorizeProviderOperation(ctx, service.renditionConsentRequest(profile))
				if consentErr != nil {
					err = consentErr
				}
			}
			return Job{}, processingConsentBoundaryError(err)
		}
		processingJobID, renditionJobID, attachmentID = renditionRun.waiterID, renditionRun.jobID, renditionRun.attachmentID
	}
	embeddingJobIDs, err := service.runEmbeddings(ctx, version, profile, principal, scope)
	if err != nil {
		return Job{}, processingConsentBoundaryError(err)
	}
	if processingJobID == "" && len(embeddingJobIDs) != 0 {
		processingJobID = embeddingJobIDs[0]
	}
	if processingJobID == "" {
		return Job{}, errors.New("processing profile has no executable stage")
	}
	return Job{ID: processingJobID, RenditionJobID: renditionJobID, AttachmentID: attachmentID,
		EmbeddingJobIDs: embeddingJobIDs, ProfileFingerprint: profile.record.Fingerprint,
		ContentVersionID: version.ID}, nil
}

func processingConsentBoundaryError(err error) error {
	if errors.Is(err, store.ErrProcessingConsentRequired) ||
		errors.Is(err, store.ErrProcessingConsentExpired) ||
		errors.Is(err, store.ErrProcessingConsentRevoked) {
		return errors.Join(ErrConsentRequired, err)
	}
	return err
}

func (service *Service) GrantConsent(ctx context.Context, request ConsentGrantRequest) (ConsentGrant, error) {
	node, version, profile, err := service.resolve(ctx, request.Selector)
	if err != nil {
		return ConsentGrant{}, err
	}
	plan, err := service.planForSource(request.Selector, node, version, profile)
	if err != nil {
		return ConsentGrant{}, err
	}
	if request.PlanFingerprint == "" || request.PlanFingerprint != plan.Fingerprint {
		return ConsentGrant{}, ErrPlanChanged
	}
	if request.ExpiresAt != nil && !request.ExpiresAt.After(service.clock()) {
		return ConsentGrant{}, fmt.Errorf("%w: expiry must be in the future", ErrInvalidConsentExpiry)
	}
	if err := service.grantProfileConsent(ctx, profile, request.ExpiresAt); err != nil {
		return ConsentGrant{}, err
	}
	return ConsentGrant{PlanFingerprint: plan.Fingerprint, ProfileFingerprint: profile.record.Fingerprint,
		ExpiresAt: request.ExpiresAt}, nil
}

func (service *Service) RevokeConsent(ctx context.Context) (ConsentRevocation, error) {
	var result ConsentRevocation
	err := service.gate.MutateContext(ctx, func() error {
		revocation, err := service.catalog.RevokeConsent(ctx, store.ProcessingConsentRevocationRequest{
			Principal: service.principal, Scope: service.scope})
		result.RevokedAt = revocation.RevokedAt
		return err
	})
	return result, err
}

func (service *Service) PlanDerivativePurge(ctx context.Context,
	request DerivativePurgeRequest,
) (DerivativePurgePlan, error) {
	normalized, err := normalizeDerivativePurgeRequest(request)
	if err != nil {
		return DerivativePurgePlan{}, err
	}
	state, err := service.catalog.DerivativePurgeFingerprint(ctx, store.PurgeRequest{
		ContentVersionIDs: normalized.ContentVersionIDs, AttachmentIDs: normalized.AttachmentIDs,
		BuildIDs: normalized.BuildIDs, All: normalized.All})
	if err != nil {
		return DerivativePurgePlan{}, fmt.Errorf("fingerprinting derivative authority: %w", err)
	}
	payload := struct {
		Contract string                 `json:"contract"`
		VaultUID string                 `json:"vault_uid"`
		State    string                 `json:"state"`
		Request  DerivativePurgeRequest `json:"request"`
	}{Contract: "docbank-derivative-purge-plan/v1", VaultUID: service.catalog.VaultID(),
		State: state, Request: normalized}
	canonical, err := json.Marshal(payload, json.Deterministic(true))
	if err != nil {
		return DerivativePurgePlan{}, err
	}
	digest := sha256.Sum256(canonical)
	return DerivativePurgePlan{Fingerprint: hex.EncodeToString(digest[:]), VaultUID: service.catalog.VaultID(),
		Request: normalized, ImmutableBackupCopiesUntouched: true}, nil
}

func (service *Service) RunDerivativePurge(ctx context.Context,
	request DerivativePurgeJobRequest,
) (DerivativePurgeReceipt, error) {
	var receipt DerivativePurgeReceipt
	err := service.gate.MaintainContext(ctx, func() error {
		plan, err := service.PlanDerivativePurge(ctx, request.DerivativePurgeRequest)
		if err != nil {
			return err
		}
		if request.PlanFingerprint == "" || request.PlanFingerprint != plan.Fingerprint {
			return ErrPurgePlanChanged
		}
		report, purgeErr := maintenance.PurgeDerivatives(ctx, service.catalog, service.blobs, store.PurgeRequest{
			ContentVersionIDs: plan.Request.ContentVersionIDs, AttachmentIDs: plan.Request.AttachmentIDs,
			BuildIDs: plan.Request.BuildIDs, All: plan.Request.All})
		if !report.CatalogCommitted {
			return purgeErr
		}
		outcome := "completed"
		if errors.Is(purgeErr, packstore.ErrPackRetirementDeferred) {
			outcome = "deferred"
		} else if purgeErr != nil {
			outcome = "partial"
		}
		receipt = DerivativePurgeReceipt{Outcome: outcome, PlanFingerprint: plan.Fingerprint,
			RemovedHeads: report.Purge.RemovedHeads, RemovedAttachments: report.Purge.RemovedAttachments,
			RemovedBuilds: report.Purge.RemovedBuilds, RemovedArtifacts: report.Purge.RemovedArtifacts,
			RemovedLexicalSegments:           report.Purge.RemovedLexicalSegments,
			RemovedEmbeddingHeads:            report.Purge.RemovedEmbeddingHeads,
			RemovedEmbeddingSets:             report.Purge.RemovedEmbeddingSets,
			PhysicalDerivativeBlobsReclaimed: report.Physical.RemovedBlobs,
			ReclaimedFiles:                   report.Physical.ReclaimedFiles,
			ImmutableBackupCopiesUntouched:   report.Purge.ImmutableBackupCopiesUntouched}
		encoded, err := json.Marshal(receipt, json.Deterministic(true))
		if err != nil {
			return err
		}
		receipt.ID = stableHash("docbank/derivative-purge-receipt/v1", string(encoded))
		return purgeErr
	})
	return receipt, err
}

func normalizeDerivativePurgeRequest(request DerivativePurgeRequest) (DerivativePurgeRequest, error) {
	if request.All && (len(request.ContentVersionIDs) != 0 || len(request.AttachmentIDs) != 0 || len(request.BuildIDs) != 0) {
		return DerivativePurgeRequest{}, fmt.Errorf("%w: vault-wide purge cannot include selectors", ErrInvalidPurgeRequest)
	}
	if !request.All && len(request.ContentVersionIDs) == 0 && len(request.AttachmentIDs) == 0 && len(request.BuildIDs) == 0 {
		return DerivativePurgeRequest{}, fmt.Errorf("%w: a selector or all=true is required", ErrInvalidPurgeRequest)
	}
	normalize := func(subject string, values []string, uuidValues bool) ([]string, error) {
		if len(values) > 1000 {
			return nil, fmt.Errorf("%w: at most 1000 %s IDs are accepted", ErrInvalidPurgeRequest, subject)
		}
		result := slices.Clone(values)
		sort.Strings(result)
		for index, value := range result {
			valid := len(value) == sha256.Size*2
			if uuidValues {
				parsed, err := uuid.Parse(value)
				valid = err == nil && parsed[6]>>4 == 4 && parsed[8]>>6 == 2 && parsed.String() == value
			} else if valid {
				decoded, err := hex.DecodeString(value)
				valid = err == nil && len(decoded) == sha256.Size && value == strings.ToLower(value)
			}
			if !valid {
				return nil, fmt.Errorf("%w: %s ID is invalid", ErrInvalidPurgeRequest, subject)
			}
			if index > 0 && result[index-1] == value {
				return nil, fmt.Errorf("%w: %s ID is duplicated", ErrInvalidPurgeRequest, subject)
			}
		}
		return result, nil
	}
	var err error
	request.ContentVersionIDs, err = normalize("content version", request.ContentVersionIDs, true)
	if err != nil {
		return DerivativePurgeRequest{}, err
	}
	request.AttachmentIDs, err = normalize("attachment", request.AttachmentIDs, false)
	if err != nil {
		return DerivativePurgeRequest{}, err
	}
	request.BuildIDs, err = normalize("build", request.BuildIDs, false)
	return request, err
}

func (service *Service) renditionConsentRequest(profile configuredProfile) store.ProviderOperationAuthorizationRequest {
	return store.ProviderOperationAuthorizationRequest{
		Principal: service.principal, Scope: service.scope, ProfileFingerprint: profile.record.Fingerprint,
		DisclosureFingerprint:   profile.record.RenditionDisclosureFingerprint,
		InputClasses:            []string{string(document.RenditionInputOriginalFile)},
		RetainedArtifactClasses: retainedRenditionClasses(profile.portable),
	}
}

func (service *Service) profileConsentRequests(profile configuredProfile) []store.ProviderOperationAuthorizationRequest {
	var requests []store.ProviderOperationAuthorizationRequest
	if profile.portable.Rendition != nil {
		requests = append(requests, service.renditionConsentRequest(profile))
	}
	for _, binding := range profile.portable.Embeddings {
		request := store.ProviderOperationAuthorizationRequest{
			Principal: service.principal, Scope: service.scope, ProfileFingerprint: profile.record.Fingerprint,
			DisclosureFingerprint: binding.DisclosureFingerprint, InputClasses: []string{string(binding.InputKind)},
			RetainedArtifactClasses: []string{"embedding_vector_set"},
		}
		requests = append(requests, request)
		if profile.embedders[binding.Name].Descriptor().SupportsTextQuery {
			request.InputClasses = []string{"query_text"}
			request.RetainedArtifactClasses = nil
			requests = append(requests, request)
		}
	}
	return requests
}

func (service *Service) grantProfileConsent(ctx context.Context, profile configuredProfile, expiresAt *time.Time) error {
	return service.gate.MutateContext(ctx, func() error {
		for _, request := range service.profileConsentRequests(profile) {
			_, err := service.catalog.GrantConsent(ctx, store.ProcessingConsentGrantRequest{
				Principal: request.Principal, Scope: request.Scope, ProfileFingerprint: request.ProfileFingerprint,
				DisclosureFingerprint: request.DisclosureFingerprint, InputClasses: request.InputClasses,
				RetainedArtifactClasses: request.RetainedArtifactClasses, ExpiresAt: expiresAt})
			if err != nil {
				return err
			}
		}
		return nil
	})
}

type renditionRun struct{ jobID, waiterID, attachmentID string }

func (service *Service) runRendition(ctx context.Context, node store.Node, version store.ContentVersion,
	profile configuredProfile, principal, scope string,
) (renditionRun, error) {
	prepared, err := service.prepareRendition(ctx, node, version, profile, false)
	if err != nil {
		return renditionRun{}, err
	}
	preflightWork := store.RenditionJobWork{VaultID: service.catalog.VaultID(),
		Job:     store.RenditionJob{SourceSHA256: version.BlobHash},
		Waiter:  store.RenditionJobWaiter{ContentVersionID: version.ID},
		Profile: profile.record, ExecutionIdentity: prepared.identity}
	preflightExecution, err := service.renditions.Prepare(ctx, preflightWork, service.clock())
	if err != nil {
		return renditionRun{}, fmt.Errorf("preparing rendition execution: %w", err)
	}
	if preflightExecution.Upload == nil {
		return renditionRun{}, errors.New("rendition preflight did not provide an upload")
	}
	defer func() { _ = preflightExecution.Upload.Close() }()
	if err := validateRenditionExecution(preflightWork, preflightExecution); err != nil {
		return renditionRun{}, fmt.Errorf("validating rendition execution: %w", err)
	}
	preflightIdentity, err := document.NewRenditionExecutionIdentityV1(
		preflightExecution.Upload.Metadata(), preflightExecution.Authorization,
		preflightExecution.EvidencePolicy, preflightExecution.RenditionPolicy)
	if err != nil {
		return renditionRun{}, err
	}
	wantIdentity, _, _ := document.CanonicalRenditionExecutionIdentityV1(prepared.identity)
	gotIdentity, _, _ := document.CanonicalRenditionExecutionIdentityV1(preflightIdentity)
	if !bytes.Equal(wantIdentity, gotIdentity) {
		return renditionRun{}, errors.New("planned rendition execution differs from executable runtime")
	}
	retained := retainedRenditionClasses(profile.portable)
	var job store.RenditionJob
	var waiter store.RenditionJobWaiter
	err = service.gate.MutateContext(ctx, func() error {
		var enqueueErr error
		job, waiter, enqueueErr = service.catalog.EnqueueRenditionJob(ctx, store.RenditionJobRequest{
			ContentVersionID: version.ID, Profile: profile.record,
			CapturedArtifactPolicy: prepared.capturedPolicy, ExecutionIdentity: prepared.identity,
			Authorization: store.ProviderOperationAuthorizationRequest{Principal: principal, Scope: scope,
				ProfileFingerprint:    profile.record.Fingerprint,
				DisclosureFingerprint: profile.record.RenditionDisclosureFingerprint,
				InputClasses:          []string{string(document.RenditionInputOriginalFile)}, RetainedArtifactClasses: retained},
		})
		return enqueueErr
	})
	if err != nil {
		return renditionRun{}, err
	}
	if current, statusErr := service.catalog.RenditionJobByID(ctx, job.ID); statusErr == nil &&
		current.State == store.RenditionJobCompleted {
		return service.renditionResult(ctx, waiter.ID)
	}
	worker, err := NewRenditionWorker(RenditionWorkerConfig{Catalog: service.catalog, Blobs: service.blobs,
		Runtime: service.renditions, Gate: service.gate, Owner: "embedded-rendition-worker",
		LeaseDuration: 5 * time.Minute, IdleDelay: time.Millisecond, Clock: service.clock})
	if err != nil {
		return renditionRun{}, err
	}
	for {
		_, err := worker.RunJob(ctx, job.ID)
		current, statusErr := service.catalog.RenditionJobByID(ctx, job.ID)
		if statusErr != nil {
			return renditionRun{}, errors.Join(err, statusErr)
		}
		switch current.State {
		case store.RenditionJobQueued, store.RenditionJobRunning, store.RenditionJobRetryWait:
		case store.RenditionJobCompleted:
			return service.renditionResult(ctx, waiter.ID)
		case store.RenditionJobFailed:
			if current.FailureCode == store.RenditionFailureConsent {
				return renditionRun{}, ErrConsentRequired
			}
			return renditionRun{}, fmt.Errorf("%w: %s", ErrRenditionFailed, current.FailureCode)
		case store.RenditionJobOperatorRequired:
			return renditionRun{}, ErrRenditionOperatorRequired
		}
		if err != nil {
			return renditionRun{}, err
		}
		// Shared work and provider retries must finish before building embeddings.
		if err := waitRenditionWorker(ctx, 100*time.Millisecond); err != nil {
			return renditionRun{}, err
		}
	}
}

func (service *Service) renditionResult(ctx context.Context, waiterID string) (renditionRun, error) {
	waiter, err := service.catalog.RenditionJobWaiterByID(ctx, waiterID)
	if err != nil {
		return renditionRun{}, err
	}
	// Shared work can finish while rejecting this request's publication authority.
	if waiter.State == "published" {
		return renditionRun{jobID: waiter.JobID, waiterID: waiter.ID, attachmentID: waiter.AttachmentID}, nil
	}
	if waiter.FailureCode == store.RenditionFailureConsent {
		return renditionRun{}, ErrConsentRequired
	}
	if waiter.FailureCode == store.RenditionFailureStaleAuthority {
		return renditionRun{}, ErrPlanChanged
	}
	return renditionRun{}, fmt.Errorf("%w: rendition request is %s", ErrRenditionFailed, waiter.State)
}

func (service *Service) Status(ctx context.Context, jobID string) (Status, error) {
	waiter, waiterErr := service.catalog.RenditionJobWaiterByID(ctx, jobID)
	if waiterErr == nil {
		if waiter.State == "rejected" {
			failureCode := string(waiter.FailureCode)
			if failureCode == "" {
				failureCode = "authorization"
			}
			return Status{JobID: jobID, State: "failed", Phase: "authorization",
				FailureCode: failureCode, EmbeddingJobIDs: []string{}}, nil
		}
		var rendition store.RenditionJob
		if waiter.State == "published" {
			rendition = store.RenditionJob{ID: waiter.JobID,
				State: store.RenditionJobCompleted, Phase: store.RenditionPhasePublished}
		} else {
			var err error
			rendition, err = service.catalog.RenditionJobByID(ctx, waiter.JobID)
			if err != nil {
				return Status{}, err
			}
		}
		embeddings, err := service.catalog.EmbeddingJobsForVersionProfile(ctx,
			waiter.ContentVersionID, waiter.ProfileFingerprint)
		if err != nil {
			return Status{}, err
		}
		return aggregateStatus(jobID, &rendition, embeddings), nil
	}
	if !errors.Is(waiterErr, store.ErrNotFound) {
		return Status{}, waiterErr
	}
	embedding, embeddingErr := service.catalog.EmbeddingJobByID(ctx, jobID)
	if embeddingErr == nil {
		embeddings, err := service.catalog.EmbeddingJobsForVersionProfile(ctx,
			embedding.ContentVersionID, embedding.ProfileFingerprint)
		if err != nil {
			return Status{}, err
		}
		return aggregateStatus(jobID, nil, embeddings), nil
	}
	if !errors.Is(embeddingErr, store.ErrNotFound) {
		return Status{}, embeddingErr
	}
	// RenditionJobID remains separately exposed for callers that intentionally
	// want only the shared rendition-stage status.
	rendition, err := service.catalog.RenditionJobByID(ctx, jobID)
	if err != nil {
		return Status{}, err
	}
	return aggregateStatus(jobID, &rendition, nil), nil
}

func aggregateStatus(jobID string, rendition *store.RenditionJob,
	embeddings []store.EmbeddingJobStatus,
) Status {
	status := Status{JobID: jobID, State: "completed", Phase: "embedding",
		EmbeddingJobIDs: make([]string, len(embeddings))}
	if rendition != nil {
		status.State, status.Phase, status.FailureCode = string(rendition.State),
			string(rendition.Phase), string(rendition.FailureCode)
	}
	for index, embedding := range embeddings {
		status.EmbeddingJobIDs[index] = embedding.ID
		if embedding.State == "completed" {
			status.CompletedBindings++
		}
	}
	if rendition != nil && rendition.State != store.RenditionJobCompleted {
		return status
	}
	for _, wanted := range []string{"failed", "abandoned", "retry_wait", "running", "queued", "partial"} {
		for _, embedding := range embeddings {
			state := embedding.State
			if embedding.Activation == document.EmbeddingOptional && (state == "failed" || state == "abandoned") {
				state = "partial"
			}
			if state == wanted {
				status.State, status.Phase = state, "embedding"
				status.FailureCode = string(embedding.FailureCode)
				return status
			}
		}
	}
	for _, embedding := range embeddings {
		if embedding.State != "completed" {
			status.State, status.Phase = embedding.State, "embedding"
			status.FailureCode = string(embedding.FailureCode)
			return status
		}
	}
	if len(embeddings) != 0 {
		status.State, status.Phase, status.FailureCode = "completed", "embedding", ""
	}
	return status
}

func (service *Service) Rendition(ctx context.Context, selector Selector, limit int64) (Rendition, error) {
	node, _, profile, err := service.resolve(ctx, selector)
	if err != nil {
		return Rendition{}, err
	}
	view, err := service.catalog.ActiveRendition(ctx, selector.ContentVersionID, profile.record.Fingerprint)
	if err != nil {
		return Rendition{}, err
	}
	return service.renditionFromView(ctx, node, selector.ContentVersionID, view, limit)
}

func (service *Service) RenditionByAttachment(ctx context.Context, attachmentID string, limit int64) (Rendition, error) {
	view, err := service.catalog.ActiveRenditionByAttachment(ctx, attachmentID)
	if err != nil {
		return Rendition{}, err
	}
	version, err := service.catalog.ContentVersionByID(ctx, view.Attachment.ContentVersionID)
	if err != nil {
		return Rendition{}, err
	}
	node, err := service.catalog.NodeByID(ctx, version.NodeID)
	if err != nil {
		return Rendition{}, err
	}
	return service.renditionFromView(ctx, node, version.ID, view, limit)
}

func (service *Service) renditionFromView(ctx context.Context, node store.Node, contentVersionID string,
	view store.RenditionView, limit int64,
) (Rendition, error) {
	if limit == 0 {
		limit = MaxRenditionBytes
	}
	if limit < 1 || limit > MaxRenditionBytes {
		return Rendition{}, errors.New("rendition byte limit is invalid")
	}
	var artifact store.RenditionArtifactRecord
	for _, candidate := range view.Build.Artifacts {
		if candidate.Role == "sanitized_markdown" {
			artifact = candidate
			break
		}
	}
	if artifact.ID == "" {
		return Rendition{}, store.ErrNotFound
	}
	if artifact.Size > limit {
		return Rendition{}, errors.New("rendition exceeds requested byte limit")
	}
	reader, size, err := service.blobs.OpenStreamContext(ctx, artifact.BlobHash)
	if err != nil {
		return Rendition{}, err
	}
	if size != artifact.Size {
		_ = reader.Close()
		return Rendition{}, errors.New("rendition blob size disagrees with catalog authority")
	}
	return Rendition{VaultUID: service.catalog.VaultID(), NodeID: node.ID,
		ContentVersionID: contentVersionID, ProfileFingerprint: view.Attachment.Profile.Fingerprint,
		AttachmentID: view.Attachment.ID, BuildID: view.Build.ID, ArtifactID: artifact.ID,
		SHA256: artifact.BlobHash, Size: artifact.Size, Completeness: string(view.Build.Completeness),
		Warnings: slices.Clone(view.Build.Warnings), Reader: reader}, nil
}

func (service *Service) Coverage(ctx context.Context, profileName string, fence SourceFence) (Coverage, error) {
	profile, ids, err := service.profileFence(profileName, fence)
	if err != nil {
		return Coverage{}, err
	}
	report := Coverage{VaultUID: service.catalog.VaultID(), ProfileFingerprint: profile.record.Fingerprint,
		State: "complete", Renditions: CoverageClass{Name: "rendition", Required: profile.portable.Rendition != nil,
			State: "not_required", Total: len(ids)}}
	if profile.portable.Rendition != nil {
		for _, id := range ids {
			version, err := service.catalog.ContentVersionByID(ctx, id)
			var node store.Node
			if err == nil {
				node, err = service.catalog.NodeByID(ctx, version.NodeID)
			}
			if err != nil && !errors.Is(err, store.ErrNotFound) {
				return Coverage{}, err
			}
			if errors.Is(err, store.ErrNotFound) || node.CurrentVersionID != id || node.TrashedAt != nil {
				report.Renditions.Stale++
				continue
			}
			if _, err := service.catalog.ActiveRendition(ctx, id, profile.record.Fingerprint); err == nil {
				report.Renditions.Complete++
			} else if errors.Is(err, store.ErrNotFound) {
				report.Renditions.Unavailable++
			} else {
				return Coverage{}, err
			}
		}
		report.Renditions.State = coverageClassState(report.Renditions)
		if report.Renditions.State != "complete" {
			report.State = "partial"
		}
	}
	if len(profile.portable.Embeddings) == 0 {
		return report, nil
	}
	required, completeRequired := 0, 0
	optionalIncomplete := false
	for _, binding := range profile.portable.Embeddings {
		validated, err := service.catalog.RevalidateSearchCandidates(ctx, nil,
			store.SearchOptions{ContentVersionIDs: ids}, profile.record.Fingerprint, binding.Name)
		if err != nil {
			return Coverage{}, err
		}
		item := CoverageClass{Name: binding.Name, Required: binding.Activation == document.EmbeddingRequired,
			Total: len(ids), Complete: validated.Coverage.CompleteDocuments,
			Stale:       len(ids) - validated.Coverage.ScopedDocuments,
			Unavailable: validated.Coverage.ScopedDocuments - validated.Coverage.CompleteDocuments}
		item.State = coverageClassState(item)
		if item.Required {
			required++
			if item.State == "complete" {
				completeRequired++
			}
		} else if item.State != "complete" {
			optionalIncomplete = true
		}
		report.Embeddings = append(report.Embeddings, item)
	}
	if completeRequired != required || (required == 0 && optionalIncomplete) {
		report.State = "partial"
	}
	return report, nil
}

func coverageClassState(item CoverageClass) string {
	switch {
	case item.Stale == item.Total:
		return "stale"
	case item.Complete == item.Total:
		return "complete"
	case item.Complete > 0:
		return "partial"
	default:
		return "unavailable"
	}
}

func (service *Service) Search(ctx context.Context, request SearchRequest) (retrieval.Report, error) {
	profile, ids, err := service.profileFence(request.Profile, request.Fence)
	if err != nil {
		return retrieval.Report{}, err
	}
	if strings.TrimSpace(request.Query) == "" {
		return retrieval.Report{}, store.ErrSearchQueryRequired
	}
	mode := retrieval.Mode(request.Mode)
	if mode == "" {
		mode = retrieval.ModeAuto
	}
	limit := request.Limit
	if limit == 0 {
		limit = DefaultSearchLimit
	}
	if limit < 1 || limit > MaxSearchLimit {
		return retrieval.Report{}, errors.New("document search limit is invalid")
	}
	searcherConfig := retrieval.SearcherConfig{Backend: service.catalog,
		Owner: "embedded-document-search", LeaseDuration: 5 * time.Minute}
	var authorization document.EmbeddingAuthorization
	if len(profile.portable.Embeddings) != 0 {
		binding, bindingErr := selectEmbeddingBinding(profile.portable, request.BindingID)
		if bindingErr != nil {
			return retrieval.Report{}, bindingErr
		}
		request.BindingID = binding.Name
		if mode == retrieval.ModeSemantic || mode == retrieval.ModeHybrid {
			_, fence, err := service.catalog.BeginProviderEgress(ctx, store.ProviderOperationAuthorizationRequest{
				Principal: service.principal, Scope: service.scope,
				ProfileFingerprint: profile.record.Fingerprint, DisclosureFingerprint: binding.DisclosureFingerprint,
				InputClasses: []string{"query_text"}, RetainedArtifactClasses: []string{},
			})
			if err != nil {
				return retrieval.Report{}, err
			}
			defer fence.Close()
		}
		searcherConfig.Encoders = service.embeddings
		authorization = document.EmbeddingAuthorization{ProviderID: binding.Descriptor.ID,
			DescriptorFingerprint: binding.Descriptor.Fingerprint,
			PolicyFingerprint:     profile.embedders[binding.Name].Descriptor().PolicyFingerprint,
			MaxBatchItems:         1, MaxInputBytes: binding.MaxInputBytes, MaxResponseBytes: binding.MaxResponseBytes}
	}
	searcher, err := retrieval.NewSearcher(searcherConfig)
	if err != nil {
		return retrieval.Report{}, err
	}
	return searcher.Search(ctx, retrieval.Query{Text: request.Query, Mode: mode, Limit: limit,
		LexicalLimit: profile.portable.Retrieval.LexicalLimit, VectorLimit: profile.portable.Retrieval.VectorLimit,
		Scope:                        store.SearchOptions{ContentVersionIDs: ids},
		ProcessingProfileFingerprint: profile.record.Fingerprint, BindingID: request.BindingID,
		Authorization: authorization})
}

func (service *Service) runEmbeddings(ctx context.Context, version store.ContentVersion,
	profile configuredProfile, principal, scope string,
) ([]string, error) {
	if len(profile.portable.Embeddings) == 0 {
		return []string{}, nil
	}
	_, fingerprints, err := document.CanonicalProfile(profile.portable)
	if err != nil {
		return nil, err
	}
	jobIDs := make([]string, 0, len(profile.portable.Embeddings))
	vectorSpaces := make([]string, 0, len(profile.portable.Embeddings))
	for _, binding := range profile.portable.Embeddings {
		var generation store.EmbeddingInputGenerationRecord
		switch binding.InputKind {
		case document.EmbeddingInputOriginalFile:
			generation = directEmbeddingGeneration(version, profile.record.Fingerprint,
				fingerprints.EmbeddingInput[binding.Name], binding)
		case document.EmbeddingInputRenditionChunk:
			generation, err = service.chunkEmbeddingGeneration(ctx, version, profile, binding)
			if err != nil {
				return nil, err
			}
		default:
			return nil, fmt.Errorf("embedding binding %q has an unsupported input kind", binding.Name)
		}
		authorization := store.ProviderOperationAuthorizationRequest{Principal: principal, Scope: scope,
			ProfileFingerprint: profile.record.Fingerprint, DisclosureFingerprint: binding.DisclosureFingerprint,
			InputClasses: []string{string(binding.InputKind)}, RetainedArtifactClasses: []string{"embedding_vector_set"}}
		var job store.EmbeddingJob
		enqueueErr := service.gate.MutateContext(ctx, func() error {
			var err error
			job, err = service.catalog.EnqueueEmbeddingJob(ctx, store.EmbeddingJobRequest{
				ContentVersionID: version.ID, Profile: profile.record, BindingID: binding.Name,
				Descriptor: profile.embedders[binding.Name].Descriptor(), InputGeneration: generation,
				Authorization: authorization,
			})
			return err
		})
		if enqueueErr != nil {
			if errors.Is(enqueueErr, store.ErrEmbeddingJobFenced) {
				return nil, ErrPlanChanged
			}
			return nil, enqueueErr
		}
		jobIDs = append(jobIDs, job.ID)
		vectorSpaces = append(vectorSpaces, fingerprints.VectorSpace[binding.Name])
	}
	for index, jobID := range jobIDs {
		binding := profile.portable.Embeddings[index]
		worker, err := NewEmbeddingWorker(EmbeddingWorkerConfig{
			Catalog: service.catalog, Authority: service.catalog, Blobs: service.blobs,
			GenerationBlobs: service.blobs, Runtime: profile.embeddingRuntimes[binding.Name], Gate: service.gate,
			Owner: "embedded-embedding-worker", LeaseDuration: 5 * time.Minute, IdleDelay: time.Millisecond,
			RetryLimit: 3, RetryBaseDelay: time.Millisecond, MaxRetryDelay: time.Second,
			AttemptLifetime: 10 * time.Minute, MaxRows: 100_000, MaxDimensions: 1_048_576,
			MaxVectorBlobBytes: 64 << 20, Clock: service.clock,
			DescriptorFingerprints: []string{binding.Descriptor.Fingerprint},
		})
		if err != nil {
			return nil, err
		}
		for {
			processed, runErr := worker.RunJob(ctx, jobID)
			if runErr != nil {
				if isEmbeddingWorkFence(runErr) {
					return nil, ErrPlanChanged
				}
				return nil, runErr
			}
			status, statusErr := service.catalog.EmbeddingJobByID(ctx, jobID)
			if statusErr != nil {
				return nil, statusErr
			}
			if status.State == "abandoned" {
				return nil, ErrPlanChanged
			}
			if status.State == "failed" && status.FailureCode == store.EmbeddingFailureAuthorization {
				// Provider credentials and consent share a durable failure category.
				// Preserve provider partial results when this binding still has consent.
				_, consentErr := service.catalog.AuthorizeProviderOperation(ctx, store.ProviderOperationAuthorizationRequest{
					Principal: principal, Scope: scope, ProfileFingerprint: profile.record.Fingerprint,
					DisclosureFingerprint: binding.DisclosureFingerprint, InputClasses: []string{string(binding.InputKind)},
					RetainedArtifactClasses: []string{"embedding_vector_set"}})
				if consentErr != nil {
					return nil, consentErr
				}
			}
			if processed || status.State == "completed" || status.State == "failed" {
				break
			}
			if status.State != "running" && status.State != "retry_wait" {
				return nil, errors.New("embedding job was not claimable")
			}
			if err := worker.wait(ctx, 100*time.Millisecond); err != nil {
				return nil, err
			}
		}
	}
	indexer, err := vectorworker.NewIndexWorker(vectorworker.IndexWorkerConfig{Catalog: service.catalog,
		ReadVectorSet: func(ctx context.Context, member store.VectorIndexMember) ([]byte, error) {
			return service.catalog.ReadVectorIndexVectorSet(ctx, service.blobs, member)
		},
		Mutate: service.gate.MutateContext, Owner: "embedded-index-worker", BuildLease: 5 * time.Minute,
		ReaderLease: 5 * time.Minute, IdleDelay: time.Millisecond, Clock: service.clock})
	if err != nil {
		return nil, err
	}
	for _, vectorSpace := range sortedUnique(vectorSpaces) {
		for {
			_, err := indexer.Rebuild(ctx, vectorSpace)
			if errors.Is(err, store.ErrVectorIndexBuildInProgress) || errors.Is(err, store.ErrVectorIndexBuildFenced) ||
				errors.Is(err, store.ErrVectorIndexSourceStale) {
				// Another request can publish embeddings while this index builds.
				if err := waitRenditionWorker(ctx, 100*time.Millisecond); err != nil {
					return nil, err
				}
				continue
			}
			if err != nil && !errors.Is(err, store.ErrNotFound) {
				return nil, err
			}
			break
		}
	}
	return jobIDs, nil
}

func (service *Service) chunkEmbeddingGeneration(ctx context.Context, version store.ContentVersion,
	profile configuredProfile, binding document.EmbeddingBindingV1,
) (store.EmbeddingInputGenerationRecord, error) {
	view, err := service.catalog.ActiveRendition(ctx, version.ID, profile.record.Fingerprint)
	if err != nil {
		return store.EmbeddingInputGenerationRecord{}, err
	}
	var artifact store.RenditionArtifactRecord
	for _, candidate := range view.Build.Artifacts {
		if candidate.Role == "normalized_evidence" {
			artifact = candidate
			break
		}
	}
	if artifact.ID == "" || artifact.Size < 2 || artifact.Size > 64<<20 {
		return store.EmbeddingInputGenerationRecord{}, errors.New("normalized evidence artifact is unavailable or exceeds bounds")
	}
	reader, size, err := service.blobs.OpenStreamContext(ctx, artifact.BlobHash)
	if err != nil {
		return store.EmbeddingInputGenerationRecord{}, err
	}
	data, readErr := io.ReadAll(io.LimitReader(reader, artifact.Size+1))
	verifyErr := reader.Verify()
	closeErr := reader.Close()
	if readErr != nil || verifyErr != nil || closeErr != nil || size != artifact.Size || int64(len(data)) != artifact.Size {
		return store.EmbeddingInputGenerationRecord{}, errors.Join(
			errors.New("normalized evidence could not be read exactly"), readErr, verifyErr, closeErr)
	}
	var evidence document.NormalizedEvidenceV1
	if err := json.Unmarshal(data, &evidence, json.RejectUnknownMembers(true)); err != nil {
		return store.EmbeddingInputGenerationRecord{}, fmt.Errorf("decoding normalized evidence: %w", err)
	}
	evidence.Checksum = artifact.Checksum
	canonical, checksum, err := document.MarshalNormalizedEvidenceV1(evidence)
	if err != nil || checksum != artifact.Checksum || !bytes.Equal(canonical, data) {
		return store.EmbeddingInputGenerationRecord{}, errors.New("normalized evidence is not exact canonical authority")
	}
	tokenizer := profile.tokenizers[binding.Name]
	policy, err := document.NewInputPolicy(binding, tokenizer, profile.record.EvidenceLexicalFingerprint, nil)
	if err != nil {
		return store.EmbeddingInputGenerationRecord{}, err
	}
	generated, err := document.BuildEmbeddingInputs(evidence, policy, document.GenerationLimits{
		MaxInputs:             100_000,
		MaxTotalContentTokens: 10_000_000, MaxTotalRenderedTokens: 10_000_000,
		MaxTotalContentBytes: 1 << 30, MaxTotalRenderedBytes: 1 << 30,
		MaxFittingWorkTokens: 100_000_000, MaxFittingWorkBytes: 16 << 30,
	})
	if err != nil {
		return store.EmbeddingInputGenerationRecord{}, err
	}
	encoded, err := document.MarshalEmbeddingInputGeneration(generated)
	if err != nil {
		return store.EmbeddingInputGenerationRecord{}, err
	}
	var receipt blob.WriteReceipt
	err = service.gate.MutateContext(ctx, func() error {
		return service.blobs.WithMutation(ctx, func() error {
			var writeErr error
			receipt, writeErr = service.blobs.WriteDetailedContext(ctx, bytes.NewReader(encoded))
			if writeErr != nil {
				return writeErr
			}
			encoding, encodingErr := receipt.EncodingName()
			if encodingErr != nil {
				return encodingErr
			}
			return service.catalog.RecordRenditionBlob(ctx, receipt.Hash, receipt.Size, store.BlobPhysical{
				Encoding: encoding, StoredBytes: receipt.StoredSize,
				PackEligible: receipt.PackEligible, Created: receipt.Created,
			})
		})
	})
	if err != nil {
		return store.EmbeddingInputGenerationRecord{}, err
	}
	inputs := make([]store.EmbeddingInputReference, len(generated.Inputs))
	for index, input := range generated.Inputs {
		inputs[index] = store.EmbeddingInputReference{ID: input.Key, RenderedChecksum: input.Checksum}
	}
	return store.EmbeddingInputGenerationRecord{
		ID:              sha256Text("embedding-generation-attachment/v1\x00" + generated.Checksum + "\x00" + view.Attachment.ID),
		SourceVersionID: version.ID, ProcessingProfileFingerprint: profile.record.Fingerprint,
		GenerationJSON: encoded, EvidenceJSON: data, GenerationBlobHash: receipt.Hash, GenerationEncodedSize: receipt.Size,
		GenerationChecksum: generated.Checksum, EvidenceFingerprint: generated.EvidenceChecksum,
		TokenizerFingerprint:   sha256Text(binding.Chunk.Tokenizer + "\x00" + binding.Chunk.TokenizerRevision),
		ChunkPolicyFingerprint: generated.PolicyFingerprint, FormatterFingerprint: sha256Text(binding.Chunk.Formatter),
		AttachmentID: view.Attachment.ID, Inputs: inputs, CreatedAt: view.Attachment.AttachedAt,
	}, nil
}

func directEmbeddingGeneration(version store.ContentVersion, profileFingerprint,
	embeddingInputFingerprint string, binding document.EmbeddingBindingV1,
) store.EmbeddingInputGenerationRecord {
	id := stableHash("docbank/direct-file-input-generation/v1", version.ID, version.BlobHash,
		profileFingerprint, binding.Name, embeddingInputFingerprint)
	return store.EmbeddingInputGenerationRecord{ID: id, SourceVersionID: version.ID,
		ProcessingProfileFingerprint: profileFingerprint,
		EvidenceFingerprint:          stableHash("docbank/direct-file-evidence/v1", version.BlobHash),
		TokenizerFingerprint:         stableHash("docbank/direct-file-tokenizer/v1", "none"),
		ChunkPolicyFingerprint:       embeddingInputFingerprint,
		FormatterFingerprint:         stableHash("docbank/direct-file-formatter/v1", binding.DocumentFormatter, binding.ModelInput.Fingerprint),
		GenerationChecksum:           version.BlobHash,
		Inputs:                       []store.EmbeddingInputReference{{ID: version.ID, RenderedChecksum: version.BlobHash}},
		CreatedAt:                    version.RecordedAt}
}

func stableHash(values ...string) string {
	hasher := sha256.New()
	for _, value := range values {
		_, _ = fmt.Fprintf(hasher, "%d:", len(value))
		_, _ = hasher.Write([]byte(value))
	}
	return hex.EncodeToString(hasher.Sum(nil))
}

func sha256Text(value string) string {
	digest := sha256.Sum256([]byte(value))
	return hex.EncodeToString(digest[:])
}

func selectEmbeddingBinding(profile document.ProcessingProfileV1,
	name string,
) (document.EmbeddingBindingV1, error) {
	if name == "" {
		if len(profile.Embeddings) == 0 {
			return document.EmbeddingBindingV1{}, store.ErrNotFound
		}
		return profile.Embeddings[0], nil
	}
	for _, binding := range profile.Embeddings {
		if binding.Name == name {
			return binding, nil
		}
	}
	return document.EmbeddingBindingV1{}, fmt.Errorf("embedding binding %q: %w", name, store.ErrNotFound)
}

func (service *Service) profileFence(name string, fence SourceFence) (configuredProfile, []string, error) {
	profile, ok := service.profiles[name]
	if !ok {
		return configuredProfile{}, nil, ErrProfileNotConfigured
	}
	if fence.VaultUID != service.catalog.VaultID() {
		return configuredProfile{}, nil, ErrForeignVault
	}
	ids, err := normalizeFenceIDs(fence.ContentVersionIDs)
	return profile, ids, err
}

func (service *Service) resolve(ctx context.Context, selector Selector) (store.Node, store.ContentVersion, configuredProfile, error) {
	profile, ok := service.profiles[selector.Profile]
	if !ok {
		return store.Node{}, store.ContentVersion{}, configuredProfile{}, ErrProfileNotConfigured
	}
	if selector.NodeID <= 0 || selector.ContentVersionID == "" {
		return store.Node{}, store.ContentVersion{}, configuredProfile{}, errors.New("processing selector is incomplete")
	}
	node, err := service.catalog.NodeByID(ctx, selector.NodeID)
	if err != nil {
		return store.Node{}, store.ContentVersion{}, configuredProfile{}, err
	}
	version, err := service.catalog.ContentVersionByID(ctx, selector.ContentVersionID)
	if err != nil {
		return store.Node{}, store.ContentVersion{}, configuredProfile{}, err
	}
	if version.NodeID != node.ID || node.CurrentVersionID != version.ID || node.TrashedAt != nil {
		return store.Node{}, store.ContentVersion{}, configuredProfile{}, store.ErrNotFound
	}
	return node, version, profile, nil
}

type preparedRendition struct {
	identity       document.RenditionExecutionIdentityV1
	capturedPolicy jsontext.Value
}

func (service *Service) prepareRendition(ctx context.Context, node store.Node, version store.ContentVersion,
	profile configuredProfile, withUpload bool,
) (preparedRendition, error) {
	execution, err := prepareProviderExecution(ctx, service.blobs, service.spoolDirectory,
		node, version, profile, service.clock(), withUpload)
	if err != nil {
		return preparedRendition{}, err
	}
	if execution.Upload != nil {
		defer func() { _ = execution.Upload.Close() }()
	}
	identity, err := document.NewRenditionExecutionIdentityV1(execution.metadata,
		execution.Authorization, execution.EvidencePolicy, execution.RenditionPolicy)
	if err != nil {
		return preparedRendition{}, err
	}
	policy, err := capturedPolicy(profile.portable)
	return preparedRendition{identity: identity, capturedPolicy: policy}, err
}

type providerRenditionRuntime struct {
	provider       document.RenditionProvider
	blobs          *blob.Store
	spoolDirectory string
	clock          func() time.Time
}

func (runtime *providerRenditionRuntime) Prepare(ctx context.Context, work store.RenditionJobWork,
	now time.Time,
) (RenditionExecution, error) {
	var portable document.ProcessingProfileV1
	if err := json.Unmarshal(work.Profile.CanonicalProfile, &portable, json.RejectUnknownMembers(true)); err != nil {
		return RenditionExecution{}, err
	}
	version := runtimeVersion(work)
	node := store.Node{ID: version.NodeID, Name: work.ExecutionIdentity.Upload.Filename,
		CurrentVersionID: version.ID, BlobHash: version.BlobHash, Size: version.Size, MimeType: version.MimeType}
	profile := configuredProfile{portable: portable, provider: runtime.provider, record: work.Profile}
	prepared, err := prepareProviderExecution(ctx, runtime.blobs, runtime.spoolDirectory, node, version,
		profile, now, true)
	if err != nil {
		return RenditionExecution{}, err
	}
	return RenditionExecution{Provider: runtime.provider, Upload: prepared.Upload,
		Authorization: prepared.Authorization, EvidencePolicy: prepared.EvidencePolicy,
		RenditionPolicy: prepared.RenditionPolicy}, nil
}

func (runtime *providerRenditionRuntime) ResumeProvider(_ context.Context, _ store.RenditionJobWork,
	_ document.RenditionExecutionSnapshotV1,
) (document.RenditionProvider, error) {
	if _, ok := runtime.provider.(document.ResumableRenditionProvider); !ok {
		return nil, ErrRenditionRuntimeUnavailable
	}
	return runtime.provider, nil
}

type providerExecution struct {
	RenditionExecution

	metadata document.AuthorizedUploadMetadata
}

func prepareProviderExecution(ctx context.Context, blobs *blob.Store, spool string,
	node store.Node, version store.ContentVersion, profile configuredProfile, now time.Time, withUpload bool,
) (providerExecution, error) {
	if profile.portable.Rendition == nil {
		return providerExecution{}, errors.New("processing profile has no rendition binding")
	}
	filename := syntheticFilename(node.Name, version.MimeType, profile.portable.Rendition.DiscloseFilename)
	policy := inspectionPolicy(filename, version, profile)
	reader, err := blobs.OpenContext(ctx, version.BlobHash)
	if err != nil {
		return providerExecution{}, err
	}
	capability, inspectErr := media.InspectCapability(reader, policy)
	closeErr := reader.Close()
	if inspectErr != nil || closeErr != nil {
		return providerExecution{}, errors.Join(inspectErr, closeErr)
	}
	if !capability.Eligible {
		return providerExecution{}, fmt.Errorf("source is not eligible for rendition: %s", capability.Reason)
	}
	emptyDigest := sha256.Sum256(nil)
	metadata := document.AuthorizedUploadMetadata{Filename: filename,
		MediaFamily: capability.MediaFamily, MediaType: capability.MediaType,
		ByteLength: version.Size, SHA256: version.BlobHash,
		CapabilityRecordChecksum: capability.Checksum,
		ProviderMetadataChecksum: hex.EncodeToString(emptyDigest[:]),
		InputKind:                document.RenditionInputOriginalFile}
	authorizedAt := now.UTC()
	maximum := int(min(profile.portable.Rendition.MaxResponseBytes, int64(math.MaxInt)))
	markdownMaximum, artifactMaximum := 0, 0
	var artifactRoles []document.EvidenceArtifactRole
	for _, role := range profile.portable.Rendition.RequestedArtifacts {
		if role == document.EvidenceArtifactMarkdown {
			markdownMaximum = maximum
		} else {
			artifactRoles = append(artifactRoles, role)
			artifactMaximum = maximum
		}
	}
	authorization := document.RenditionAuthorization{ProviderID: profile.provider.Descriptor().ID,
		DescriptorFingerprint:       profile.provider.Descriptor().Fingerprint,
		PolicyFingerprint:           profile.provider.Descriptor().PolicyFingerprint,
		RenditionRequestFingerprint: profile.record.RenditionRequestFingerprint,
		SourceSHA256:                version.BlobHash, SourceBytes: version.Size,
		CapabilityRecordChecksum: capability.Checksum, ProviderMetadataChecksum: metadata.ProviderMetadataChecksum,
		MediaFamily: capability.MediaFamily, MediaType: capability.MediaType,
		InputKind:                document.RenditionInputOriginalFile,
		DiscloseFilename:         profile.portable.Rendition.DiscloseFilename,
		AllowedArtifactRoles:     artifactRoles,
		MaxProviderMarkdownBytes: markdownMaximum, MaxArtifactBytes: artifactMaximum,
		MaxArtifacts:        len(artifactRoles),
		MaxTotalResultBytes: maximum, AuthorizedAt: authorizedAt.Format(timestampForm),
		ExpiresAt: authorizedAt.Add(10 * time.Minute).Format(timestampForm)}
	evidence, rendition, err := document.RenditionExecutionPoliciesForProfileV1(profile.portable)
	if err != nil {
		return providerExecution{}, err
	}
	result := providerExecution{Provider: profile.provider,
		Authorization: authorization, EvidencePolicy: evidence, RenditionPolicy: rendition, metadata: metadata}
	if withUpload {
		source, err := blobs.OpenContext(ctx, version.BlobHash)
		if err != nil {
			return providerExecution{}, err
		}
		result.Upload, err = upload.Authorize(ctx, upload.Source{Reader: source, Directory: spool},
			capability, upload.UploadMetadata{Filename: filename})
		if err != nil {
			return providerExecution{}, err
		}
		result.metadata = result.Upload.Metadata()
	}
	return result, nil
}

func runtimeVersion(work store.RenditionJobWork) store.ContentVersion {
	return store.ContentVersion{ID: work.Waiter.ContentVersionID, NodeID: 1,
		BlobHash: work.Job.SourceSHA256, Size: work.ExecutionIdentity.Upload.ByteLength,
		MimeType: work.ExecutionIdentity.Upload.MediaType}
}

func inspectionPolicy(filename string, version store.ContentVersion, profile configuredProfile) media.InspectionPolicy {
	maximum := min(profile.portable.Rendition.MaxDocumentBytes, int64(1<<30))
	return media.InspectionPolicy{Filename: filename, DeclaredMediaType: version.MimeType,
		ExpectedBytes: version.Size, ExpectedSHA256: version.BlobHash,
		DescriptorFingerprint: profile.provider.Descriptor().Fingerprint,
		ProfileFingerprint:    profile.record.Fingerprint,
		DisclosureFingerprint: profile.portable.Rendition.DisclosureFingerprint,
		InputKind:             document.RenditionInputOriginalFile, MaxSourceBytes: maximum,
		MaxExpandedBytes: maximum, MaxEntryBytes: maximum, MaxEntries: 100_000,
		MaxNestingDepth: 1, MaxTextLines: 10_000_000, MaxCharacters: 1_000_000_000,
		MaxRecords: 10_000_000, MaxPages: 1_000_000, MaxSlides: 1_000_000,
		MaxSheets: 1_000_000, MaxCells: 100_000_000, MaxSpineItems: 1_000_000,
		MaxResources: 1_000_000, MaxPixels: 1_000_000_000, MaxFrames: 1_000_000,
		MaxDurationMS: 24 * 60 * 60 * 1000}
}

func capturedPolicy(profile document.ProcessingProfileV1) (jsontext.Value, error) {
	type role struct {
		MaxCount int    `json:"max_count"`
		MinCount int    `json:"min_count"`
		Role     string `json:"role"`
	}
	roles := []role{{MaxCount: 1, MinCount: 1, Role: "normalized_evidence"}}
	if profile.RetentionDisclosure.RetainSanitizedMarkdown {
		roles = append(roles, role{MaxCount: 1, MinCount: 1, Role: "sanitized_markdown"})
	}
	if profile.RetentionDisclosure.RetainProviderMarkdown {
		roles = append(roles, role{MaxCount: 1, Role: string(document.EvidenceArtifactMarkdown)})
	}
	if profile.RetentionDisclosure.RetainTypedArtifacts {
		for _, artifact := range profile.Rendition.RequestedArtifacts {
			if artifact != document.EvidenceArtifactMarkdown {
				roles = append(roles, role{MaxCount: 1, Role: string(artifact)})
			}
		}
	}
	sort.Slice(roles, func(i, j int) bool { return roles[i].Role < roles[j].Role })
	encoded, err := json.Marshal(struct {
		Roles   []role `json:"roles"`
		Version int    `json:"version"`
	}{Roles: roles, Version: 1}, json.Deterministic(true))
	return jsontext.Value(encoded), err
}

func retainedRenditionClasses(profile document.ProcessingProfileV1) []string {
	classes := []string{"normalized_evidence"}
	if profile.RetentionDisclosure.RetainSanitizedMarkdown {
		classes = append(classes, "sanitized_markdown")
	}
	if profile.RetentionDisclosure.RetainProviderMarkdown {
		classes = append(classes, string(document.EvidenceArtifactMarkdown))
	}
	if profile.RetentionDisclosure.RetainTypedArtifacts {
		for _, role := range profile.Rendition.RequestedArtifacts {
			if role != document.EvidenceArtifactMarkdown {
				classes = append(classes, string(role))
			}
		}
	}
	return sortedUnique(classes)
}

func normalizeFenceIDs(ids []string) ([]string, error) {
	if len(ids) < 1 || len(ids) > MaxSourceFenceIDs {
		return nil, fmt.Errorf("source fence must contain between 1 and %d content versions", MaxSourceFenceIDs)
	}
	result := slices.Clone(ids)
	sort.Strings(result)
	for index, id := range result {
		parsed, err := uuid.Parse(id)
		if err != nil || (parsed[6]>>4 != 4 || parsed[8]>>6 != 2) || parsed.String() != id {
			return nil, errors.New("source fence contains an invalid content version ID")
		}
		if index > 0 && result[index-1] == id {
			return nil, errors.New("source fence contains a duplicate content version ID")
		}
	}
	return result, nil
}

func planFingerprint(plan Plan) (string, error) {
	plan.Fingerprint = ""
	plan.ConsentRequired = true // Grant availability is not part of the disclosure contract.
	encoded, err := json.Marshal(plan, json.Deterministic(true))
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:]), nil
}

// sameProvider reports whether two profiles supplied the identical provider
// value. Runtimes are keyed by descriptor fingerprint, which excludes
// endpoints and credentials, so a second distinct provider with the same
// descriptor would silently execute through the first one.
func sameProvider(existing, candidate any) bool {
	if !reflect.ValueOf(existing).Comparable() || !reflect.ValueOf(candidate).Comparable() {
		return false
	}
	return existing == candidate
}

func validateProfileName(name string) error {
	if name == "" || len(name) > 128 || strings.TrimSpace(name) != name {
		return errors.New("processing profile name is invalid")
	}
	return nil
}

func sortedUnique(values []string) []string {
	sort.Strings(values)
	return slices.Compact(values)
}

func syntheticFilename(original, mediaType string, disclose bool) string {
	if disclose && filepath.Base(original) == original && original != "." && original != "" {
		return original
	}
	return "document" + extensionForMediaType(mediaType)
}

func extensionForMediaType(value string) string {
	base, _, _ := mime.ParseMediaType(value)
	// Use the spellings accepted by capability inspection, independent of the
	// host MIME database's aliases. Sealed identities withhold the source name.
	if extension := map[string]string{
		"text/plain": ".txt", "text/markdown": ".md", "text/x-rst": ".rst",
		"application/x-tex": ".tex", "application/x-ndjson": ".jsonl",
		"application/xml": ".xml", "application/yaml": ".yaml",
		"text/x-go": ".go", "text/x-python": ".py", "text/javascript": ".js",
		"image/jpeg": ".jpg", "audio/wav": ".wav", "audio/x-wav": ".wav",
		"audio/mpeg": ".mp3", "message/rfc822": ".eml",
	}[base]; extension != "" {
		return extension
	}
	extensions, _ := mime.ExtensionsByType(base)
	if len(extensions) == 0 {
		return ".bin"
	}
	sort.Strings(extensions)
	return extensions[0]
}

func sourceFormat(filename, family, mediaType string) string {
	if filename == "" {
		filename = syntheticFilename("", mediaType, false)
	}
	if extension := strings.TrimPrefix(strings.ToLower(filepath.Ext(filename)), "."); extension != "" {
		return extension
	}
	base, _, _ := mime.ParseMediaType(mediaType)
	if _, subtype, found := strings.Cut(base, "/"); found && subtype != "" {
		return subtype
	}
	return family
}

func classifyEmbeddingProviderError(err error) (EmbeddingProviderFailure, time.Duration) {
	if err == nil {
		return EmbeddingProviderPermanent, 0
	}
	return EmbeddingProviderTransient, 0
}

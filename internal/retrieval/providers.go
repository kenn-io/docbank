package retrieval

import (
	"context"
	"errors"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"go.kenn.io/docbank/internal/store"
)

const (
	maxQueryExpansionVariants      = 8
	maxRerankingExcerptBytes       = 4 << 10
	maxProviderQueryBytes          = 4 << 10
	maxRerankingEvidenceReferences = 32
	maxRerankingEvidenceBytes      = 32 << 10
)

var (
	// ErrQueryExpansionFailed is returned when a fail-closed expansion stage cannot complete.
	ErrQueryExpansionFailed = errors.New("query expansion stage failed")
	// ErrRerankingFailed is returned when a fail-closed reranking stage cannot complete.
	ErrRerankingFailed = errors.New("reranking stage failed")
)

type ProviderFailurePolicy string

const (
	ProviderFailureDegrade    ProviderFailurePolicy = "degrade"
	ProviderFailureFailClosed ProviderFailurePolicy = "fail_closed"
)

type ProviderStage string

const (
	ProviderStageExpansion ProviderStage = "query_expansion"
	ProviderStageReranking ProviderStage = "reranking"
)

type ProviderOutcome string

const (
	ProviderOutcomeApplied             ProviderOutcome = "applied"
	ProviderOutcomeAuthorizationDenied ProviderOutcome = "authorization_denied"
	ProviderOutcomeTimedOut            ProviderOutcome = "timed_out"
	ProviderOutcomeMalformed           ProviderOutcome = "malformed_output"
	ProviderOutcomeUnavailable         ProviderOutcome = "unavailable"
)

// ProviderReceipt records a provider-stage outcome without retaining any query,
// document, provider-response, or credential material.
type ProviderReceipt struct {
	Stage          ProviderStage
	Outcome        ProviderOutcome
	VariantCount   int
	CandidateCount int
}

// ExpansionProfile identifies the bounded expansion behavior independently of
// the provider, authorization, deadline, and failure policy.
type ExpansionProfile struct {
	ID          string
	MaxVariants int
}

// RerankingProfile identifies the bounded reranking behavior independently of
// the provider, authorization, deadline, and failure policy.
type RerankingProfile struct {
	ID            string
	MaxCandidates int
}

type ExpansionRequest struct {
	Query       string
	MaxVariants int
}

type RerankingRequest struct {
	Query      string
	Candidates []RerankingCandidate
}

// RerankingCandidate is a bounded, already-authorized retrieval result. It
// never carries a document body; providers receive only its stable identity,
// bounded lexical excerpt, and provenance references.
type RerankingCandidate struct {
	Document DocumentIdentity
	Excerpt  string
	Evidence []EvidenceReference
}

type ProviderInputClass string

const (
	ProviderInputQueryText       ProviderInputClass = "query_text"
	ProviderInputQueryAndExcerpt ProviderInputClass = "query_text_and_excerpt"
)

// ProviderOperation is immutable typed authorization metadata. It deliberately
// excludes query, excerpt, document, provider-response, and credential text.
type ProviderOperation struct {
	Stage                          ProviderStage
	ProfileID                      string
	Scope                          store.SearchOptions
	InputClass                     ProviderInputClass
	VariantLimit                   int
	CandidateCount                 int
	QueryBytes                     int
	QueryByteLimit                 int
	ExcerptBytes                   int
	ExcerptBytesPerCandidateLimit  int
	ExcerptBytesTotalLimit         int
	EvidenceCount                  int
	EvidencePerCandidateLimit      int
	EvidenceTotalLimit             int
	EvidenceBytes                  int
	EvidenceBytesPerCandidateLimit int
	EvidenceBytesTotalLimit        int
}

type QueryExpansionProvider interface {
	Expand(ctx context.Context, request ExpansionRequest) ([]string, error)
}

type RerankingProvider interface {
	Rerank(ctx context.Context, request RerankingRequest) ([]RerankScore, error)
}

// ExpansionAuthorizer is the separate consent/authorization boundary for one
// expansion operation. Implementations must authorize before provider egress.
type ExpansionAuthorizer interface {
	AuthorizeExpansion(ctx context.Context, operation ProviderOperation) error
}

// RerankingAuthorizer is the separate consent/authorization boundary for one
// reranking operation. Implementations must authorize before provider egress.
type RerankingAuthorizer interface {
	AuthorizeReranking(ctx context.Context, operation ProviderOperation) error
}

type ExpansionConfig struct {
	Enabled       bool
	Profile       ExpansionProfile
	Provider      QueryExpansionProvider
	Authorizer    ExpansionAuthorizer
	Deadline      time.Duration
	FailurePolicy ProviderFailurePolicy
}

type RerankingConfig struct {
	Enabled       bool
	Profile       RerankingProfile
	Provider      RerankingProvider
	Authorizer    RerankingAuthorizer
	Deadline      time.Duration
	FailurePolicy ProviderFailurePolicy
}

// RerankScore is one provider score for an already-authorized document.
type RerankScore struct {
	Document DocumentIdentity
	Score    float64
}

func validateExpansionConfig(config ExpansionConfig) error {
	if !config.Enabled {
		return nil
	}
	if config.Profile.ID == "" || config.Profile.MaxVariants < 1 ||
		config.Profile.MaxVariants > maxQueryExpansionVariants || config.Provider == nil ||
		config.Authorizer == nil || config.Deadline <= 0 || !validProviderFailurePolicy(config.FailurePolicy) {
		return errors.New("query expansion configuration is invalid")
	}
	return nil
}

func validateRerankingConfig(config RerankingConfig) error {
	if !config.Enabled {
		return nil
	}
	if config.Profile.ID == "" || config.Profile.MaxCandidates < 1 ||
		config.Profile.MaxCandidates > MaxCandidateLimit || config.Provider == nil ||
		config.Authorizer == nil || config.Deadline <= 0 || !validProviderFailurePolicy(config.FailurePolicy) {
		return errors.New("reranking configuration is invalid")
	}
	return nil
}

func validProviderFailurePolicy(policy ProviderFailurePolicy) bool {
	return policy == ProviderFailureDegrade || policy == ProviderFailureFailClosed
}

func (searcher *Searcher) expand(ctx context.Context, query Query) ([]string, *ProviderReceipt, Degradation, error) {
	config := searcher.expansion
	if !config.Enabled {
		return nil, nil, DegradationNone, nil
	}
	stageCtx, cancel := context.WithTimeout(ctx, config.Deadline)
	defer cancel()
	if len(query.Text) > maxProviderQueryBytes {
		return searcher.expandFailure(config, ProviderOutcomeMalformed)
	}
	operation := ProviderOperation{Stage: ProviderStageExpansion, ProfileID: config.Profile.ID,
		Scope: query.Scope, InputClass: ProviderInputQueryText, VariantLimit: config.Profile.MaxVariants,
		QueryBytes: len(query.Text), QueryByteLimit: maxProviderQueryBytes}
	if err := config.Authorizer.AuthorizeExpansion(stageCtx, operation); err != nil {
		if ctx.Err() != nil {
			return nil, nil, DegradationNone, ctx.Err()
		}
		return searcher.expandFailure(config, ProviderOutcomeAuthorizationDenied)
	}
	if err := stageCtx.Err(); err != nil {
		if ctx.Err() != nil {
			return nil, nil, DegradationNone, ctx.Err()
		}
		return searcher.expandFailure(config, stageOutcome(stageCtx))
	}
	variants, err := config.Provider.Expand(stageCtx, ExpansionRequest{Query: query.Text,
		MaxVariants: config.Profile.MaxVariants})
	if err != nil {
		if ctx.Err() != nil {
			return nil, nil, DegradationNone, ctx.Err()
		}
		return searcher.expandFailure(config, stageOutcome(stageCtx))
	}
	if err := stageCtx.Err(); err != nil {
		if ctx.Err() != nil {
			return nil, nil, DegradationNone, ctx.Err()
		}
		return searcher.expandFailure(config, stageOutcome(stageCtx))
	}
	variants, err = validateExpansionVariants(query.Text, variants, config.Profile.MaxVariants)
	if err != nil {
		return searcher.expandFailure(config, ProviderOutcomeMalformed)
	}
	return variants, &ProviderReceipt{Stage: ProviderStageExpansion,
		Outcome: ProviderOutcomeApplied, VariantCount: len(variants)}, DegradationNone, nil
}

func (searcher *Searcher) expandFailure(config ExpansionConfig, outcome ProviderOutcome) ([]string, *ProviderReceipt, Degradation, error) {
	if config.FailurePolicy == ProviderFailureFailClosed {
		return nil, nil, DegradationNone, ErrQueryExpansionFailed
	}
	return nil, &ProviderReceipt{Stage: ProviderStageExpansion,
		Outcome: outcome}, DegradationExpansionDegraded, nil
}

func (searcher *Searcher) rerank(ctx context.Context, query Query, report Report) (Report, *ProviderReceipt, Degradation, error) {
	config := searcher.reranking
	if !config.Enabled || len(report.Results) == 0 {
		return report, nil, DegradationNone, nil
	}
	candidates := rerankingCandidates(report.Results, config.Profile.MaxCandidates)
	allowed := make([]DocumentIdentity, len(candidates))
	for index, candidate := range candidates {
		allowed[index] = candidate.Document
	}
	stageCtx, cancel := context.WithTimeout(ctx, config.Deadline)
	defer cancel()
	if len(query.Text) > maxProviderQueryBytes {
		return searcher.rerankFailure(config, report, ProviderOutcomeMalformed, len(candidates))
	}
	excerptBytes, evidenceCount, evidenceBytes := rerankingPayloadBounds(candidates)
	operation := ProviderOperation{Stage: ProviderStageReranking, ProfileID: config.Profile.ID,
		Scope: query.Scope, InputClass: ProviderInputQueryAndExcerpt, CandidateCount: len(candidates),
		QueryBytes: len(query.Text), QueryByteLimit: maxProviderQueryBytes,
		ExcerptBytes: excerptBytes, ExcerptBytesPerCandidateLimit: maxRerankingExcerptBytes,
		ExcerptBytesTotalLimit: boundedProviderTotal(maxRerankingExcerptBytes, len(candidates)),
		EvidenceCount:          evidenceCount, EvidencePerCandidateLimit: maxRerankingEvidenceReferences,
		EvidenceTotalLimit: boundedProviderTotal(maxRerankingEvidenceReferences, len(candidates)),
		EvidenceBytes:      evidenceBytes, EvidenceBytesPerCandidateLimit: maxRerankingEvidenceBytes,
		EvidenceBytesTotalLimit: boundedProviderTotal(maxRerankingEvidenceBytes, len(candidates))}
	if err := config.Authorizer.AuthorizeReranking(stageCtx, operation); err != nil {
		if ctx.Err() != nil {
			return Report{}, nil, DegradationNone, ctx.Err()
		}
		return searcher.rerankFailure(config, report, ProviderOutcomeAuthorizationDenied, len(candidates))
	}
	if err := stageCtx.Err(); err != nil {
		if ctx.Err() != nil {
			return Report{}, nil, DegradationNone, ctx.Err()
		}
		return searcher.rerankFailure(config, report, stageOutcome(stageCtx), len(candidates))
	}
	scores, err := config.Provider.Rerank(stageCtx, RerankingRequest{Query: query.Text,
		Candidates: slices.Clone(candidates)})
	if err != nil {
		if ctx.Err() != nil {
			return Report{}, nil, DegradationNone, ctx.Err()
		}
		return searcher.rerankFailure(config, report, stageOutcome(stageCtx), len(candidates))
	}
	if err := stageCtx.Err(); err != nil {
		if ctx.Err() != nil {
			return Report{}, nil, DegradationNone, ctx.Err()
		}
		return searcher.rerankFailure(config, report, stageOutcome(stageCtx), len(candidates))
	}
	if err := validateRerankScores(allowed, scores); err != nil {
		return searcher.rerankFailure(config, report, ProviderOutcomeMalformed, len(candidates))
	}
	byDocument := make(map[DocumentIdentity]float64, len(scores))
	for _, score := range scores {
		byDocument[score.Document] = score.Score
	}
	for index := range report.Results[:len(candidates)] {
		report.Results[index].Score = byDocument[report.Results[index].Document]
	}
	slices.SortFunc(report.Results[:len(candidates)], compareResults)
	for index := range report.Results {
		report.Results[index].Rank = index + 1
	}
	report.Trace = append(report.Trace, TraceEvent{Code: TraceRerankedCandidates, Count: len(candidates)})
	return report, &ProviderReceipt{Stage: ProviderStageReranking,
		Outcome: ProviderOutcomeApplied, CandidateCount: len(candidates)}, DegradationNone, nil
}

func rerankingCandidates(results []Result, limit int) []RerankingCandidate {
	count := min(len(results), limit)
	candidates := make([]RerankingCandidate, count)
	for index, result := range results[:count] {
		evidence := boundedRerankingEvidence(result.Evidence)
		candidates[index] = RerankingCandidate{Document: result.Document,
			Excerpt: boundedRerankingExcerpt(result.Excerpt), Evidence: evidence}
	}
	return candidates
}

func rerankingPayloadBounds(candidates []RerankingCandidate) (int, int, int) {
	var excerpts, evidence, evidenceBytes int
	for _, candidate := range candidates {
		excerpts += len(candidate.Excerpt)
		evidence += len(candidate.Evidence)
		for _, reference := range candidate.Evidence {
			evidenceBytes += rerankingEvidenceBytes(reference)
		}
	}
	return excerpts, evidence, evidenceBytes
}

func boundedRerankingEvidence(references []EvidenceReference) []EvidenceReference {
	bounded := make([]EvidenceReference, 0, min(len(references), maxRerankingEvidenceReferences))
	bytes := 0
	for _, reference := range references {
		size := rerankingEvidenceBytes(reference)
		if len(bounded) == maxRerankingEvidenceReferences || size > maxRerankingEvidenceBytes-bytes {
			break
		}
		bounded = append(bounded, reference)
		bytes += size
	}
	return bounded
}

func rerankingEvidenceBytes(reference EvidenceReference) int {
	return len(reference.Kind) + len(reference.VaultID) + len(reference.ContentVersionID) +
		len(strconv.FormatInt(reference.NodeRevision, 10)) +
		len(reference.VectorSpaceID) + len(reference.EmbeddingSetID) + len(reference.InputGenerationID) +
		len(reference.InputID) + len(reference.InputKind) + len(reference.BuildID) + len(reference.SegmentID) +
		len(reference.BlobHash) + len(reference.SourceManifestChecksum)
}

func boundedProviderTotal(perCandidate, candidateCount int) int {
	if perCandidate <= 0 || candidateCount <= 0 {
		return 0
	}
	if candidateCount > math.MaxInt/perCandidate {
		return math.MaxInt
	}
	return perCandidate * candidateCount
}

func boundedRerankingExcerpt(excerpt string) string {
	excerpt = strings.ToValidUTF8(excerpt, "")
	if len(excerpt) <= maxRerankingExcerptBytes {
		return excerpt
	}
	end := maxRerankingExcerptBytes
	for end > 0 && !utf8.RuneStart(excerpt[end]) {
		end--
	}
	return excerpt[:end]
}

func (searcher *Searcher) rerankFailure(config RerankingConfig, report Report, outcome ProviderOutcome,
	candidateCount int,
) (Report, *ProviderReceipt, Degradation, error) {
	if config.FailurePolicy == ProviderFailureFailClosed {
		return Report{}, nil, DegradationNone, ErrRerankingFailed
	}
	return report, &ProviderReceipt{Stage: ProviderStageReranking,
		Outcome: outcome, CandidateCount: candidateCount}, DegradationRerankingDegraded, nil
}

func stageOutcome(ctx context.Context) ProviderOutcome {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return ProviderOutcomeTimedOut
	}
	return ProviderOutcomeUnavailable
}

func validateExpansionVariants(query string, variants []string, limit int) ([]string, error) {
	if limit < 1 || len(variants) > limit {
		return nil, errors.New("query expansion provider returned an invalid variant count")
	}
	seen := make(map[string]struct{}, len(variants))
	for _, variant := range variants {
		variant = strings.TrimSpace(variant)
		if variant == "" || variant == query || len(variant) > maxProviderQueryBytes || !utf8.ValidString(variant) {
			return nil, errors.New("query expansion provider returned an invalid variant")
		}
		if _, duplicate := seen[variant]; duplicate {
			return nil, errors.New("query expansion provider returned duplicate variants")
		}
		seen[variant] = struct{}{}
	}
	result := make([]string, 0, len(seen))
	for variant := range seen {
		result = append(result, variant)
	}
	slices.Sort(result)
	return result, nil
}

func validateRerankScores(allowed []DocumentIdentity, scores []RerankScore) error {
	if len(scores) != len(allowed) {
		return errors.New("reranking provider returned an invalid score count")
	}
	expected := make(map[DocumentIdentity]struct{}, len(allowed))
	for _, document := range allowed {
		expected[document] = struct{}{}
	}
	seen := make(map[DocumentIdentity]struct{}, len(scores))
	for _, score := range scores {
		if _, allowed := expected[score.Document]; !allowed {
			return errors.New("reranking provider returned an unknown document score")
		}
		if _, duplicate := seen[score.Document]; duplicate {
			return errors.New("reranking provider returned duplicate document scores")
		}
		if math.IsNaN(score.Score) || math.IsInf(score.Score, 0) {
			return errors.New("reranking provider returned a non-finite score")
		}
		seen[score.Document] = struct{}{}
	}
	return nil
}

package retrieval

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"time"

	"go.kenn.io/docbank/document"
	embeddingeval "go.kenn.io/docbank/document/embedding/eval"
	"go.kenn.io/docbank/internal/store"
	"go.kenn.io/docbank/internal/vectorindex"
)

type passageEvaluationBackend interface {
	EvaluationPassageFocus(ctx context.Context, query string, texts []string) ([][2]int, error)
	SearchEvaluationLexicalPassages(ctx context.Context, query string, fences []store.EvaluationLexicalFence, maxRows int) ([]store.EvaluationLexicalPassage, bool, error)
	RevalidateEvaluationLexicalFences(ctx context.Context, fences []store.EvaluationLexicalFence) error
	ResolveEvaluationSemanticPassages(ctx context.Context, profile, binding string, inputKind document.EmbeddingInputKind, space, source string,
		neighbors []vectorindex.Neighbor, opts store.SearchOptions, selected map[int64]struct{}, perNode int) (store.SemanticSearchResolution, error)
}

// PassageEvaluation freezes an actual serving request for offline comparison.
// It retains private text and must never be serialized into tracked evidence.
type PassageEvaluation struct {
	Candidates []embeddingeval.PassageCandidate
	Report     Report
	searcher   *Searcher
	query      Query
	backend    *passageRecordingBackend
	catalog    passageEvaluationBackend
}

// PreparePassageEvaluation runs Search with a local recording reranker. It uses
// the serving excerpt, identity fences, fusion and candidate limit unchanged.
// Ordinary top-K truncation is expected; incomplete authority is an error.
func (searcher *Searcher) PreparePassageEvaluation(ctx context.Context, query Query) (*PassageEvaluation, error) {
	if searcher.expansion.Enabled {
		return nil, errors.New("passage evaluation requires unexpanded queries")
	}
	catalog, ok := searcher.backend.(passageEvaluationBackend)
	if !ok {
		return nil, errors.New("passage evaluation catalog is unavailable")
	}
	revalidation, ok := searcher.backend.(CandidateRevalidationBackend)
	if !ok {
		return nil, errors.New("passage evaluation revalidation is unavailable")
	}
	semantic, _ := searcher.backend.(SemanticBackend)
	if (query.Mode == ModeHybrid || query.Mode == ModeSemantic) && semantic == nil {
		return nil, errors.New("semantic evaluation is unavailable")
	}
	query, err := normalizeQuery(query)
	if err != nil {
		return nil, err
	}
	recorder := &passageRecordingBackend{Backend: searcher.backend, SemanticBackend: semantic, CandidateRevalidationBackend: revalidation}
	provider := &passageRecordingReranker{}
	copySearcher := *searcher
	copySearcher.backend = recorder
	copySearcher.reranking = RerankingConfig{Enabled: true, Profile: RerankingProfile{ID: "offline-capture", MaxCandidates: query.Limit, MaxExcerptBytes: 4096},
		Provider: provider, Authorizer: provider, Deadline: 5 * time.Second, FailurePolicy: ProviderFailureFailClosed}
	report, err := copySearcher.Search(ctx, query)
	if err != nil {
		return nil, err
	}
	if len(report.Degradations) != 0 || ((report.ActualMode == ModeHybrid || report.ActualMode == ModeSemantic) && report.Coverage.State != CoverageComplete) {
		return nil, errors.New("passage evaluation retrieval is incomplete")
	}
	for _, candidate := range recorder.resolved {
		if candidate.InputKind == document.EmbeddingInputRenditionChunk {
			if searcher.mediaEvidence == nil {
				return nil, errors.New("chunk evidence resolver is unavailable")
			}
			if _, err := searcher.mediaEvidence.resolveInput(ctx, candidate.MediaEvidence, candidate.InputID); err != nil {
				return nil, err
			}
		}
	}
	prepared := &PassageEvaluation{Report: report, searcher: &copySearcher, query: query, backend: recorder, catalog: catalog}
	ties := slices.Clone(report.Results)
	for i := range ties {
		ties[i].Score = 0
	}
	slices.SortFunc(ties, compareResults)
	for _, candidate := range provider.candidates {
		tie := slices.IndexFunc(ties, func(r Result) bool { return r.Document == candidate.Document })
		prepared.Candidates = append(prepared.Candidates, embeddingeval.PassageCandidate{DocumentID: candidate.Document.ContentVersionID, Excerpt: candidate.Excerpt, TieBreak: tie})
	}
	return prepared, nil
}

type passageRecordingReranker struct{ candidates []RerankingCandidate }

func (r *passageRecordingReranker) Rerank(_ context.Context, request RerankingRequest) ([]RerankScore, error) {
	r.candidates = slices.Clone(request.Candidates)
	var scores []RerankScore
	for i, candidate := range request.Candidates {
		scores = append(scores, RerankScore{Document: candidate.Document, Score: float64(len(request.Candidates) - i)})
	}
	return scores, nil
}
func (*passageRecordingReranker) AuthorizeReranking(context.Context, ProviderOperation) (ProviderEgressLease, error) {
	return passageRecordingLease{}, nil
}

type passageRecordingLease struct{}

func (passageRecordingLease) Close() {}

type passageRecordingBackend struct {
	Backend
	SemanticBackend
	CandidateRevalidationBackend

	neighbors     []vectorindex.Neighbor
	resolved      []store.SemanticSearchCandidate
	inputKind     document.EmbeddingInputKind
	space, source string
}

func (b *passageRecordingBackend) ResolveSemanticCandidates(ctx context.Context, profile, binding string, kind document.EmbeddingInputKind,
	space, source string, neighbors []vectorindex.Neighbor, limit int, opts store.SearchOptions, excluded map[int64]struct{},
) (store.SemanticSearchResolution, error) {
	if b.neighbors == nil {
		b.neighbors, b.inputKind, b.space, b.source = slices.Clone(neighbors), kind, space, source
	}
	result, err := b.SemanticBackend.ResolveSemanticCandidates(ctx, profile, binding, kind, space, source, neighbors, limit, opts, excluded)
	b.resolved = append(b.resolved, result.Candidates...)
	return result, err
}

type evaluationPassage struct {
	text, source, build, unit, segment string
	start, end                         int
	focus                              int
	evidence                           EvidenceReference
}

// Select is evaluator-owned policy, reusable as PassageOptions.Select. Each
// invocation revalidates frozen evidence and runs under its arm's deadline.
func (evaluation *PassageEvaluation) Select(ctx context.Context, limit int) ([]embeddingeval.PassageCandidate, embeddingeval.PassageSelection, error) {
	diagnostics := embeddingeval.PassageSelection{}
	if limit != 1 && limit != 3 {
		return nil, diagnostics, errors.New("passage limit must be 1 or 3")
	}
	selected := slices.Clone(evaluation.Candidates)
	index := make(map[string]int, len(selected))
	nodes := make(map[int64]struct{}, len(selected))
	var fences []store.EvaluationLexicalFence
	var currentSegments = make(map[string]string)
	for i, result := range evaluation.Report.Results {
		index[result.Document.ContentVersionID], nodes[result.Document.NodeID] = i, struct{}{}
		for _, reference := range result.Evidence {
			if reference.SegmentID != "" {
				currentSegments[result.Document.ContentVersionID] = reference.SegmentID
			}
			if reference.BuildID != "" {
				fence := store.EvaluationLexicalFence{ContentVersionID: result.Document.ContentVersionID, BuildID: reference.BuildID}
				if !slices.Contains(fences, fence) {
					fences = append(fences, fence)
				}
			}
		}
	}
	rows, capped, err := evaluation.catalog.SearchEvaluationLexicalPassages(ctx, evaluation.query.Text, fences, 4096)
	diagnostics.Capped = capped
	if err != nil {
		return nil, diagnostics, err
	}
	lexical := make(map[string][]evaluationPassage)
	var texts []string
	for _, row := range rows {
		lexical[row.ContentVersionID] = append(lexical[row.ContentVersionID], evaluationPassage{text: row.Text, source: "lexical", build: row.BuildID, unit: row.UnitID,
			start: row.CharStart, end: row.CharEnd, segment: row.SegmentID, focus: len(texts), evidence: EvidenceReference{Kind: "rendition_segment", BuildID: row.BuildID, SegmentID: row.SegmentID}})
		texts = append(texts, row.Text)
	}
	chunks := make(map[string][]evaluationPassage)
	if evaluation.backend.space != "" {
		b := evaluation.backend
		resolution, err := evaluation.catalog.ResolveEvaluationSemanticPassages(ctx, evaluation.query.ProcessingProfileFingerprint, evaluation.query.BindingID, b.inputKind,
			b.space, b.source, b.neighbors, evaluation.query.Scope, nodes, 3)
		if err != nil {
			return nil, diagnostics, err
		}
		for _, item := range resolution.Candidates {
			input, err := evaluation.searcher.mediaEvidence.resolveInput(ctx, item.MediaEvidence, item.InputID)
			if err != nil {
				return nil, diagnostics, err
			}
			span := input.sourceSpan
			if span.CharEnd <= span.CharStart {
				diagnostics.UnknownCoordinates++
				continue
			}
			chunks[item.ContentVersionID] = append(chunks[item.ContentVersionID], evaluationPassage{text: input.excerpt, source: "chunk", build: item.MediaEvidence.BuildID,
				unit: strconv.Itoa(span.UnitIndex), start: span.CharStart, end: span.CharEnd, focus: len(texts),
				evidence: EvidenceReference{Kind: "embedding", VectorSpaceID: item.VectorSpaceID, EmbeddingSetID: item.EmbeddingSetID,
					InputGenerationID: item.InputGenerationID, InputID: item.InputID, InputKind: item.InputKind, BuildID: item.MediaEvidence.BuildID, SourceManifestChecksum: b.source}})
			texts = append(texts, input.excerpt)
		}
	}
	focus, err := evaluation.catalog.EvaluationPassageFocus(ctx, evaluation.query.Text, texts)
	if err != nil {
		return nil, diagnostics, err
	}
	report := evaluation.Report
	report.Results = slices.Clone(report.Results)
	for version, i := range index {
		var kept []evaluationPassage
		lanes := [][]evaluationPassage{lexical[version], chunks[version]}
		for rank := range max(len(lanes[0]), len(lanes[1])) {
			for _, lane := range lanes {
				if rank >= len(lane) {
					continue
				}
				passage := lane[rank]
				span := focus[passage.focus]
				start, end := embeddingeval.BoundedPassageWindow(passage.text, span[0], span[1], 1536)
				passage.text = string([]rune(passage.text)[start:end])
				passage.end, passage.start = passage.start+end, passage.start+start
				if passage.text == "" || overlapsEvaluationPassage(kept, passage) {
					diagnostics.Excluded++
					continue
				}
				if len(kept) == limit {
					diagnostics.Excluded++
					continue
				}
				kept = append(kept, passage)
				if passage.source == "lexical" {
					diagnostics.Lexical++
					if currentSegments[version] != "" && currentSegments[version] != passage.segment {
						diagnostics.LateSegments++
					}
				} else {
					diagnostics.Chunks++
				}
			}
		}
		selected[i].Passages = nil
		for _, passage := range kept {
			selected[i].Passages = append(selected[i].Passages, passage.text)
			report.Results[i].Evidence = append(slices.Clone(report.Results[i].Evidence), passage.evidence)
		}
		if len(kept) == 0 {
			diagnostics.Fallbacks++
		}
	}
	if err := evaluation.catalog.RevalidateEvaluationLexicalFences(ctx, fences); err != nil {
		return nil, diagnostics, err
	}
	current, err := evaluation.searcher.revalidateReport(ctx, evaluation.query, report)
	if err != nil {
		return nil, diagnostics, err
	}
	if len(current.Results) != len(selected) {
		return nil, diagnostics, errors.New("passage evaluation evidence became stale")
	}
	return selected, diagnostics, ctx.Err()
}

func overlapsEvaluationPassage(kept []evaluationPassage, candidate evaluationPassage) bool {
	for _, prior := range kept {
		if prior.source == candidate.source && prior.build == candidate.build && prior.unit == candidate.unit &&
			2*(min(prior.end, candidate.end)-max(prior.start, candidate.start)) > candidate.end-candidate.start {
			return true
		}
	}
	return false
}

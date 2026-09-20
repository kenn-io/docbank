package retrieval

import (
	"bytes"
	"context"
	"errors"
	"time"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/store"
	"go.kenn.io/docbank/internal/vectorindex"
)

type SimilarBackend interface {
	AcquireSimilarSearchAuthority(ctx context.Context, profile, binding, owner string, at time.Time, duration time.Duration, scope store.SearchOptions, source store.SimilarSource) (store.SimilarSearchAuthority, error)
	ResolveSimilarCandidates(ctx context.Context, profile, binding string, kind document.EmbeddingInputKind, space, manifest string, neighbors []vectorindex.Neighbor, limit int, scope store.SearchOptions, source store.SimilarSource) (store.SimilarSearchResolution, error)
	ReleaseVectorIndexGeneration(ctx context.Context, leaseID string, token int64, at time.Time) error
}

type SimilarQuery struct {
	Source                                  DocumentIdentity
	Limit                                   int
	Scope                                   store.SearchOptions
	ProcessingProfileFingerprint, BindingID string
}

type MissingCoverage struct {
	Kind, BindingID, ProfileFingerprint, ContentVersionID string
}

type SimilarResult struct {
	Document       DocumentIdentity
	Rank           int
	Score          float64
	Path, BlobHash string
	DuplicateCount int
	Evidence       []EvidenceReference
}

type SimilarReport struct {
	Source          DocumentIdentity
	BindingID       string
	MissingCoverage *MissingCoverage
	Coverage        Coverage
	Results         []SimilarResult
	Truncated       bool
}

func (searcher *Searcher) Similar(ctx context.Context, query SimilarQuery) (_ SimilarReport, retErr error) {
	backend, ok := searcher.backend.(SimilarBackend)
	if !ok {
		return SimilarReport{}, errors.New("similar retrieval is unavailable")
	}
	if query.Source.VaultID != searcher.backend.VaultID() || query.Source.NodeID <= 0 || query.Source.ContentVersionID == "" {
		return SimilarReport{}, errors.New("similar source identity is invalid")
	}
	if query.Limit < 1 || query.Limit > MaxCandidateLimit {
		return SimilarReport{}, errors.New("similar search limit is invalid")
	}
	source := store.SimilarSource{NodeID: query.Source.NodeID, ContentVersionID: query.Source.ContentVersionID}
	report := SimilarReport{Source: query.Source, BindingID: query.BindingID, Coverage: Coverage{State: CoverageUnknown}, Results: []SimilarResult{}}
	authority, err := backend.AcquireSimilarSearchAuthority(ctx, query.ProcessingProfileFingerprint, query.BindingID,
		searcher.owner, searcher.clock().UTC(), searcher.leaseDuration, query.Scope, source)
	if errors.Is(err, store.ErrSimilarSourceUnavailable) {
		report.MissingCoverage = &MissingCoverage{Kind: "embedding", BindingID: query.BindingID,
			ProfileFingerprint: query.ProcessingProfileFingerprint, ContentVersionID: source.ContentVersionID}
		return report, nil
	}
	if err != nil {
		return SimilarReport{}, err
	}
	defer func() {
		if err := backend.ReleaseVectorIndexGeneration(context.WithoutCancel(ctx), authority.Lease.ID, authority.Lease.FencingToken, searcher.clock().UTC()); err != nil {
			if retErr == nil {
				retErr = err
			} else {
				retErr = &semanticReleaseError{operation: retErr, release: err}
			}
		}
	}()
	if authority.ANNRows == nil {
		return SimilarReport{}, errors.New("similar authority lacks source-fenced ANN rows")
	}
	stored := authority.Lease.Generation
	generation, err := vectorindex.OpenGeneration(bytes.NewReader(stored.Bytes), int64(len(stored.Bytes)))
	if err != nil {
		return SimilarReport{}, err
	}
	metadata, descriptor := generation.Metadata(), authority.VectorSpace.Descriptor
	if metadata.VectorSpaceID != authority.VectorSpace.ID || metadata.Dimension != descriptor.Dimension ||
		metadata.Metric != descriptor.Metric || metadata.Normalization != descriptor.Normalization ||
		metadata.Manifest.Checksum != stored.IndexManifestChecksum || metadata.RowCount != stored.RowCount {
		return SimilarReport{}, errors.New("leased vector generation is incompatible with similar authority")
	}
	if err := ctx.Err(); err != nil {
		return SimilarReport{}, err
	}
	neighbors, err := generation.SearchSimilarRows(ctx, authority.SourceRows, authority.ANNRows)
	if err != nil {
		return SimilarReport{}, err
	}
	if metadata.Metric == document.VectorMetricL2 {
		for i := range neighbors {
			neighbors[i].Score = -neighbors[i].Distance
		}
	}
	resolution, err := backend.ResolveSimilarCandidates(ctx, query.ProcessingProfileFingerprint, query.BindingID,
		authority.InputKind, authority.VectorSpace.ID, stored.SourceManifestChecksum, neighbors, query.Limit, query.Scope, source)
	if err != nil {
		return SimilarReport{}, err
	}
	if resolution.SourceManifestChecksum != stored.SourceManifestChecksum {
		return SimilarReport{}, store.ErrVectorIndexSourceStale
	}
	report.Coverage = Coverage{BindingRequired: authority.BindingRequired, ScopedDocuments: resolution.ScopedDocuments, CompleteDocuments: resolution.CompleteDocuments, State: CoverageComplete}
	if resolution.ScopedDocuments != resolution.CompleteDocuments {
		report.Coverage.State = CoverageIncomplete
	}
	report.Truncated = resolution.Truncated
	for _, item := range resolution.Candidates {
		reference := EvidenceReference{Kind: "embedding", VaultID: item.VaultID, NodeID: item.NodeID, NodeRevision: item.NodeRevision,
			ContentVersionID: item.ContentVersionID, VectorSpaceID: item.VectorSpaceID, EmbeddingSetID: item.EmbeddingSetID,
			InputGenerationID: item.InputGenerationID, InputID: item.InputID, InputKind: item.InputKind,
			BuildID: item.MediaEvidence.BuildID, SourceManifestChecksum: resolution.SourceManifestChecksum}
		report.Results = append(report.Results, SimilarResult{Document: DocumentIdentity{VaultID: item.VaultID, NodeID: item.NodeID,
			ContentVersionID: item.ContentVersionID}, Rank: len(report.Results) + 1, Score: item.Score, Path: item.Path,
			BlobHash: item.BlobHash, DuplicateCount: item.DuplicateCount, Evidence: []EvidenceReference{reference}})
	}
	return report, nil
}

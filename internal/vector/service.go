package vector

import (
	"context"

	kitvec "go.kenn.io/kit/vector"
)

// Service binds one configured encoder and authoritative source to an Index.
// It is the narrow daemon-facing embeddings lifecycle surface.
type Service struct {
	Index       *Index
	Source      SourceFunc
	Generation  kitvec.Generation
	Encode      kitvec.EncodeFunc
	BatchSize   int
	Concurrency int
}

// Build refreshes and fills the configured generation.
func (s *Service) Build(ctx context.Context, progress func(Progress)) (BuildResult, error) {
	return s.Index.Build(ctx, s.Source, s.Generation, s.Encode,
		s.BatchSize, s.Concurrency, progress)
}

// Generations reports retained generations and current coverage.
func (s *Service) Generations(ctx context.Context) ([]GenerationInfo, error) {
	return s.Index.Generations(ctx)
}

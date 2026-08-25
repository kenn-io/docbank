// Package processing publishes verified document derivatives without making
// their provider or rebuild state part of document authority.
package processing

import (
	"context"

	"go.kenn.io/docbank/internal/store"
)

// LexicalGeneration identifies one complete immutable search projection.
type LexicalGeneration = store.LexicalGeneration

type lexicalGenerationStore interface {
	StageLexicalGeneration(
		ctx context.Context, generationID string,
	) (store.LexicalGeneration, error)
}

// RebuildLexicalGeneration builds a complete unreachable FTS generation.
// Publication remains a separate atomic attachment and head operation.
func RebuildLexicalGeneration(
	ctx context.Context, catalog *store.Store, generationID string,
) (LexicalGeneration, error) {
	return rebuildLexicalGeneration(ctx, catalog, generationID)
}

func rebuildLexicalGeneration(
	ctx context.Context, catalog lexicalGenerationStore, generationID string,
) (LexicalGeneration, error) {
	return catalog.StageLexicalGeneration(ctx, generationID)
}

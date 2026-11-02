package vector

import (
	"context"
	"errors"
)

// ErrStale reports that a document changed between the scan that read its
// content and the save that would stamp it embedded. The save must persist
// nothing and the document stays pending, so a later fill re-reads it at
// its new revision. Store implementations return it (possibly wrapped)
// from SaveVectors when the revision check fails.
var ErrStale = errors.New("document changed since scan")

// Pending is one document that still needs embedding for a generation,
// paired with the text to embed.
type Pending[K comparable] struct {
	Doc     K
	Content string
	// Revision is an opaque token identifying the version of Content that
	// was read, for example a last-modified timestamp or version counter.
	// It is passed back to SaveVectors, which stamps the document only if
	// the revision is unchanged. Stores that do not track revisions leave
	// it nil.
	Revision any
}

// ChunkVector is a single chunk's embedding, ready to persist.
type ChunkVector struct {
	ChunkIndex int
	Vector     Vector
}

// Store is the persistence contract the Fill and Search flows depend on.
// Implementations are a function of the caller's source system — a SQLite,
// pgvector, or DuckDB table — and own all backend SQL and query
// construction. The flows never open a database or build SQL themselves.
//
// K is the caller's document key type and G its generation id type; the
// package compares both for equality but never interprets them.
type Store[K, G comparable] interface {
	// PendingForGeneration returns up to limit documents that are not yet
	// embedded for gen, in a stable order. Implementations typically scan
	// for "embed_gen IS NULL OR embed_gen <> gen". A document must stop
	// being reported once SaveVectors has persisted it for gen, so that a
	// fill loop terminates.
	PendingForGeneration(ctx context.Context, gen G, limit int) ([]Pending[K], error)

	// SaveVectors persists every chunk vector for doc under gen and marks
	// doc as embedded for gen (the scan-and-fill stamp), atomically.
	//
	// revision is the Pending.Revision token read with the content. When
	// the store tracks revisions and doc's revision no longer matches, the
	// save must persist nothing and return an error wrapping ErrStale, so
	// stale vectors are never stamped over a concurrent edit.
	//
	// An empty vectors slice is a stamp-only save: it marks doc handled
	// for gen without storing vectors (used for empty content and for
	// documents skipped after a permanent encode failure).
	SaveVectors(ctx context.Context, gen G, doc K, revision any, vectors []ChunkVector) error

	// LiveGenerations returns the generations a search should query, in
	// descending preference. During a migration the building generation
	// precedes the active one, so Merge keeps the newer generation's hit
	// when a document appears in both.
	LiveGenerations(ctx context.Context) ([]G, error)

	// QueryGeneration returns chunk-level hits for query within gen,
	// ranked best first and capped at limit. This is where each backend's
	// vector query construction lives.
	QueryGeneration(ctx context.Context, gen G, query Vector, limit int) ([]Hit[K], error)
}

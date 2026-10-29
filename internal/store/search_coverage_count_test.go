package store

import (
	"context"
	"database/sql"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
)

type semanticCoverageQuery struct {
	query string
	args  []any
}

type semanticCoverageRecorder struct {
	metadataQuerier

	queries []semanticCoverageQuery
}

func (q *semanticCoverageRecorder) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	q.queries = append(q.queries, semanticCoverageQuery{query: query, args: slices.Clone(args)})
	return q.metadataQuerier.QueryRowContext(ctx, query, args...)
}

func TestSemanticCoverageCountsOneCurrentHeadPerDocument(t *testing.T) {
	t.Parallel()
	for _, shape := range []struct {
		name   string
		kind   document.EmbeddingInputKind
		shared bool
	}{
		{"original", document.EmbeddingInputOriginalFile, false},
		{"chunk", document.EmbeddingInputRenditionChunk, false},
		{"shared chunk", document.EmbeddingInputRenditionChunk, true},
	} {
		t.Run(shape.name, func(t *testing.T) {
			s, profile, records, _ := newSemanticPrefixFixture(t, shape.kind, 3, shape.shared)
			// Another live head shares this version and vector space. The fixed
			// profile/binding/kind tuple must still select exactly one head.
			otherBinding := "required"
			if shape.kind == document.EmbeddingInputRenditionChunk {
				otherBinding = "optional"
			}
			other := embeddingSetFixture(s, records[0].ContentVersionID, profile.Fingerprint,
				document.EmbeddingInputOriginalFile, otherBinding, "")
			require.Equal(t, records[0].VectorSpace.ID, other.VectorSpace.ID)
			require.NoError(t, s.StageEmbeddingSet(t.Context(), other))
			require.NoError(t, s.PublishEmbeddingHead(t.Context(), EmbeddingHeadRecord{FencingToken: 1,
				Key:   EmbeddingHeadKey{ContentVersionID: other.ContentVersionID, BindingID: other.BindingID, InputKind: other.InputKind},
				SetID: other.ID, VectorSpaceID: other.VectorSpace.ID, ProcessingProfileFingerprint: profile.Fingerprint,
				PublishedAt: embeddingCatalogTime}))
			version, err := s.ContentVersionByID(t.Context(), records[0].ContentVersionID)
			require.NoError(t, err)
			tag, err := s.CreateTag(t.Context(), "coverage-selected")
			require.NoError(t, err)
			_, err = s.AssignTag(t.Context(), tag.ID, version.NodeID, UnconditionalRev)
			require.NoError(t, err)
			dir, err := s.Mkdir(t.Context(), s.RootID(), "coverage-selected")
			require.NoError(t, err)
			node, err := s.NodeByID(t.Context(), version.NodeID)
			require.NoError(t, err)
			_, _, err = s.Move(t.Context(), node.ID, dir.ID, node.Name, UnconditionalRev)
			require.NoError(t, err)
			for _, filter := range []struct {
				name string
				opts SearchOptions
				want int
			}{
				{"all", SearchOptions{}, 3},
				{"version", SearchOptions{ContentVersionIDs: []string{records[0].ContentVersionID}}, 1},
				{"tag", SearchOptions{TagID: tag.ID}, 1},
				{"ancestry", SearchOptions{UnderNodeID: dir.ID}, 1},
				{"mime excluded", SearchOptions{MIMEType: "image/png"}, 0},
				{"time excluded", SearchOptions{ModifiedBefore: "2000-01-01T00:00:00Z"}, 0},
			} {
				t.Run(filter.name, func(t *testing.T) {
					opts, err := s.NormalizeSearchOptions(t.Context(), filter.opts)
					require.NoError(t, err)
					require.NoError(t, s.withStorageTx(t.Context(), func(tx *sql.Tx) error {
						recorder := &semanticCoverageRecorder{metadataQuerier: tx}
						_, complete, err := semanticSearchCoverageTx(t.Context(), recorder, profile.Fingerprint,
							records[0].BindingID, records[0].InputKind, records[0].VectorSpace.ID, opts)
						require.NoError(t, err)
						require.Equal(t, filter.want, complete)
						require.Len(t, recorder.queries, 2, "population remains before completeness")
						require.NotContains(t, recorder.queries[0].query, "JOIN embedding_heads")
						query := recorder.queries[1]
						require.Contains(t, query.query, "JOIN embedding_heads eh")
						require.Len(t, query.args, strings.Count(query.query, "?"))
						// The current-version PK gives at most one row per node. Fixed
						// profile/binding/kind fills the head PK; set/generation joins
						// use PKs, and chunk/filter predicates do not multiply rows.
						legacy := strings.Replace(query.query, "COUNT(*)", "COUNT(DISTINCT n.id)", 1)
						var legacyCount int
						require.NoError(t, tx.QueryRowContext(t.Context(), legacy, query.args...).Scan(&legacyCount))
						require.Equal(t, legacyCount, complete, "count parity includes shared vector rows and unrelated heads")
						rows, err := tx.QueryContext(t.Context(), "EXPLAIN QUERY PLAN "+query.query, query.args...)
						require.NoError(t, err)
						defer func() { require.NoError(t, rows.Close()) }()
						for rows.Next() {
							var id, parent, unused int
							var detail string
							require.NoError(t, rows.Scan(&id, &parent, &unused, &detail))
							require.NotContains(t, strings.ToLower(detail), "count(distinct)", "one current head cannot multiply a document: %s", detail)
						}
						return rows.Err()
					}))
				})
			}
		})
	}
}

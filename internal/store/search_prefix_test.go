package store

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/vectorindex"
)

type semanticPrefixRead struct {
	query string
	args  []any
	rows  int
}

// Measure rows selected by real queries in the same snapshot, rather than
// inferring bounded work from an implementation-specific SQL spelling.
type semanticPrefixQuerier struct {
	metadataQuerier

	reads []semanticPrefixRead
}

func (q *semanticPrefixQuerier) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	var count int
	if err := q.metadataQuerier.QueryRowContext(ctx, "SELECT COUNT(*) FROM ("+query+")", args...).Scan(&count); err != nil {
		return nil, err
	}
	q.reads = append(q.reads, semanticPrefixRead{query: query, args: append([]any(nil), args...), rows: count})
	return q.metadataQuerier.QueryContext(ctx, query, args...)
}

func newSemanticPrefixFixture(t *testing.T, kind document.EmbeddingInputKind, count int, shared bool) (*Store, ProcessingProfileRecord, []EmbeddingSetRecord, []vectorindex.Neighbor) {
	t.Helper()
	s, first, profile, firstAttachment := newEmbeddingCatalogFixture(t)
	binding := "optional"
	if kind == document.EmbeddingInputRenditionChunk {
		binding = "chunk"
	}
	var build string
	require.NoError(t, s.db.QueryRow(`SELECT build_id FROM rendition_attachments WHERE attachment_id=?`, firstAttachment).Scan(&build))
	var records []EmbeddingSetRecord
	var neighbors []vectorindex.Neighbor
	for i := range count {
		version, attachment := first, ""
		if kind == document.EmbeddingInputRenditionChunk {
			attachment = firstAttachment
		}
		if i > 0 {
			node, err := s.CreateFile(t.Context(), s.RootID(), fmt.Sprintf("prefix-%03d.pdf", i), catalogSourceHash, 20, "application/pdf")
			require.NoError(t, err)
			version = node.CurrentVersionID
			if kind == document.EmbeddingInputRenditionChunk {
				attachment = testSHA256([]byte("prefix-attachment-" + version))
				require.NoError(t, publishAttachmentForTest(t, s, RenditionAttachmentRecord{ID: attachment, VaultID: s.VaultID(), ContentVersionID: version, BuildID: build, Profile: profile, AttachedAt: embeddingCatalogTime}))
			}
		}
		record := embeddingSetFixture(s, version, profile.Fingerprint, kind, binding, attachment)
		if kind == document.EmbeddingInputRenditionChunk && i > 0 && !shared {
			vectors, _, err := document.DecodeVectorSetV1(record.VectorSet.Payload, document.VectorBounds{MaxRows: 128, MaxDimension: 8, MaxBytes: len(record.VectorSet.Payload)})
			require.NoError(t, err)
			for row := range vectors.Vectors {
				vectors.Vectors[row][0] += float32(i + 1)
			}
			payload, checksum, err := document.EncodeVectorSetV1(vectors)
			require.NoError(t, err)
			record.VectorSet.ID, record.VectorSet.PayloadChecksum = checksum, checksum
			record.VectorSet.Payload, record.VectorSet.PayloadBlobHash = payload, testSHA256(payload)
			require.NoError(t, s.withStorageTx(t.Context(), func(tx *sql.Tx) error {
				return s.EnsureBlobTx(tx, record.VectorSet.PayloadBlobHash, int64(len(payload)))
			}))
		}
		require.NoError(t, s.StageEmbeddingSet(t.Context(), record))
		require.NoError(t, s.PublishEmbeddingHead(t.Context(), EmbeddingHeadRecord{FencingToken: 1,
			Key: EmbeddingHeadKey{ContentVersionID: version, BindingID: binding, InputKind: kind}, SetID: record.ID,
			VectorSpaceID: record.VectorSpace.ID, ProcessingProfileFingerprint: profile.Fingerprint, PublishedAt: embeddingCatalogTime}))
		records = append(records, record)
		if !shared || i == 0 {
			for _, input := range record.InputGeneration.Inputs {
				neighbors = append(neighbors, vectorindex.Neighbor{SetID: record.VectorSet.ID, InputKey: input.ID, InputChecksum: input.RenderedChecksum, Score: 1 - float64(len(neighbors))/1000})
			}
		}
	}
	if shared {
		require.Equal(t, records[0].VectorSet.ID, records[1].VectorSet.ID)
	} else if kind == document.EmbeddingInputRenditionChunk {
		require.NotEqual(t, records[0].VectorSet.ID, records[1].VectorSet.ID)
	}
	return s, profile, records, neighbors
}

func TestSemanticCandidatePrefixMatchesFullLegacyReduction(t *testing.T) {
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
			s, profile, records, neighbors := newSemanticPrefixFixture(t, shape.kind, 2, shape.shared)
			first, err := s.ContentVersionByID(t.Context(), records[0].ContentVersionID)
			require.NoError(t, err)
			tag, err := s.CreateTag(t.Context(), "prefix-selected")
			require.NoError(t, err)
			_, err = s.AssignTag(t.Context(), tag.ID, first.NodeID, UnconditionalRev)
			require.NoError(t, err)
			dir, err := s.Mkdir(t.Context(), s.RootID(), "prefix-selected")
			require.NoError(t, err)
			node, err := s.NodeByID(t.Context(), first.NodeID)
			require.NoError(t, err)
			_, _, err = s.Move(t.Context(), node.ID, dir.ID, node.Name, UnconditionalRev)
			require.NoError(t, err)
			for _, scope := range []SearchOptions{{}, {ContentVersionIDs: []string{records[0].ContentVersionID}}, {TagID: tag.ID}, {UnderNodeID: dir.ID}, {MIMEType: "text/plain"}, {ModifiedBefore: "2000-01-01T00:00:00Z"}} {
				normalized, err := s.NormalizeSearchOptions(t.Context(), scope)
				require.NoError(t, err)
				filterSQL, args := searchFilterSQL(normalized)
				for _, excluded := range []map[int64]struct{}{nil, {first.NodeID: {}}} {
					for _, limit := range []int{1, 2} {
						require.NoError(t, s.withStorageTx(t.Context(), func(tx *sql.Tx) error {
							legacy := &semanticPrefixQuerier{metadataQuerier: tx}
							eligible, err := loadSemanticEligibility(t.Context(), legacy, profile.Fingerprint, records[0].BindingID, shape.kind, records[0].VectorSpace.ID, filterSQL, args)
							require.NoError(t, err)
							want, truncated, next := reduceSemanticCandidates(s.vaultID, records[0].VectorSpace.ID, neighbors, limit, eligible, excluded)
							query := &semanticPrefixQuerier{metadataQuerier: tx}
							got, gotTruncated, gotNext, err := s.resolveSemanticCandidatePrefix(t.Context(), query, profile.Fingerprint, records[0].BindingID, shape.kind, records[0].VectorSpace.ID, neighbors, limit, filterSQL, args, excluded)
							require.NoError(t, err)
							require.Equal(t, want, got)
							require.Equal(t, truncated, gotTruncated)
							require.Equal(t, next, gotNext)
							if shape.shared && len(scope.ContentVersionIDs) == 0 && scope.TagID == "" && scope.UnderNodeID == 0 && scope.MIMEType == "" && scope.ModifiedBefore == "" {
								require.GreaterOrEqual(t, len(query.reads), 2)
								last := query.reads[len(query.reads)-1]
								require.Equal(t, legacy.reads[0].query, last.query, "shared rows must use the exact un-ORDERED full legacy query")
								require.Equal(t, legacy.reads[0].args, last.args)
							}
							return nil
						}))
					}
				}
			}
		})
	}
}

// All rows originate in the real catalog fixture. BLOB shadow vector sets
// retain its payload, row count and manifest; no source validation is skipped.
func mutateSemanticPrefixTypes(t *testing.T, s *Store, records []EmbeddingSetRecord, mutation string) {
	t.Helper()
	before, err := s.CaptureVectorIndexSource(t.Context(), records[0].VectorSpace.ID)
	require.NoError(t, err)
	require.NoError(t, s.withStorageTx(t.Context(), func(tx *sql.Tx) error {
		column := strings.Fields(mutation)[0]
		generation := records[0].InputGeneration.ID
		if strings.HasSuffix(mutation, "collision") {
			require.Equal(t, records[0].VectorSet.ID, records[1].VectorSet.ID)
			_, err := tx.Exec(`INSERT INTO embedding_vector_sets
 SELECT CAST(vector_set_id AS BLOB),contract_version,vector_space_id,payload_blob_hash,
 payload_size,payload_checksum,manifest_checksum,row_count,dimensions
 FROM embedding_vector_sets WHERE vector_set_id=?`, records[0].VectorSet.ID)
			require.NoError(t, err)
			input, checksum := "input_id", "checksum"
			if column == "input_id" {
				input = "CAST(input_id AS BLOB)"
			}
			if column == "checksum" {
				checksum = "CAST(checksum AS BLOB)"
			}
			_, err = tx.Exec(`INSERT INTO embedding_vector_rows
 SELECT CAST(vector_set_id AS BLOB),row_id,row_order,`+input+`,dimensions,`+checksum+`
 FROM embedding_vector_rows WHERE vector_set_id=?`, records[0].VectorSet.ID)
			require.NoError(t, err)
			_, err = tx.Exec(`DROP TRIGGER embedding_sets_immutable_update`)
			require.NoError(t, err)
			_, err = tx.Exec(`UPDATE embedding_sets SET vector_set_id=CAST(vector_set_id AS BLOB) WHERE embedding_set_id=?`, records[1].ID)
			require.NoError(t, err)
			generation = records[1].InputGeneration.ID
		} else {
			_, err := tx.Exec(`DROP TRIGGER embedding_vector_rows_immutable_update`)
			require.NoError(t, err)
			_, err = tx.Exec(`UPDATE embedding_vector_rows SET `+column+`=CAST(`+column+` AS BLOB) WHERE vector_set_id=?`, records[0].VectorSet.ID)
			require.NoError(t, err)
		}
		if column == "vector_set_id" {
			return nil
		}
		if column == "checksum" {
			column = "rendered_checksum"
		}
		_, err := tx.Exec(`DROP TRIGGER embedding_generation_inputs_immutable_update`)
		require.NoError(t, err)
		_, err = tx.Exec(`UPDATE embedding_generation_inputs SET `+column+`=CAST(`+column+` AS BLOB) WHERE generation_id=?`, generation)
		return err
	}))
	after, err := s.CaptureVectorIndexSource(t.Context(), records[0].VectorSpace.ID)
	require.NoError(t, err)
	require.Equal(t, before, after, "all original source loads, counts and manifests still validate")
}

func TestSemanticCandidatePrefixTypedRowsMatchFullLegacyReduction(t *testing.T) {
	t.Parallel()
	for _, kind := range []document.EmbeddingInputKind{document.EmbeddingInputOriginalFile, document.EmbeddingInputRenditionChunk} {
		mutations := []string{"input_id pair", "checksum pair"}
		if kind == document.EmbeddingInputRenditionChunk {
			mutations = append(mutations, "vector_set_id collision", "input_id collision", "checksum collision")
		}
		for _, mutation := range mutations {
			t.Run(string(kind)+"/"+mutation, func(t *testing.T) {
				s, profile, records, neighbors := newSemanticPrefixFixture(t, kind, 2, kind == document.EmbeddingInputRenditionChunk)
				mutateSemanticPrefixTypes(t, s, records, mutation)
				first, err := s.ContentVersionByID(t.Context(), records[0].ContentVersionID)
				require.NoError(t, err)
				for _, scope := range []SearchOptions{{}, {MIMEType: "application/pdf"}, {ContentVersionIDs: []string{records[0].ContentVersionID}}} {
					normalized, err := s.NormalizeSearchOptions(t.Context(), scope)
					require.NoError(t, err)
					filter, args := searchFilterSQL(normalized)
					for _, excluded := range []map[int64]struct{}{nil, {first.NodeID: {}}} {
						for _, limit := range []int{1, 3} {
							require.NoError(t, s.withStorageTx(t.Context(), func(tx *sql.Tx) error {
								legacy := &semanticPrefixQuerier{metadataQuerier: tx}
								eligible, err := loadSemanticEligibility(t.Context(), legacy, profile.Fingerprint, records[0].BindingID, kind, records[0].VectorSpace.ID, filter, args)
								require.NoError(t, err)
								require.NotEmpty(t, eligible, "typed fixture retains genuine native membership")
								want, truncated, next := reduceSemanticCandidates(s.vaultID, records[0].VectorSpace.ID, neighbors, limit, eligible, excluded)
								query := &semanticPrefixQuerier{metadataQuerier: tx}
								got, gotTruncated, gotNext, err := s.resolveSemanticCandidatePrefix(t.Context(), query, profile.Fingerprint, records[0].BindingID, kind, records[0].VectorSpace.ID, neighbors, limit, filter, args, excluded)
								require.NoError(t, err)
								require.Equal(t, want, got)
								require.Equal(t, truncated, gotTruncated)
								require.Equal(t, next, gotNext)
								if strings.HasSuffix(mutation, "collision") && len(scope.ContentVersionIDs) == 0 {
									require.Len(t, query.reads, 2, "same-byte native classes must use shared-key fallback")
									require.Equal(t, legacy.reads[0], query.reads[1], "retain exact full SQL, arguments and unordered last-member choice")
								}
								return nil
							}))
						}
					}
				}
			})
		}
	}
}

func TestSemanticCandidatePrefixBoundsReadsAndKeepsGlobalLookahead(t *testing.T) {
	t.Parallel()
	s, profile, records, neighbors := newSemanticPrefixFixture(t, document.EmbeddingInputOriginalFile, 140, false)
	filterSQL, args := moveSemanticPrefixRowsAfterCapture(t, s, records, neighbors, records[:128])
	first, err := s.ContentVersionByID(t.Context(), records[128].ContentVersionID)
	require.NoError(t, err)
	require.NoError(t, s.withStorageTx(t.Context(), func(tx *sql.Tx) error {
		eligible, err := loadSemanticEligibility(t.Context(), tx, profile.Fingerprint, records[0].BindingID, records[0].InputKind, records[0].VectorSpace.ID, filterSQL, args)
		require.NoError(t, err)
		excluded := map[int64]struct{}{first.NodeID: {}}
		want, truncated, next := reduceSemanticCandidates(s.vaultID, records[0].VectorSpace.ID, neighbors, 5, eligible, excluded)
		query := &semanticPrefixQuerier{metadataQuerier: tx}
		got, gotTruncated, gotNext, err := s.resolveSemanticCandidatePrefix(t.Context(), query, profile.Fingerprint, records[0].BindingID, records[0].InputKind, records[0].VectorSpace.ID, neighbors, 5, filterSQL, args, excluded)
		require.NoError(t, err)
		require.Equal(t, want, got)
		require.Equal(t, truncated, gotTruncated)
		require.Equal(t, next, gotNext)
		require.Len(t, got, 5)
		require.True(t, gotTruncated)
		require.Greater(t, gotNext, 128, "withheld lookahead uses the original global neighbor index")
		require.Len(t, query.reads, 2)
		require.Zero(t, query.reads[0].rows, "the first batch became ineligible after scoring authority capture")
		total := 0
		for _, read := range query.reads {
			require.LessOrEqual(t, read.rows, 128)
			total += read.rows
		}
		require.Less(t, total, len(records), "top-five resolution must not materialize the entire selected population")
		return nil
	}))
}

func TestSemanticCandidatePrefixZeroRemainingLookahead(t *testing.T) {
	t.Parallel()
	s, profile, records, stream := newSemanticPrefixFixture(t, document.EmbeddingInputOriginalFile, 129, false)
	filterSQL, args := moveSemanticPrefixRowsAfterCapture(t, s, records, stream, records[5:128])
	require.NoError(t, s.withStorageTx(t.Context(), func(tx *sql.Tx) error {
		eligible, err := loadSemanticEligibility(t.Context(), tx, profile.Fingerprint, records[0].BindingID, records[0].InputKind, records[0].VectorSpace.ID, filterSQL, args)
		require.NoError(t, err)
		excluded := map[int64]struct{}{}
		want, truncated, next := reduceSemanticCandidates(s.vaultID, records[0].VectorSpace.ID, stream, 5, eligible, excluded)
		query := &semanticPrefixQuerier{metadataQuerier: tx}
		got, gotTruncated, gotNext, err := s.resolveSemanticCandidatePrefix(t.Context(), query, profile.Fingerprint, records[0].BindingID, records[0].InputKind, records[0].VectorSpace.ID, stream, 5, filterSQL, args, excluded)
		require.NoError(t, err)
		require.Equal(t, want, got)
		require.Equal(t, truncated, gotTruncated)
		require.Equal(t, next, gotNext)
		require.True(t, gotTruncated)
		require.Equal(t, 128, gotNext)
		require.Len(t, got, 5)
		require.Len(t, query.reads, 2, "zero remaining capacity still checks the next batch for lookahead")
		require.Equal(t, 5, query.reads[0].rows)
		require.Equal(t, 1, query.reads[1].rows)
		require.Empty(t, excluded, "batch accumulation must not mutate caller exclusions")
		return nil
	}))
}

func TestSemanticCandidatePrefixSharedFallbackAfterPartialReduction(t *testing.T) {
	t.Parallel()
	s, profile, records, neighbors := newSemanticPrefixFixture(t, document.EmbeddingInputRenditionChunk, 129, false)
	var build string
	require.NoError(t, s.db.QueryRow(`SELECT build_id FROM rendition_attachments WHERE attachment_id=?`, records[0].InputGeneration.AttachmentID).Scan(&build))
	node, err := s.CreateFile(t.Context(), s.RootID(), "prefix-shared-copy.pdf", catalogSourceHash, 20, "application/pdf")
	require.NoError(t, err)
	attachment := testSHA256([]byte("partial-prefix-copy-" + node.CurrentVersionID))
	require.NoError(t, publishAttachmentForTest(t, s, RenditionAttachmentRecord{ID: attachment, VaultID: s.VaultID(), ContentVersionID: node.CurrentVersionID, BuildID: build, Profile: profile, AttachedAt: embeddingCatalogTime}))
	copyRecord := embeddingSetFixture(s, node.CurrentVersionID, profile.Fingerprint, records[0].InputKind, records[0].BindingID, attachment)
	require.Equal(t, records[0].VectorSet.ID, copyRecord.VectorSet.ID)
	require.NoError(t, s.StageEmbeddingSet(t.Context(), copyRecord))
	require.NoError(t, s.PublishEmbeddingHead(t.Context(), EmbeddingHeadRecord{FencingToken: 1,
		Key:   EmbeddingHeadKey{ContentVersionID: copyRecord.ContentVersionID, BindingID: copyRecord.BindingID, InputKind: copyRecord.InputKind},
		SetID: copyRecord.ID, VectorSpaceID: copyRecord.VectorSpace.ID, ProcessingProfileFingerprint: profile.Fingerprint, PublishedAt: embeddingCatalogTime}))
	sharedCount := len(records[0].InputGeneration.Inputs)
	// One eligible row precedes 127 rows that move out of scope. The next
	// batch encounters one scoring identity shared by real document members.
	filler := neighbors[2*sharedCount:]
	stream := slices.Concat(neighbors[sharedCount:sharedCount+1], filler[:127], neighbors[:sharedCount],
		neighbors[sharedCount+1:2*sharedCount], filler[127:])
	for i := range stream {
		stream[i].Score = 1 - float64(i)/1000
	}
	filterSQL, args := moveSemanticPrefixRowsAfterCapture(t, s, append(records, copyRecord), stream, records[2:])
	require.NoError(t, s.withStorageTx(t.Context(), func(tx *sql.Tx) error {
		legacy := &semanticPrefixQuerier{metadataQuerier: tx}
		eligible, err := loadSemanticEligibility(t.Context(), legacy, profile.Fingerprint, records[0].BindingID, records[0].InputKind, records[0].VectorSpace.ID, filterSQL, args)
		require.NoError(t, err)
		excluded := map[int64]struct{}{}
		want, truncated, next := reduceSemanticCandidates(s.vaultID, records[0].VectorSpace.ID, stream, 2, eligible, excluded)
		query := &semanticPrefixQuerier{metadataQuerier: tx}
		got, gotTruncated, gotNext, err := s.resolveSemanticCandidatePrefix(t.Context(), query, profile.Fingerprint, records[0].BindingID, records[0].InputKind, records[0].VectorSpace.ID, stream, 2, filterSQL, args, excluded)
		require.NoError(t, err)
		require.Equal(t, want, got, "shared fallback must discard prior partial reduction and use original exclusions")
		require.Equal(t, truncated, gotTruncated)
		require.Equal(t, next, gotNext)
		require.Len(t, got, 2)
		require.Len(t, query.reads, 3)
		require.Equal(t, 1, query.reads[0].rows, "fallback follows a batch that already reduced a candidate")
		last := query.reads[len(query.reads)-1]
		require.Equal(t, legacy.reads[0].query, last.query)
		require.Equal(t, legacy.reads[0].args, last.args)
		require.Empty(t, excluded)
		return nil
	}))
}

func TestSemanticCandidatePrefixUsesIdentityIndexes(t *testing.T) {
	t.Parallel()
	s, profile, records, neighbors := newSemanticPrefixFixture(t, document.EmbeddingInputOriginalFile, 140, false)
	require.NoError(t, s.withStorageTx(t.Context(), func(tx *sql.Tx) error {
		query := &semanticPrefixQuerier{metadataQuerier: tx}
		_, _, _, err := s.resolveSemanticCandidatePrefix(t.Context(), query, profile.Fingerprint, records[0].BindingID, records[0].InputKind, records[0].VectorSpace.ID, neighbors, 5, "", nil, nil)
		require.NoError(t, err)
		require.NotEmpty(t, query.reads)
		read := query.reads[0]
		rows, err := tx.QueryContext(t.Context(), "EXPLAIN QUERY PLAN "+read.query, read.args...)
		require.NoError(t, err)
		defer func() { require.NoError(t, rows.Close()) }()
		var details []string
		for rows.Next() {
			var id, parent, unused int
			var detail string
			require.NoError(t, rows.Scan(&id, &parent, &unused, &detail))
			details = append(details, detail)
		}
		require.NoError(t, rows.Err())
		require.NoError(t, rows.Close())
		plan := strings.Join(details, "\n")
		require.NotContains(t, plan, "embedding_sets_vector_space", "bounded metadata rows must also avoid scanning the selected vector-space population")
		require.Contains(t, plan, "embedding_sets_vector_set")
		require.Contains(t, plan, "vector_set_id=? AND input_id=?")
		return nil
	}))
}

// Capture the same exact identities used for scoring, then change only ancestry.
// The generation stays valid while fresh candidate metadata rejects moved rows.
func moveSemanticPrefixRowsAfterCapture(t *testing.T, s *Store, records []EmbeddingSetRecord, neighbors []vectorindex.Neighbor, moved []EmbeddingSetRecord) (string, []any) {
	t.Helper()
	dir, err := s.Mkdir(t.Context(), s.RootID(), "prefix-scope")
	require.NoError(t, err)
	move := func(record EmbeddingSetRecord, parentID int64) {
		t.Helper()
		version, err := s.ContentVersionByID(t.Context(), record.ContentVersionID)
		require.NoError(t, err)
		node, err := s.NodeByID(t.Context(), version.NodeID)
		require.NoError(t, err)
		_, _, err = s.Move(t.Context(), node.ID, parentID, node.Name, node.Revision)
		require.NoError(t, err)
	}
	for _, record := range records {
		move(record, dir.ID)
	}
	opts, err := s.NormalizeSearchOptions(t.Context(), SearchOptions{UnderNodeID: dir.ID})
	require.NoError(t, err)
	first := records[0]
	source, _, _, rows, err := s.semanticSearchAuthorityFence(t.Context(), first.ProcessingProfileFingerprint,
		first.BindingID, first.InputKind, first.VectorSpace.ID, opts, nil)
	require.NoError(t, err)
	identities := make([]vectorindex.RowIdentity, 0, len(neighbors))
	seen := make(map[vectorindex.RowIdentity]struct{}, len(neighbors))
	for _, neighbor := range neighbors {
		identity := vectorindex.RowIdentity{SetID: neighbor.SetID, InputKey: neighbor.InputKey, InputChecksum: neighbor.InputChecksum}
		require.NotContains(t, seen, identity, "scoring cannot return duplicate identities")
		seen[identity] = struct{}{}
		identities = append(identities, identity)
	}
	require.ElementsMatch(t, rows, identities, "every scoring identity must come from the captured authority")
	for _, record := range moved {
		move(record, s.RootID())
	}
	current, err := s.CaptureVectorIndexSource(t.Context(), first.VectorSpace.ID)
	require.NoError(t, err)
	require.Equal(t, source.ManifestChecksum, current.ManifestChecksum, "ancestry changes must leave the scored generation valid")
	return searchFilterSQL(opts)
}

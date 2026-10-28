package store

import (
	"context"
	"database/sql"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/vectorindex"
)

func originalSnapshotErrorParity(t *testing.T, want, got error) {
	t.Helper()
	if want == nil {
		require.NoError(t, got)
		return
	}
	require.Error(t, got)
	require.Equal(t, want.Error(), got.Error())
}

// compareOriginalSnapshotSQL keeps the owning generic SQL as an independent
// oracle, including predicates added to those fallback helpers in the future.
func compareOriginalSnapshotSQL(t *testing.T, s *Store, profile ProcessingProfileRecord, record EmbeddingSetRecord, opts SearchOptions, reusable bool, witness EmbeddingSetRecord) {
	t.Helper()
	normalized, err := s.NormalizeSearchOptions(t.Context(), opts)
	require.NoError(t, err)
	var source VectorIndexSource
	var required, complete int
	var rows []vectorindex.RowIdentity
	var validated bool
	require.NoError(t, s.withStorageTx(t.Context(), func(tx *sql.Tx) error {
		compareSingletonCaptureTx(t, s, tx, record, normalized)
		old, oldErr := captureVectorIndexSourceTx(t.Context(), tx, record.VectorSpace.ID)
		c := newOriginalSemanticSnapshot(profile.Fingerprint, record.BindingID, record.VectorSpace.ID, record.InputKind, normalized, true)
		require.NotNil(t, c)
		actual, actualErr := captureVectorIndexSourceObservedTx(t.Context(), tx, record.VectorSpace.ID, c.observe)
		originalSnapshotErrorParity(t, oldErr, actualErr)
		if oldErr == nil {
			require.Equal(t, old, actual, "ordered source members and checksum unchanged")
			source = old
			validated = true
			require.Equal(t, reusable, c.usable, "storage classes select collection or SQL fallback")
			required, complete, err = semanticSearchCoverageTx(t.Context(), tx, profile.Fingerprint, record.BindingID, record.InputKind, record.VectorSpace.ID, normalized)
			require.NoError(t, err)
			filter, args := searchFilterSQL(normalized)
			population, err := countSemanticSearchPopulationTx(t.Context(), tx, filter, args)
			require.NoError(t, err)
			require.Equal(t, required, population)
			if c.usable {
				require.Len(t, c.completeNodes, complete)
			} else {
				require.Nil(t, c.completeNodes)
				require.Nil(t, c.annRows)
				require.Nil(t, c.seenANN)
			}
			rows, err = loadSemanticANNRows(t.Context(), tx, profile.Fingerprint, record.BindingID, record.InputKind, record.VectorSpace.ID, filter, args)
			require.NoError(t, err)
			sortOriginalSnapshotRows(rows)
			if c.usable {
				sortOriginalSnapshotRows(c.annRows)
				require.Equal(t, rows, c.annRows)
			}
			t.Logf("COLLECTOR_PARITY required=%d complete=%d rows=%d members=%d", required, complete, len(rows), len(old.Members))
		}
		return nil
	}))
	if !reusable {
		require.True(t, validated, "typed fixtures still complete every source validation")
		compareOriginalSnapshotFallback(t, s, profile, witness, normalized, source, required, complete, rows)
	}
}

func sortOriginalSnapshotRows(rows []vectorindex.RowIdentity) {
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].SetID != rows[j].SetID {
			return rows[i].SetID < rows[j].SetID
		}
		if rows[i].InputKey != rows[j].InputKey {
			return rows[i].InputKey < rows[j].InputKey
		}
		return rows[i].InputChecksum < rows[j].InputChecksum
	})
}

// Use real unaffected embedding evidence to exercise the final typed fallback,
// rather than taking the intentional no-semantic-evidence bypass.
func compareOriginalSnapshotFallback(t *testing.T, s *Store, p ProcessingProfileRecord, witness EmbeddingSetRecord, opts SearchOptions, wantSource VectorIndexSource, wantRequired, wantComplete int, wantRows []vectorindex.RowIdentity) {
	t.Helper()
	source, required, complete, rows, err := s.semanticSearchAuthorityFence(t.Context(), p.Fingerprint, witness.BindingID, document.EmbeddingInputOriginalFile, witness.VectorSpace.ID, opts, nil)
	require.NoError(t, err)
	require.Equal(t, wantSource, source)
	require.Equal(t, wantRequired, required)
	require.Equal(t, wantComplete, complete)
	require.Equal(t, wantRows, rows)
	neighbors := make([]vectorindex.Neighbor, len(rows))
	for i, row := range rows {
		neighbors[i] = vectorindex.Neighbor{SetID: row.SetID, InputKey: row.InputKey, InputChecksum: row.InputChecksum, Score: 1 - float64(i)/float64(len(rows)+1)}
	}
	for _, limit := range []int{1, 10} {
		// Reuse the independent full-SQL/legacy-reducer oracle from the
		// prefix tests: typed fallback must retain every ranked candidate,
		// including withheld lookahead, rather than only the TEXT witness.
		want := SemanticSearchResolution{SourceManifestChecksum: source.ManifestChecksum,
			ScopedDocuments: wantRequired, CompleteDocuments: wantComplete}
		require.NoError(t, s.withStorageTx(t.Context(), func(tx *sql.Tx) error {
			filterSQL, args := searchFilterSQL(opts)
			eligible, err := loadSemanticEligibility(t.Context(), tx, p.Fingerprint, witness.BindingID,
				document.EmbeddingInputOriginalFile, witness.VectorSpace.ID, filterSQL, args)
			require.NoError(t, err)
			want.Candidates, want.Truncated, want.NextNeighbor = reduceSemanticCandidates(s.vaultID,
				witness.VectorSpace.ID, neighbors, limit, eligible, nil)
			for i := range want.Candidates {
				candidate := &want.Candidates[i]
				candidate.Path, err = pathOf(t.Context(), tx, candidate.NodeID)
				require.NoError(t, err)
				candidate.Excerpt, err = searchExcerptByVersionTx(t.Context(), tx, candidate.ContentVersionID)
				require.NoError(t, err)
			}
			return nil
		}))
		resolved, err := s.ResolveSemanticCandidates(t.Context(), p.Fingerprint, witness.BindingID,
			document.EmbeddingInputOriginalFile, witness.VectorSpace.ID, source.ManifestChecksum, neighbors, limit, opts, nil)
		require.NoError(t, err)
		require.Equal(t, want, resolved, "typed fallback must match every legacy candidate and global lookahead at limit %d", limit)
	}
	requested := originalSnapshotEvidence(t, s, witness, source.ManifestChecksum)
	final, err := s.RevalidateSearchCandidates(t.Context(), requested, opts, p.Fingerprint, witness.BindingID)
	require.NoError(t, err)
	require.Equal(t, &SearchCoverageSnapshot{ScopedDocuments: wantRequired, CompleteDocuments: wantComplete}, final.Coverage)
	require.Len(t, final.Candidates, 1)
	require.Equal(t, requested[0], final.Candidates[0].SearchCandidateIdentity)
}

func TestOriginalSemanticSnapshotParity(t *testing.T) {
	t.Parallel()
	for _, mutation := range []string{"none", "checksum", "input checksum blob", "vector checksum blob", "both checksum blobs", "input id blob", "vector input id blob", "both input id blobs", "input id class collision", "shared original", "head set", "head binding", "head binding blob", "head kind blob", "head version blob", "set binding blob", "both binding blobs", "set kind blob", "head version", "head kind", "head profile", "head space", "trash", "input count", "vector manifest", "descriptor"} {
		t.Run(mutation, func(t *testing.T) {
			s, profile, records, _ := newSemanticPrefixFixture(t, document.EmbeddingInputOriginalFile, 3, false)
			first := records[0]
			require.NoError(t, s.withStorageTx(t.Context(), func(tx *sql.Tx) error {
				var err error
				switch mutation {
				case "vector checksum blob", "vector input id blob", "both checksum blobs", "both input id blobs":
					_, err = tx.Exec(`DROP TRIGGER embedding_vector_rows_immutable_update`)
					require.NoError(t, err)
					column := "checksum"
					if strings.Contains(mutation, "input id") {
						column = "input_id"
					}
					_, err = tx.Exec(`UPDATE embedding_vector_rows SET `+column+`=CAST(`+column+` AS BLOB) WHERE vector_set_id=?`, first.VectorSet.ID)
					require.NoError(t, err)
					if strings.HasPrefix(mutation, "both") {
						_, err = tx.Exec(`DROP TRIGGER embedding_generation_inputs_immutable_update`)
						require.NoError(t, err)
						column = "rendered_checksum"
						if strings.Contains(mutation, "input id") {
							column = "input_id"
						}
						_, err = tx.Exec(`UPDATE embedding_generation_inputs SET `+column+`=CAST(`+column+` AS BLOB) WHERE generation_id=?`, first.InputGeneration.ID)
					}
				case "input id blob":
					_, err = tx.Exec(`DROP TRIGGER embedding_generation_inputs_immutable_update`)
					require.NoError(t, err)
					_, err = tx.Exec(`UPDATE embedding_generation_inputs SET input_id=CAST(input_id AS BLOB) WHERE generation_id=?`, first.InputGeneration.ID)
				case "input id class collision":
					require.Len(t, first.InputGeneration.Inputs, 1)
					require.NotEqual(t, first.InputGeneration.Inputs[0].RenderedChecksum, first.VectorSet.PayloadChecksum)
					_, err = tx.Exec(`DROP TRIGGER embedding_input_generations_immutable_update`)
					require.NoError(t, err)
					_, err = tx.Exec(`UPDATE embedding_input_generations SET input_count=2 WHERE generation_id=?`, first.InputGeneration.ID)
					require.NoError(t, err)
					_, err = tx.Exec(`INSERT INTO embedding_generation_inputs(generation_id,input_id,input_order,rendered_checksum)
                        SELECT generation_id,CAST(input_id AS BLOB),input_order+1,? FROM embedding_generation_inputs WHERE generation_id=?`, first.VectorSet.PayloadChecksum, first.InputGeneration.ID)
				case "head kind blob":
					_, err = tx.Exec(`UPDATE embedding_heads SET input_kind=CAST(input_kind AS BLOB) WHERE embedding_set_id=?`, first.ID)
				case "head version blob":
					_, err = tx.Exec(`UPDATE embedding_heads SET content_version_id=CAST(content_version_id AS BLOB) WHERE embedding_set_id=?`, first.ID)
				case "set binding blob", "both binding blobs", "set kind blob":
					_, err = tx.Exec(`DROP TRIGGER embedding_sets_immutable_update`)
					require.NoError(t, err)
					column := "binding_id"
					if mutation == "set kind blob" {
						column = "input_kind"
					}
					_, err = tx.Exec(`UPDATE embedding_sets SET `+column+`=CAST(`+column+` AS BLOB) WHERE embedding_set_id=?`, first.ID)
					require.NoError(t, err)
					if mutation == "both binding blobs" {
						_, err = tx.Exec(`UPDATE embedding_heads SET binding_id=CAST(binding_id AS BLOB) WHERE embedding_set_id=?`, first.ID)
					}
				case "head set":
					_, err = tx.Exec(`UPDATE embedding_heads SET embedding_set_id=? WHERE embedding_set_id=?`, records[1].ID, first.ID)
				case "shared original":
					_, err = tx.Exec(`DROP TRIGGER embedding_sets_immutable_update`)
					require.NoError(t, err)
					_, err = tx.Exec(`UPDATE embedding_sets SET vector_set_id=?,input_generation_id=? WHERE embedding_set_id=?`, first.VectorSet.ID, first.InputGeneration.ID, records[1].ID)
				case "input checksum blob":
					_, err = tx.Exec(`DROP TRIGGER embedding_generation_inputs_immutable_update`)
					require.NoError(t, err)
					_, err = tx.Exec(`UPDATE embedding_generation_inputs SET rendered_checksum=CAST(rendered_checksum AS BLOB) WHERE generation_id=?`, first.InputGeneration.ID)
				case "checksum":
					_, err = tx.Exec(`DROP TRIGGER embedding_generation_inputs_immutable_update`)
					require.NoError(t, err)
					_, err = tx.Exec(`UPDATE embedding_generation_inputs SET rendered_checksum=? WHERE generation_id=?`, strings.Repeat("e", 64), first.InputGeneration.ID)
				case "head binding blob":
					_, err = tx.Exec(`UPDATE embedding_heads SET binding_id=CAST(binding_id AS BLOB) WHERE embedding_set_id=?`, first.ID)
				case "head binding":
					_, err = tx.Exec(`UPDATE embedding_heads SET binding_id='unrelated' WHERE embedding_set_id=?`, first.ID)
				case "head version":
					_, err = tx.Exec(`DELETE FROM embedding_heads WHERE embedding_set_id=?`, records[2].ID)
					require.NoError(t, err)
					_, err = tx.Exec(`UPDATE embedding_heads SET content_version_id=? WHERE embedding_set_id=?`, records[2].ContentVersionID, first.ID)
				case "head kind":
					_, err = tx.Exec(`UPDATE embedding_heads SET input_kind='rendition_chunk' WHERE embedding_set_id=?`, first.ID)
				case "head profile":
					// Retain a real stored profile; mismatch the loaded set's fingerprint.
					other := catalogProcessingProfile(t, false)
					require.NoError(t, ensureProcessingProfileTx(t.Context(), tx, other))
					_, err = tx.Exec(`UPDATE embedding_heads SET profile_fingerprint=? WHERE embedding_set_id=?`, other.Fingerprint, first.ID)
				case "head space":
					foreign, loadErr := loadVectorSpaceTx(t.Context(), tx, first.VectorSpace.ID)
					require.NoError(t, loadErr)
					foreign.ID = testSHA256([]byte("snapshot-real-foreign-space"))
					require.NoError(t, insertVectorSpaceTx(t.Context(), tx, foreign))
					_, err = tx.Exec(`UPDATE embedding_heads SET vector_space_id=? WHERE embedding_set_id=?`, foreign.ID, first.ID)
				case "trash":
					_, err = tx.Exec(`UPDATE nodes SET trashed_at=? WHERE current_version_id=?`, embeddingCatalogTime, first.ContentVersionID)
				case "input count":
					_, err = tx.Exec(`DROP TRIGGER embedding_input_generations_immutable_update`)
					require.NoError(t, err)
					_, err = tx.Exec(`UPDATE embedding_input_generations SET input_count=input_count+1 WHERE generation_id=?`, first.InputGeneration.ID)
				case "vector manifest":
					_, err = tx.Exec(`DROP TRIGGER embedding_vector_sets_immutable_update`)
					require.NoError(t, err)
					_, err = tx.Exec(`UPDATE embedding_vector_sets SET manifest_checksum=? WHERE vector_set_id=?`, strings.Repeat("e", 64), first.VectorSet.ID)
				case "descriptor":
					_, err = tx.Exec(`DROP TRIGGER embedding_vector_spaces_immutable_update`)
					require.NoError(t, err)
					_, err = tx.Exec(`UPDATE embedding_vector_spaces SET descriptor_json=? WHERE vector_space_id=?`, []byte(`{}`), first.VectorSpace.ID)
				}
				return err
			}))
			compareOriginalSnapshotSQL(t, s, profile, first, SearchOptions{}, !strings.Contains(mutation, "blob") && mutation != "input id class collision", records[1])
			compareOriginalSnapshotSQL(t, s, profile, first, SearchOptions{ContentVersionIDs: []string{records[1].ContentVersionID}}, !strings.Contains(mutation, "blob") && mutation != "input id class collision", records[1])
			compareOriginalSnapshotSQL(t, s, profile, first, SearchOptions{ContentVersionIDs: []string{records[2].ContentVersionID, records[1].ContentVersionID}}, !strings.Contains(mutation, "blob") && mutation != "input id class collision", records[1])
		})
	}
}

func originalSnapshotEvidence(t *testing.T, s *Store, r EmbeddingSetRecord, manifest string) []SearchCandidateIdentity {
	t.Helper()
	v, err := s.ContentVersionByID(t.Context(), r.ContentVersionID)
	require.NoError(t, err)
	return []SearchCandidateIdentity{{NodeID: v.NodeID, ContentVersionID: r.ContentVersionID, Evidence: []SearchEvidenceIdentity{{Kind: "embedding", VectorSpaceID: r.VectorSpace.ID, EmbeddingSetID: r.ID, InputGenerationID: r.InputGeneration.ID, InputID: r.InputGeneration.Inputs[0].ID, InputKind: r.InputKind, SourceManifestChecksum: manifest}}}}
}

type originalSnapshotCancelContext struct {
	context.Context

	cancel context.CancelFunc
	calls  int
}

func (c *originalSnapshotCancelContext) Err() error {
	c.calls++
	if c.calls == 3 {
		c.cancel()
	}
	return c.Context.Err()
}
func TestOriginalSemanticSnapshotCancellation(t *testing.T) {
	t.Parallel()
	s, p, records, _ := newSemanticPrefixFixture(t, document.EmbeddingInputOriginalFile, 2, false)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := s.RevalidateSearchCandidates(ctx, nil, SearchOptions{}, p.Fingerprint, records[0].BindingID)
	require.ErrorIs(t, err, context.Canceled)
	require.NoError(t, s.withStorageTx(t.Context(), func(tx *sql.Tx) error {
		heads, err := loadVectorIndexHeadCandidatesWithKey(t.Context(), tx, records[0].VectorSpace.ID, true)
		require.NoError(t, err)
		observed := false
		for _, head := range heads {
			if head.setID != records[0].ID {
				continue
			}
			observed = true
			record, err := loadEmbeddingSetTx(t.Context(), tx, head.setID)
			require.NoError(t, err)
			_, live, err := embeddingSetEligibilityTx(t.Context(), tx, record, true)
			require.NoError(t, err)
			child, stop := context.WithCancel(t.Context())
			defer stop()
			wrapped := &originalSnapshotCancelContext{Context: child, cancel: stop}
			c := newOriginalSemanticSnapshot(p.Fingerprint, record.BindingID, record.VectorSpace.ID, record.InputKind, SearchOptions{}, true)
			require.ErrorIs(t, c.observe(wrapped, head, record, live, true), context.Canceled)
		}
		require.True(t, observed, "actual source head reached cancellation observer")
		return nil
	}))
}

// A missed constructor guard would let original-only facts replace richer SQL.
func TestOriginalSemanticSnapshotFallbackGuards(t *testing.T) {
	t.Parallel()
	s, p, records, _ := newSemanticPrefixFixture(t, document.EmbeddingInputOriginalFile, 1, false)
	tag, err := s.CreateTag(t.Context(), "snapshot-guard")
	require.NoError(t, err)
	record := records[0]
	for _, opts := range []SearchOptions{{TagID: tag.ID}, {MIMEType: "text/plain"}, {UnderNodeID: s.RootID()}, {ModifiedSince: embeddingCatalogTime}, {ModifiedBefore: embeddingCatalogTime}, {SelectedBuilds: []SearchSelectedBuild{{ContentVersionID: record.ContentVersionID}}}} {
		require.Nil(t, newOriginalSemanticSnapshot(p.Fingerprint, record.BindingID, record.VectorSpace.ID, document.EmbeddingInputOriginalFile, opts, true))
	}
	for _, kind := range []document.EmbeddingInputKind{document.EmbeddingInputRenditionChunk, "unknown"} {
		require.Nil(t, newOriginalSemanticSnapshot(p.Fingerprint, record.BindingID, record.VectorSpace.ID, kind, SearchOptions{}, true))
	}
	require.NotNil(t, newOriginalSemanticSnapshot(p.Fingerprint, record.BindingID, record.VectorSpace.ID, document.EmbeddingInputOriginalFile, SearchOptions{ContentVersionIDs: []string{record.ContentVersionID}}, true))
}

// Source capture can fail before final coverage, but that earlier evaluation
// must preserve the old coverage > lexical-schema > source error priority.
func TestOriginalSemanticSnapshotFinalErrorPriorityAndEvidenceGate(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"missing source", "missing heads", "descriptor", "no evidence", "nonembedding evidence"} {
		t.Run(mode, func(t *testing.T) {
			s, p, records, _ := newSemanticPrefixFixture(t, document.EmbeddingInputOriginalFile, 2, false)
			source, err := s.CaptureVectorIndexSource(t.Context(), records[0].VectorSpace.ID)
			require.NoError(t, err)
			requested := originalSnapshotEvidence(t, s, records[0], source.ManifestChecksum)
			switch mode {
			case "missing source":
				_, err = s.db.Exec(`DELETE FROM embedding_heads`)
			case "missing heads":
				_, err = s.db.Exec(`DROP TABLE embedding_heads`)
			default:
				_, err = s.db.Exec(`DROP TRIGGER embedding_vector_spaces_immutable_update`)
				require.NoError(t, err)
				_, err = s.db.Exec(`UPDATE embedding_vector_spaces SET descriptor_json=? WHERE vector_space_id=?`, []byte(`{}`), records[0].VectorSpace.ID)
				if mode == "no evidence" {
					requested = nil
				}
				if mode == "nonembedding evidence" {
					node, loadErr := s.NodeByID(t.Context(), requested[0].NodeID)
					require.NoError(t, loadErr)
					requested[0].Evidence = []SearchEvidenceIdentity{{Kind: "node_name", NodeName: node.Name}}
				}
			}
			require.NoError(t, err)
			var required, complete int
			var coverageErr error
			require.NoError(t, s.withStorageTx(t.Context(), func(tx *sql.Tx) error {
				required, complete, coverageErr = semanticSearchCoverageTx(t.Context(), tx, p.Fingerprint, records[0].BindingID, document.EmbeddingInputOriginalFile, records[0].VectorSpace.ID, SearchOptions{})
				return nil
			}))
			_, sourceErr := s.CaptureVectorIndexSource(t.Context(), records[0].VectorSpace.ID)
			got, gotErr := s.RevalidateSearchCandidates(t.Context(), requested, SearchOptions{}, p.Fingerprint, records[0].BindingID)
			switch mode {
			case "missing heads":
				require.Error(t, coverageErr)
				originalSnapshotErrorParity(t, coverageErr, gotErr)
			case "missing source":
				require.NoError(t, coverageErr)
				require.ErrorIs(t, gotErr, ErrVectorIndexSourceStale)
			case "descriptor":
				require.NoError(t, coverageErr)
				require.Error(t, sourceErr)
				originalSnapshotErrorParity(t, sourceErr, gotErr)
			case "no evidence", "nonembedding evidence":
				require.Error(t, sourceErr, "fixture has invalid source that final must not capture")
				require.NoError(t, gotErr)
				require.Equal(t, &SearchCoverageSnapshot{ScopedDocuments: required, CompleteDocuments: complete}, got.Coverage)
				if mode == "nonembedding evidence" {
					require.Len(t, got.Candidates, 1, "real name evidence survives despite invalid unrelated source")
					require.Equal(t, requested[0], got.Candidates[0].SearchCandidateIdentity)
				}
			}
			if gotErr != nil {
				require.Zero(t, got, "no partial coverage or evidence may escape")
			}
		})
	}
}

func TestOriginalSemanticSnapshotSourceErrorMappings(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"empty source", "missing heads"} {
		t.Run(mode, func(t *testing.T) {
			s, p, records, neighbors := newSemanticPrefixFixture(t, document.EmbeddingInputOriginalFile, 2, false)
			source, err := s.CaptureVectorIndexSource(t.Context(), records[0].VectorSpace.ID)
			require.NoError(t, err)
			query := `DELETE FROM embedding_heads`
			if mode == "missing heads" {
				query = `DROP TABLE embedding_heads`
			}
			_, err = s.db.Exec(query)
			require.NoError(t, err)
			_, sourceErr := s.CaptureVectorIndexSource(t.Context(), records[0].VectorSpace.ID)
			require.Error(t, sourceErr)
			_, _, _, _, initialErr := s.semanticSearchAuthorityFence(t.Context(), p.Fingerprint, records[0].BindingID, document.EmbeddingInputOriginalFile, records[0].VectorSpace.ID, SearchOptions{}, nil)
			originalSnapshotErrorParity(t, sourceErr, initialErr)
			got, resolverErr := s.ResolveSemanticCandidates(t.Context(), p.Fingerprint, records[0].BindingID, document.EmbeddingInputOriginalFile, records[0].VectorSpace.ID, source.ManifestChecksum, neighbors, 2, SearchOptions{}, nil)
			require.Zero(t, got)
			if mode == "empty source" {
				require.ErrorIs(t, initialErr, ErrNotFound)
				require.ErrorIs(t, resolverErr, ErrVectorIndexSourceStale)
			} else {
				originalSnapshotErrorParity(t, sourceErr, resolverErr)
			}
		})
	}
}

// All three actual snapshots must report the independent SQL population and
// complete counts; initial identities must match the generic ANN predicate.
func TestOriginalSemanticSnapshotThreeSnapshotsMatchSQL(t *testing.T) {
	t.Parallel()
	s, p, records, neighbors := newSemanticPrefixFixture(t, document.EmbeddingInputOriginalFile, 3, false)
	for _, opts := range []SearchOptions{{}, {ContentVersionIDs: []string{records[2].ContentVersionID, records[0].ContentVersionID}}, {MIMEType: "application/pdf"}} {
		normalized, err := s.NormalizeSearchOptions(t.Context(), opts)
		require.NoError(t, err)
		var wantRequired, wantComplete int
		var wantRows []vectorindex.RowIdentity
		require.NoError(t, s.withStorageTx(t.Context(), func(tx *sql.Tx) error {
			wantRequired, wantComplete, err = semanticSearchCoverageTx(t.Context(), tx, p.Fingerprint, records[0].BindingID, document.EmbeddingInputOriginalFile, records[0].VectorSpace.ID, normalized)
			if err != nil {
				return err
			}
			filter, args := searchFilterSQL(normalized)
			wantRows, err = loadSemanticANNRows(t.Context(), tx, p.Fingerprint, records[0].BindingID, document.EmbeddingInputOriginalFile, records[0].VectorSpace.ID, filter, args)
			return err
		}))
		source, required, complete, rows, err := s.semanticSearchAuthorityFence(t.Context(), p.Fingerprint, records[0].BindingID, document.EmbeddingInputOriginalFile, records[0].VectorSpace.ID, normalized, nil)
		require.NoError(t, err)
		require.Equal(t, wantRequired, required)
		require.Equal(t, wantComplete, complete)
		require.ElementsMatch(t, wantRows, rows)
		resolved, err := s.ResolveSemanticCandidates(t.Context(), p.Fingerprint, records[0].BindingID, document.EmbeddingInputOriginalFile, records[0].VectorSpace.ID, source.ManifestChecksum, neighbors, 10, normalized, nil)
		require.NoError(t, err)
		require.Equal(t, wantRequired, resolved.ScopedDocuments)
		require.Equal(t, wantComplete, resolved.CompleteDocuments)
		requested := make([]SearchCandidateIdentity, 0, len(resolved.Candidates))
		for _, candidate := range resolved.Candidates {
			for _, record := range records {
				if candidate.EmbeddingSetID == record.ID {
					requested = append(requested, originalSnapshotEvidence(t, s, record, source.ManifestChecksum)...)
					break
				}
			}
		}
		require.Len(t, requested, len(resolved.Candidates))
		final, err := s.RevalidateSearchCandidates(t.Context(), requested, normalized, p.Fingerprint, records[0].BindingID)
		require.NoError(t, err)
		require.Equal(t, &SearchCoverageSnapshot{ScopedDocuments: wantRequired, CompleteDocuments: wantComplete}, final.Coverage)
		require.Len(t, final.Candidates, len(resolved.Candidates))
		for i, candidate := range final.Candidates {
			require.Equal(t, requested[i], candidate.SearchCandidateIdentity)
		}
	}
}

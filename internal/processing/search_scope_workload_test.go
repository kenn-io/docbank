package processing

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/store"
	"go.kenn.io/docbank/internal/vectorindex"
	docsqlite "go.kenn.io/docbank/sqlite"
	"uuid"
)

// Grow a real, validated worker publication into a synthetic workload. Copy
// catalog rows in one transaction; queries still use the real store, source
// manifest, exact generation and encoder. Fixture preparation is never timed.
func growSearchScopeWorkload(tb testing.TB, fixture publicationFixture, job store.EmbeddingJobRequest, documents int) []string {
	tb.Helper()
	db, err := store.DefaultSQLiteDriver().Open(fixture.databasePath, docsqlite.OpenOptions{Access: docsqlite.ReadWriteExisting, TransactionMode: docsqlite.Immediate})
	require.NoError(tb, err)
	defer func() { require.NoError(tb, db.Close()) }()
	tx, err := db.BeginTx(tb.Context(), nil)
	require.NoError(tb, err)
	defer func() { _ = tx.Rollback() }()
	// Discover each table once instead of maintaining a second catalog schema.
	columnsByTable := make(map[string][]string)
	discoverColumns := func(table string) []string {
		rows, err := tx.QueryContext(tb.Context(), "SELECT name FROM pragma_table_info(?) ORDER BY cid", table)
		require.NoError(tb, err)
		defer func() { require.NoError(tb, rows.Close()) }()
		var columns []string
		for rows.Next() {
			var name string
			require.NoError(tb, rows.Scan(&name))
			columns = append(columns, name)
		}
		require.NoError(tb, rows.Err())
		return columns
	}
	clone := func(table, where string, whereArgs []any, changes map[string]any) sql.Result {
		columns, ok := columnsByTable[table]
		if !ok {
			columns = discoverColumns(table)
			columnsByTable[table] = columns
		}
		var values []string
		var args []any
		for _, name := range columns {
			if value, ok := changes[name]; ok {
				values = append(values, "?")
				args = append(args, value)
			} else {
				values = append(values, name)
			}
		}
		args = append(args, whereArgs...)
		result, err := tx.ExecContext(tb.Context(), "INSERT INTO "+table+"("+strings.Join(columns, ",")+") SELECT "+strings.Join(values, ",")+" FROM "+table+" WHERE "+where, args...)
		require.NoError(tb, err)
		return result
	}
	// current_version_id has no index. Resolve once, then clone by primary key.
	var baseNodeID int64
	require.NoError(tb, tx.QueryRowContext(tb.Context(), "SELECT id FROM nodes WHERE current_version_id=?", job.ContentVersionID).Scan(&baseNodeID))
	var baseSet, baseVectors, baseAttachment string
	require.NoError(tb, tx.QueryRowContext(tb.Context(), "SELECT es.embedding_set_id,es.vector_set_id,COALESCE(eig.attachment_id,'') FROM embedding_sets es JOIN embedding_input_generations eig ON eig.generation_id=es.input_generation_id WHERE es.content_version_id=? AND es.binding_id=?", job.ContentVersionID, job.BindingID).Scan(&baseSet, &baseVectors, &baseAttachment))
	var source store.VectorIndexMember
	require.NoError(tb, tx.QueryRowContext(tb.Context(), "SELECT payload_blob_hash,payload_size FROM embedding_vector_sets WHERE vector_set_id=?", baseVectors).Scan(&source.PayloadBlobHash, &source.PayloadSize))
	source.EmbeddingSetID, source.VectorSetID = baseSet, baseVectors
	payload, err := fixture.catalog.ReadVectorIndexVectorSet(tb.Context(), fixture.blobs, source)
	require.NoError(tb, err)
	base, _, err := document.DecodeVectorSetV1(payload, document.VectorBounds{MaxRows: 100, MaxDimension: job.Descriptor.Dimension, MaxBytes: 1 << 20})
	require.NoError(tb, err)
	sets := make([]document.VectorSetV1, 0, documents)
	sets = append(sets, base)
	setIDs := []string{baseVectors}
	versions := []string{job.ContentVersionID}
	for i := 1; i < documents; i++ {
		version := uuid.New().String()
		versions = append(versions, version)
		result := clone("nodes", "id=?", []any{baseNodeID}, map[string]any{"id": nil, "name": fmt.Sprintf("synthetic-scope-%06d.bin", i), "current_version_id": version, "revision": 1})
		nodeID, err := result.LastInsertId()
		require.NoError(tb, err)
		clone("content_versions", "version_id=?", []any{job.ContentVersionID}, map[string]any{"version_id": version, "node_id": nodeID, "node_revision": 1, "introduced_operation_id": uuid.New().String(), "transition_kind": "content_create", "source_version_id": nil})
		generationID := workerHash("scope-generation-" + version)
		changes := map[string]any{"generation_id": generationID, "source_version_id": version}
		if baseAttachment != "" {
			attachment := workerHash("scope-attachment-" + version)
			generationID = workerHash("embedding-generation-attachment/v1\x00" + job.InputGeneration.GenerationChecksum + "\x00" + attachment)
			changes["generation_id"], changes["attachment_id"] = generationID, attachment
			clone("rendition_attachments", "attachment_id=?", []any{baseAttachment}, map[string]any{"attachment_id": attachment, "content_version_id": version})
			clone("rendition_heads", "attachment_id=?", []any{baseAttachment}, map[string]any{"attachment_id": attachment, "content_version_id": version})
		}
		clone("embedding_input_generations", "generation_id=?", []any{job.InputGeneration.ID}, changes)
		inputChanges := map[string]any{"generation_id": generationID}
		rowChanges := map[string]any{}
		vectorSet := base
		vectorSet.InputKeys = slices.Clone(base.InputKeys)
		vectorSet.Vectors = make([][]float32, len(base.Vectors))
		for row := range vectorSet.Vectors {
			vectorSet.Vectors[row] = slices.Clone(base.Vectors[row])
			vectorSet.Vectors[row][0] = 2 + float32(i)/float32(documents)
		}
		if job.InputGeneration.AttachmentID == "" {
			vectorSet.InputKeys[0] = version
			inputChanges["input_id"] = version
			rowChanges["row_id"], rowChanges["input_id"] = version, version
		}
		clone("embedding_generation_inputs", "generation_id=?", []any{job.InputGeneration.ID}, inputChanges)
		encoded, vectorID, err := document.EncodeVectorSetV1(vectorSet)
		require.NoError(tb, err)
		hash := processingSHA256(encoded)
		_, err = tx.ExecContext(tb.Context(), "INSERT INTO blobs(hash,size,created_at) VALUES(?,?,?)", hash, len(encoded), "2026-10-01T00:00:00.000000000Z")
		require.NoError(tb, err)
		var manifest strings.Builder
		for row, key := range vectorSet.InputKeys {
			fmt.Fprintf(&manifest, "%s\x00%s\x00%d\x00%s\n", key, key, vectorSet.Dimension, vectorSet.InputChecksums[row])
		}
		clone("embedding_vector_sets", "vector_set_id=?", []any{baseVectors}, map[string]any{"vector_set_id": vectorID, "payload_blob_hash": hash, "payload_size": len(encoded), "payload_checksum": vectorID, "manifest_checksum": processingSHA256([]byte(manifest.String()))})
		rowChanges["vector_set_id"] = vectorID
		clone("embedding_vector_rows", "vector_set_id=?", []any{baseVectors}, rowChanges)
		setID := workerHash("scope-set-" + version)
		clone("embedding_sets", "embedding_set_id=?", []any{baseSet}, map[string]any{"embedding_set_id": setID, "content_version_id": version, "input_generation_id": generationID, "vector_set_id": vectorID})
		clone("embedding_heads", "embedding_set_id=?", []any{baseSet}, map[string]any{"embedding_set_id": setID, "content_version_id": version})
		sets = append(sets, vectorSet)
		setIDs = append(setIDs, vectorID)
		if i%5000 == 0 {
			tb.Logf("prepared synthetic versions=%d/%d", i, documents)
		}
	}
	require.NoError(tb, tx.Commit())
	indexSource, err := fixture.catalog.CaptureVectorIndexSource(tb.Context(), base.VectorSpaceFingerprint)
	require.NoError(tb, err)
	require.Len(tb, indexSource.Members, documents)
	manifest, err := vectorindex.NewManifest(setIDs)
	require.NoError(tb, err)
	generation, err := vectorindex.BuildGeneration(manifest, sets, vectorindex.Options{})
	require.NoError(tb, err)
	record := store.VectorIndexGenerationRecord{VectorSpaceID: base.VectorSpaceFingerprint, SourceManifestChecksum: indexSource.ManifestChecksum, IndexManifestChecksum: manifest.Checksum, Bytes: generation.Bytes(), RowCount: generation.Metadata().RowCount, BuiltAt: time.Now().UTC().Format("2006-01-02T15:04:05.000000000Z")}
	record.ID = workerHash(indexSource.ManifestChecksum + manifest.Checksum)
	at := time.Now().UTC()
	claim, claimed, err := fixture.catalog.ClaimVectorIndexBuild(tb.Context(), record.VectorSpaceID, indexSource.ManifestChecksum, "scope-workload", at, 5*time.Minute)
	require.NoError(tb, err)
	require.True(tb, claimed)
	require.NoError(tb, fixture.catalog.StageVectorIndexGeneration(tb.Context(), claim, record, at))
	require.NoError(tb, fixture.catalog.PublishVectorIndexGeneration(tb.Context(), claim, record.ID, at))
	tb.Logf("synthetic versions=%d rows=%d dimensions=%d generation_bytes=%d", documents, record.RowCount, base.Dimension, len(record.Bytes))
	return versions
}

func searchScopeRequest(service *Service) SearchRequest {
	return SearchRequest{Query: "synthetic", Mode: "semantic", Profile: "private", Limit: 10, Fence: SourceFence{VaultUID: service.catalog.VaultID()}, Scope: &SearchScope{Kind: "all_live"}}
}

func TestSearchScopeBeyondExplicitFenceLimit(t *testing.T) {
	t.Parallel()
	fixture, service, _, job := newSearchScopeFixture(t)
	versions := growSearchScopeWorkload(t, fixture, job, store.MaxSearchSourceFenceIDs+1)
	report, err := service.Search(t.Context(), searchScopeRequest(service))
	require.NoError(t, err)
	require.Len(t, report.Results, 10)
	require.Equal(t, len(versions)+1, report.ObservedScopeCount)
	require.Equal(t, len(versions), report.Coverage.CompleteDocuments)
	explicit := searchScopeRequest(service)
	explicit.Scope = nil
	explicit.Fence.ContentVersionIDs = versions
	_, err = service.Search(t.Context(), explicit)
	require.Error(t, err)
}

func TestSearchScopeExcludesGlobalNearestBeforeRanking(t *testing.T) {
	t.Parallel()
	fixture, service, _, job := newSearchScopeFixture(t)
	versions := growSearchScopeWorkload(t, fixture, job, 4)
	tag, err := fixture.catalog.CreateTag(t.Context(), "selected-neighbors")
	require.NoError(t, err)
	for _, version := range versions[1:] {
		node, err := fixture.catalog.ContentVersionByID(t.Context(), version)
		require.NoError(t, err)
		current, err := fixture.catalog.NodeByID(t.Context(), node.NodeID)
		require.NoError(t, err)
		_, err = fixture.catalog.AssignTag(t.Context(), tag.ID, current.ID, current.Revision)
		require.NoError(t, err)
	}
	_, fingerprints, err := document.CanonicalProfile(service.profiles["private"].portable)
	require.NoError(t, err)
	spaceID := job.VectorSpaceID
	if spaceID == "" {
		spaceID = fingerprints.VectorSpace[job.BindingID]
	}
	authority, err := fixture.catalog.AcquireSemanticSearchAuthority(t.Context(), fixture.profile.Fingerprint,
		job.BindingID, "scope-before-scoring", time.Now().UTC(), time.Minute,
		store.SearchOptions{TagID: tag.ID, VectorSpaceID: spaceID, EmbeddingDescriptor: &job.Descriptor})
	require.NoError(t, err)
	defer func() {
		require.NoError(t, fixture.catalog.ReleaseVectorIndexGeneration(context.WithoutCancel(t.Context()),
			authority.Lease.ID, authority.Lease.FencingToken, time.Now().UTC()))
		t.Log("released actual tagged-scope reader lease before fixture cleanup")
	}()
	require.NotEmpty(t, authority.Lease.ID)
	require.Equal(t, spaceID, authority.VectorSpace.ID)
	// Read actual source rows before any scoring: output-only equality could
	// also pass if an implementation filtered the globally scored neighbors.
	source, err := fixture.catalog.CaptureVectorIndexSource(t.Context(), spaceID)
	require.NoError(t, err)
	var expected []vectorindex.RowIdentity
	var nearest vectorindex.RowIdentity
	func() {
		db, err := store.DefaultSQLiteDriver().Open(fixture.databasePath, docsqlite.OpenOptions{
			Access: docsqlite.ReadWriteExisting, TransactionMode: docsqlite.Deferred})
		require.NoError(t, err)
		defer func() { require.NoError(t, db.Close()) }()
		for _, member := range source.Members {
			func() {
				rows, err := db.QueryContext(t.Context(), `SELECT vector_set_id,input_id,checksum
 FROM embedding_vector_rows WHERE vector_set_id=?`, member.VectorSetID)
				require.NoError(t, err)
				defer func() { require.NoError(t, rows.Close()) }()
				for rows.Next() {
					var row vectorindex.RowIdentity
					require.NoError(t, rows.Scan(&row.SetID, &row.InputKey, &row.InputChecksum))
					if row.InputKey == versions[0] {
						nearest = row
					} else if slices.Contains(versions[1:], row.InputKey) {
						expected = append(expected, row)
					}
				}
				require.NoError(t, rows.Err())
			}()
		}
	}()
	require.NotZero(t, nearest)
	require.Len(t, expected, 3)
	require.ElementsMatch(t, expected, authority.ANNRows)
	require.NotContains(t, authority.ANNRows, nearest, "untagged global nearest is excluded before scoring")
	request := searchScopeRequest(service)
	global, err := service.Search(t.Context(), request)
	require.NoError(t, err)
	require.Equal(t, versions[0], global.Results[0].Document.ContentVersionID)
	request.Scope.Filters = &store.SearchOptions{TagID: tag.ID}
	scoped, err := service.Search(t.Context(), request)
	require.NoError(t, err)
	request.Scope = nil
	request.Fence.ContentVersionIDs = versions[1:]
	explicit, err := service.Search(t.Context(), request)
	require.NoError(t, err)
	require.Len(t, scoped.Results, 3)
	require.Equal(t, explicit.Results, scoped.Results)
	require.Equal(t, explicit.Coverage, scoped.Coverage)
}

func TestSearchScopeBenchmarkFixture(t *testing.T) {
	t.Parallel()
	for _, kind := range []document.EmbeddingInputKind{document.EmbeddingInputOriginalFile, document.EmbeddingInputRenditionChunk} {
		t.Run(string(kind), func(t *testing.T) {
			fixture, service, _, job := newSearchScopeFixtureDimension(t, kind, 1024)
			growSearchScopeWorkload(t, fixture, job, 4)
			report, err := service.Search(t.Context(), searchScopeRequest(service))
			require.NoError(t, err)
			require.Len(t, report.Results, 4)
			require.Equal(t, 4, report.Coverage.CompleteDocuments)
		})
	}
}

package store

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
)

// A source-ID fence, a 4096 collection ceiling, and an unbounded lookup would
// each break this independently authored namespace. Every source has the same
// basename and alias, but a different exact binding.
func TestNoteIndexKeyset4097(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	_, notes := noteIndexFixture(t, s, indexNoteSpec{ref: "0000/note.md", body: "---\naliases: [shared]\n---\nnote", keys: []NoteLookupKey{{"path", "0000/note.md"}, {"basename", "note"}, {"alias", "shared"}}})
	seed := notes[0]
	tx, err := s.db.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	for i := 1; i < 4097; i++ {
		version := noteOperationID(t)
		id := seed.source.Identity.NodeID + int64(i)
		name, ref := fmt.Sprintf("note-%04d.md", i), fmt.Sprintf("%04d/note.md", i)
		_, err = tx.ExecContext(t.Context(), `INSERT INTO nodes(id,parent_id,name,kind,current_version_id,revision,created_at,modified_at) SELECT $1,parent_id,$2,'file',$3,1,created_at,modified_at FROM nodes WHERE id=$4`, id, name, version, seed.source.Identity.NodeID)
		require.NoError(t, err)
		_, err = tx.ExecContext(t.Context(), `INSERT INTO content_versions(version_id,node_id,blob_hash,size,mime_type,recorded_at,node_revision,introduced_operation_id,transition_kind) SELECT $1,$2,blob_hash,size,mime_type,recorded_at,1,$3,'content_create' FROM content_versions WHERE version_id=$4`, version, id, noteOperationID(t), seed.source.Identity.ContentVersionID)
		require.NoError(t, err)
		_, err = tx.ExecContext(t.Context(), `INSERT INTO published_note_sources(publication_name,source_ref,node_id,content_version_id,blob_hash,size,source_revision,presence,availability,accepted_at) SELECT publication_name,$1,$2,$3,blob_hash,size,1,'present','retained',accepted_at FROM published_note_sources WHERE source_ref=$4`, ref, id, version, seed.source.Identity.SourceRef)
		require.NoError(t, err)
		_, err = tx.ExecContext(t.Context(), `INSERT INTO note_parse_bindings(content_version_id,recipe,generation_id) SELECT $1,recipe,generation_id FROM note_parse_bindings WHERE content_version_id=$2`, version, seed.parse.ContentVersionID)
		require.NoError(t, err)
	}
	require.NoError(t, tx.Commit())
	g := beginIndexFixture(t, s)
	require.EqualValues(t, 4097, g.ExpectedSources)
	for pass := 0; pass < 2; pass++ {
		after, total := "", 0
		for {
			page, err := s.EligibleNoteSources(t.Context(), "notebook", after, 64)
			require.NoError(t, err)
			require.LessOrEqual(t, len(page), 64)
			if len(page) == 0 {
				break
			}
			for _, source := range page {
				if pass == 0 {
					record := seed.parse
					record.ContentVersionID = source.Identity.ContentVersionID
					require.NoError(t, s.StageNoteIndexSource(t.Context(), g, source, record, []NoteLookupKey{{"path", source.Identity.SourceRef}, {"basename", "note"}, {"alias", "shared"}}))
				} else {
					require.NoError(t, s.StageNoteIndexEdges(t.Context(), g, source.Identity.SourceRef, nil))
				}
			}
			total += len(page)
			after = page[len(page)-1].Identity.SourceRef
		}
		require.Equal(t, 4097, total)
		if pass == 0 {
			require.NoError(t, s.SealNoteIndexKeys(t.Context(), g))
		}
	}
	require.NoError(t, s.PublishNoteIndex(t.Context(), g))
	rows, err := s.NoteIndexCandidates(t.Context(), g, []NoteLookupKey{{"basename", "note"}, {"alias", "shared"}})
	require.NoError(t, err)
	require.Len(t, rows, 2)
	require.Equal(t, "0000/note.md", rows[0].SourceRef)
	require.Equal(t, "0001/note.md", rows[1].SourceRef)
	for _, row := range rows {
		require.Len(t, row.Keys, 2)
	}
	var staged int
	require.NoError(t, s.db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM note_index_sources WHERE generation_id=$1 AND edges_done=1`, g.ID).Scan(&staged))
	require.Equal(t, 4097, staged)
}

package store

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCollectionLabelMetadataRoundTripsNullAndNonNull(t *testing.T) {
	source := newTestStore(t)
	named := createCollectionRun(t, source, "named.txt", "a1")
	cleared := createCollectionRun(t, source, "cleared.txt", "b2")
	name := "Portable label"
	_, err := source.SetCollectionLabel(t.Context(), named.ID(), 1, &name)
	require.NoError(t, err)
	set, err := source.SetCollectionLabel(t.Context(), cleared.ID(), 1, new("Temporary"))
	require.NoError(t, err)
	_, err = source.SetCollectionLabel(t.Context(), cleared.ID(), set.Revision, nil)
	require.NoError(t, err)

	var exported bytes.Buffer
	require.NoError(t, source.ExportMetadata(t.Context(), &exported))
	assert.Contains(t, exported.String(), `{"type":"collection_label","ingest_id":"`+named.ID()+`","label":"Portable label","revision":2,`)
	assert.Contains(t, exported.String(), `{"type":"collection_label","ingest_id":"`+cleared.ID()+`","label":null,"revision":3,`)

	target := newTestStore(t)
	require.NoError(t, target.ImportMetadata(t.Context(), bytes.NewReader(exported.Bytes())))
	namedLabel, err := target.CollectionLabel(t.Context(), named.ID())
	require.NoError(t, err)
	require.Equal(t, name, *namedLabel.Label)
	clearedLabel, err := target.CollectionLabel(t.Context(), cleared.ID())
	require.NoError(t, err)
	assert.Nil(t, clearedLabel.Label)
	assert.Equal(t, int64(3), clearedLabel.Revision)
	var restored bytes.Buffer
	require.NoError(t, target.ExportMetadata(t.Context(), &restored))
	assert.Equal(t, exported.Bytes(), restored.Bytes())
}

func TestCollectionLabelMetadataRoundTripsAfterFinalMemberPurge(t *testing.T) {
	source := newTestStore(t)
	named := createCollectionRun(t, source, "named-purge.txt", "c1")
	cleared := createCollectionRun(t, source, "cleared-purge.txt", "d2")
	namedLabel, err := source.SetCollectionLabel(
		t.Context(), named.ID(), 1, new("Retained after purge"),
	)
	require.NoError(t, err)
	temporary, err := source.SetCollectionLabel(
		t.Context(), cleared.ID(), 1, new("Clear before purge"),
	)
	require.NoError(t, err)
	clearedLabel, err := source.SetCollectionLabel(
		t.Context(), cleared.ID(), temporary.Revision, nil,
	)
	require.NoError(t, err)

	for _, ingestID := range []string{named.ID(), cleared.ID()} {
		page, pageErr := source.CollectionMembers(t.Context(), ingestID, 10, 0)
		require.NoError(t, pageErr)
		require.Len(t, page.Items, 1)
		_, _, trashErr := source.Trash(
			t.Context(), page.Items[0].Node.ID, UnconditionalRev,
		)
		require.NoError(t, trashErr)
	}
	emptied, err := source.TrashEmpty(t.Context(), 0, true)
	require.NoError(t, err)
	require.Equal(t, int64(2), emptied.Deleted)
	var provenance int
	require.NoError(t, source.db.QueryRow(`SELECT COUNT(*) FROM provenance
		WHERE ingest_id IN (?,?)`, named.ID(), cleared.ID()).Scan(&provenance))
	require.Zero(t, provenance)
	require.Equal(t, namedLabel, mustCollectionLabel(t, source, named.ID()))
	require.Equal(t, clearedLabel, mustCollectionLabel(t, source, cleared.ID()))

	var exported bytes.Buffer
	require.NoError(t, source.ExportMetadata(t.Context(), &exported))
	require.Contains(t, exported.String(),
		`"type":"collection_label","ingest_id":"`+named.ID()+`"`)
	require.Contains(t, exported.String(),
		`"type":"collection_label","ingest_id":"`+cleared.ID()+`"`)
	var backup bytes.Buffer
	snapshot, err := source.BeginMetadataSnapshot(t.Context())
	require.NoError(t, err)
	require.NoError(t, snapshot.ExportBackup(t.Context(), &backup))
	require.NoError(t, snapshot.Close())
	require.Contains(t, backup.String(),
		`"type":"collection_label","ingest_id":"`+named.ID()+`"`)
	require.Contains(t, backup.String(),
		`"type":"collection_label","ingest_id":"`+cleared.ID()+`"`)
	target := newTestStore(t)
	require.NoError(t, target.ImportMetadata(t.Context(), bytes.NewReader(backup.Bytes())))
	require.Equal(t, namedLabel, mustCollectionLabel(t, target, named.ID()))
	require.Equal(t, clearedLabel, mustCollectionLabel(t, target, cleared.ID()))
	collections, total, err := target.Collections(t.Context(), 100, 0)
	require.NoError(t, err)
	require.Empty(t, collections)
	require.Zero(t, total)
}

func TestCollectionLabelMetadataRejectsMalformedAndCollidingRecordsAtomically(t *testing.T) {
	source := newTestStore(t)
	first := createCollectionRun(t, source, "first.txt", "a1")
	second := createCollectionRun(t, source, "second.txt", "b2")
	label := "First label"
	_, err := source.SetCollectionLabel(t.Context(), first.ID(), 1, &label)
	require.NoError(t, err)
	other := "Second label"
	_, err = source.SetCollectionLabel(t.Context(), second.ID(), 1, &other)
	require.NoError(t, err)
	var exported bytes.Buffer
	require.NoError(t, source.ExportMetadata(t.Context(), &exported))

	tests := []struct {
		name   string
		mutate func(string) string
	}{
		{name: "noncanonical NFC", mutate: func(input string) string {
			return strings.Replace(input, `"label":"First label"`, `"label":"Cafe\u0301"`, 1)
		}},
		{name: "zero revision", mutate: func(input string) string {
			return strings.Replace(input, `"label":"First label","revision":2`, `"label":"First label","revision":0`, 1)
		}},
		{name: "invalid time", mutate: func(input string) string {
			return strings.Replace(input, `"updated_at":"`, `"updated_at":"invalid`, 1)
		}},
		{name: "label collision", mutate: func(input string) string {
			return strings.Replace(input, `"label":"Second label"`, `"label":"First label"`, 1)
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			target := newTestStore(t)
			err := target.ImportMetadata(t.Context(), strings.NewReader(test.mutate(exported.String())))
			require.Error(t, err)
			var ingests, labels int
			require.NoError(t, target.db.QueryRow(`SELECT
				(SELECT COUNT(*) FROM ingests), (SELECT COUNT(*) FROM collection_labels)`).Scan(&ingests, &labels))
			assert.Zero(t, ingests)
			assert.Zero(t, labels)
		})
	}
}

func TestCollectionLabelMetadataRejectsMaterializedVirtualState(t *testing.T) {
	source := newTestStore(t)
	run := createCollectionRun(t, source, "note.txt", "a1")
	_, err := source.SetCollectionLabel(t.Context(), run.ID(), 1, new("Temporary"))
	require.NoError(t, err)
	var exported bytes.Buffer
	require.NoError(t, source.ExportMetadata(t.Context(), &exported))
	malformed := strings.Replace(exported.String(),
		`"label":"Temporary","revision":2`, `"label":null,"revision":1`, 1)
	require.NotEqual(t, exported.String(), malformed)

	target := newTestStore(t)
	err = target.ImportMetadata(t.Context(), strings.NewReader(malformed))
	require.Error(t, err)
	var labels int
	require.NoError(t, target.db.QueryRow(`SELECT COUNT(*) FROM collection_labels`).Scan(&labels))
	assert.Zero(t, labels)
}

func TestReleasedMetadataWithoutCollectionLabelsImports(t *testing.T) {
	source := newTestStore(t)
	createCollectionRun(t, source, "note.txt", "a1")
	var exported bytes.Buffer
	require.NoError(t, source.ExportMetadata(t.Context(), &exported))
	lines := bytes.Split(bytes.TrimSpace(exported.Bytes()), []byte{'\n'})
	filtered := make([][]byte, 0, len(lines))
	for _, line := range lines {
		if !bytes.Contains(line, []byte(`"type":"collection_label"`)) {
			filtered = append(filtered, line)
		}
	}
	legacy := append(bytes.Join(filtered, []byte{'\n'}), '\n')
	target, err := Open(filepath.Join(t.TempDir(), "legacy-target.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, target.Close()) })
	require.NoError(t, target.ImportMetadata(t.Context(), bytes.NewReader(legacy)))
	var labels int
	require.NoError(t, target.db.QueryRow(`SELECT COUNT(*) FROM collection_labels`).Scan(&labels))
	assert.Zero(t, labels)
}

func TestVersion7MetadataPreservesSavedQueriesWithoutCollectionLabels(t *testing.T) {
	source := newTestStore(t)
	ctx := t.Context()
	query, err := source.CreateSavedQuery(ctx, "Synthetic query", "", SavedQueryKindQuery,
		[]byte(`{"text":"example"}`))
	require.NoError(t, err)
	_, err = source.db.Exec(`DROP TABLE collection_labels`)
	require.NoError(t, err)
	_, err = source.db.Exec(`UPDATE vault_metadata SET schema_version=7 WHERE singleton=1`)
	require.NoError(t, err)

	var exported bytes.Buffer
	require.NoError(t, exportMetadataSnapshotWithVaultIdentity(ctx, source.db, &exported,
		metadataSourceLayout{schemaVersion: 7}))
	target := newTestStore(t)
	require.NoError(t, target.ImportMetadata(ctx, bytes.NewReader(exported.Bytes())))
	restored, err := target.SavedQueryByID(ctx, query.ID)
	require.NoError(t, err)
	assert.Equal(t, query, restored)
}

func mustCollectionLabel(t *testing.T, s *Store, ingestID string) CollectionLabel {
	t.Helper()
	label, err := s.CollectionLabel(t.Context(), ingestID)
	require.NoError(t, err)
	return label
}

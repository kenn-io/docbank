package store

import (
	"bytes"
	"encoding/json/v2"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/document"
)

func TestContentMapMetadataBackupRoundTripKeepsFrozenHistory(t *testing.T) {
	for _, driver := range v090UpgradeDrivers() {
		t.Run(driver.name, func(t *testing.T) {
			source := newTestStoreWithDriver(t, driver.driver)
			ctx := t.Context()
			node, err := source.CreateFile(ctx, source.RootID(), "synthetic-paper.txt",
				fakeHash("a1"), 14, "text/plain")
			require.NoError(t, err)
			identity, err := source.EnsureDocumentIdentity(ctx, node.ID)
			require.NoError(t, err)
			hiddenNode, err := source.CreateFile(ctx, source.RootID(), "synthetic-background.txt",
				fakeHash("b2"), 10, "text/plain")
			require.NoError(t, err)
			hiddenIdentity, err := source.EnsureDocumentIdentity(ctx, hiddenNode.ID)
			require.NoError(t, err)
			hiddenPin := document.ContentMapPin{DocumentUID: hiddenIdentity.DocumentUID,
				Mode: document.MapPinVersionPinned, ContentVersionID: hiddenNode.CurrentVersionID}
			access := MapAccess{Owner: "local", AllSources: true}
			definition := ContentMapDefinition{Title: "Synthetic research", Scope: "local",
				Sections: []ContentMapSection{{ID: "core", Heading: "Core", Description: "Source summary",
					HeadingSources:     []document.ContentMapPin{hiddenPin},
					DescriptionSources: []document.ContentMapPin{hiddenPin}, Exclude: []document.ContentMapPin{hiddenPin},
					Include: []document.ContentMapPin{{DocumentUID: identity.DocumentUID,
						Mode: document.MapPinVersionPinned, ContentVersionID: node.CurrentVersionID}},
					Ordering: "explicit", MaxEntries: 1}}}
			plan, err := source.PreviewContentMap(ctx, access, definition)
			require.NoError(t, err)
			created, err := source.CreateContentMap(ctx, access, definition, plan.DefinitionDigest)
			require.NoError(t, err)
			frozen, err := source.CreateContentMapSnapshot(ctx, access, created.ID, created.Revision)
			require.NoError(t, err)
			public, err := json.Marshal(frozen)
			require.NoError(t, err)
			assert.NotContains(t, string(public), hiddenIdentity.DocumentUID)
			archived, err := source.ArchiveContentMap(ctx, access, created.ID, created.Revision)
			require.NoError(t, err)

			pinned, err := source.BeginMetadataSnapshot(ctx)
			require.NoError(t, err)
			var exported bytes.Buffer
			require.NoError(t, pinned.ExportBackup(ctx, &exported))
			require.NoError(t, pinned.Close())
			assert.Contains(t, exported.String(), `"type":"content_map"`)
			assert.Contains(t, exported.String(), `"type":"content_map_snapshot"`)

			target := newTestStoreWithDriver(t, driver.driver)
			require.NoError(t, target.ImportMetadata(ctx, bytes.NewReader(exported.Bytes())))
			restoredMap, err := target.ContentMapByID(ctx, access, created.ID)
			require.NoError(t, err)
			assert.Equal(t, archived, restoredMap)
			restoredSnapshot, err := target.ContentMapSnapshotByID(ctx, access, frozen.ID)
			require.NoError(t, err)
			assert.Equal(t, frozen, restoredSnapshot)
			var again bytes.Buffer
			require.NoError(t, target.ExportMetadata(ctx, &again))
			assert.Equal(t, exported.Bytes(), again.Bytes())

			_, err = target.db.ExecContext(ctx,
				`UPDATE content_map_snapshots SET member_hash=? WHERE id=?`,
				strings.Repeat("0", 64), frozen.ID)
			require.ErrorContains(t, err, "immutable")
			_, err = target.db.ExecContext(ctx, `DELETE FROM content_map_snapshots WHERE id=?`, frozen.ID)
			require.ErrorContains(t, err, "immutable")
		})
	}
}

func TestContentMapMetadataRejectsTamperedSnapshotTransactionally(t *testing.T) {
	source := newTestStore(t)
	ctx := t.Context()
	node, err := source.CreateFile(ctx, source.RootID(), "synthetic.txt",
		fakeHash("a2"), 9, "text/plain")
	require.NoError(t, err)
	identity, err := source.EnsureDocumentIdentity(ctx, node.ID)
	require.NoError(t, err)
	access := MapAccess{Owner: "local", AllSources: true}
	definition := ContentMapDefinition{Title: "Evidence", Scope: "local",
		Sections: []ContentMapSection{{ID: "s1", Heading: "Section",
			Include: []document.ContentMapPin{{DocumentUID: identity.DocumentUID,
				Mode: document.MapPinFollowCurrent}},
			Ordering: "explicit", MaxEntries: 1}}}
	plan, err := source.PreviewContentMap(ctx, access, definition)
	require.NoError(t, err)
	created, err := source.CreateContentMap(ctx, access, definition, plan.DefinitionDigest)
	require.NoError(t, err)
	_, err = source.CreateContentMapSnapshot(ctx, access, created.ID, created.Revision)
	require.NoError(t, err)
	var exported bytes.Buffer
	require.NoError(t, source.ExportMetadata(ctx, &exported))

	lines := strings.Split(strings.TrimSuffix(exported.String(), "\n"), "\n")
	mutated := false
	for index, line := range lines {
		if !strings.Contains(line, `"type":"content_map_snapshot"`) {
			continue
		}
		var record metadataContentMapSnapshot
		require.NoError(t, json.Unmarshal([]byte(line), &record))
		record.MemberHash = strings.Repeat("0", 64)
		raw, marshalErr := json.Marshal(record)
		require.NoError(t, marshalErr)
		lines[index] = string(raw)
		mutated = true
	}
	require.True(t, mutated)
	target := newTestStore(t)
	err = target.ImportMetadata(ctx, strings.NewReader(strings.Join(lines, "\n")+"\n"))
	require.ErrorContains(t, err, "content map snapshot")
	var count int
	require.NoError(t, target.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM content_maps`).Scan(&count))
	assert.Zero(t, count, "failed import must roll back definition and snapshot together")
}

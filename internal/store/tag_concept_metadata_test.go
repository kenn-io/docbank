package store

import (
	"bytes"
	"encoding/json/v2"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/document"
)

func TestTagConceptMetadataAndBackupRoundTrip(t *testing.T) {
	ctx := t.Context()
	source, err := Open(filepath.Join(t.TempDir(), "source.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, source.Close()) })
	parent, err := source.CreateTag(ctx, "Parent")
	require.NoError(t, err)
	child, err := source.CreateTag(ctx, "Child")
	require.NoError(t, err)
	sourceID, err := newUUIDv4()
	require.NoError(t, err)
	mergeID, err := newUUIDv4()
	require.NoError(t, err)
	reversedMergeID, err := newUUIDv4()
	require.NoError(t, err)
	reversedSourceID, err := newUUIDv4()
	require.NoError(t, err)
	documentUID, err := newUUIDv4()
	require.NoError(t, err)
	versionID, err := newUUIDv4()
	require.NoError(t, err)
	vaultID, err := readVaultIdentity(ctx, source.db, false)
	require.NoError(t, err)
	ref := document.PassageRefV1{
		Version: document.PassageRefVersionV1, VaultUID: vaultID,
		DocumentUID: documentUID, ContentVersionID: versionID,
		SourceSHA256: strings.Repeat("a", 64), RenditionBuildID: strings.Repeat("b", 64),
		AttachmentID: strings.Repeat("c", 64), BodySHA256: strings.Repeat("d", 64),
		ByteStart: 0, ByteEnd: 5, QuoteSHA256: strings.Repeat("e", 64),
	}
	passageID, err := document.PassageIdentityV1(ref)
	require.NoError(t, err)
	refJSON, err := json.Marshal(ref)
	require.NoError(t, err)
	previewJSON, err := json.Marshal(TagMergePreview{
		SourceTagID: sourceID, TargetTagID: child.ID, SourceName: "Old child",
		TargetName: child.Name, SourceRevision: 1, TargetRevision: 2,
		SourceConceptRev: 1, TargetConceptRev: 1,
	})
	require.NoError(t, err)
	reversedPreviewJSON, err := json.Marshal(TagMergePreview{
		SourceTagID: reversedSourceID, TargetTagID: parent.ID, SourceName: "Old parent",
		TargetName: parent.Name, SourceRevision: 2, TargetRevision: 3,
		SourceConceptRev: 1, TargetConceptRev: 1,
	})
	require.NoError(t, err)
	for _, insert := range []struct {
		query string
		args  []any
	}{
		{`INSERT INTO tag_concepts(tag_id,description,revision) VALUES(?,?,?)`, []any{parent.ID, "Top level", 3}},
		{`INSERT INTO tag_aliases(alias,tag_id) VALUES(?,?)`, []any{"Ancestor", parent.ID}},
		{`INSERT INTO tag_concept_edges(parent_tag_id,child_tag_id,kind) VALUES(?,?,?)`, []any{parent.ID, child.ID, "broader"}},
		{`INSERT INTO tag_redirects(source_tag_id,target_tag_id,source_name,merged_at,merge_id) VALUES(?,?,?,?,?)`, []any{sourceID, child.ID, "Old child", "2026-09-23T00:00:00.000000000Z", mergeID}},
		{`INSERT INTO tag_merge_audit(merge_id,source_tag_id,target_tag_id,source_revision,target_revision,preview_json,committed_at,reversed_at) VALUES(?,?,?,?,?,?,?,?)`, []any{mergeID, sourceID, child.ID, 1, 2, previewJSON, "2026-09-23T00:00:00.000000000Z", nil}},
		{`INSERT INTO tag_merge_audit(merge_id,source_tag_id,target_tag_id,source_revision,target_revision,preview_json,committed_at,reversed_at) VALUES(?,?,?,?,?,?,?,?)`, []any{reversedMergeID, reversedSourceID, parent.ID, 2, 3, reversedPreviewJSON, "2026-09-22T00:00:00.000000000Z", "2026-09-23T00:00:00.000000000Z"}},
		{`INSERT INTO passage_tags(passage_id,tag_id,ref_json,document_uid,content_version_id) VALUES(?,?,?,?,?)`, []any{passageID, child.ID, refJSON, documentUID, versionID}},
	} {
		_, err := source.db.ExecContext(ctx, insert.query, insert.args...)
		require.NoError(t, err)
	}
	var exported bytes.Buffer
	require.NoError(t, source.ExportMetadata(ctx, &exported))
	for _, kind := range []string{metadataTagConceptType, metadataTagAliasType,
		metadataTagConceptEdgeType, metadataTagRedirectType, metadataTagMergeAuditType,
		metadataPassageTagType} {
		require.Contains(t, exported.String(), `"type":"`+kind+`"`)
	}
	target, err := Open(filepath.Join(t.TempDir(), "target.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, target.Close()) })
	require.NoError(t, target.ImportMetadata(ctx, bytes.NewReader(exported.Bytes())))
	var restored bytes.Buffer
	require.NoError(t, target.ExportMetadata(ctx, &restored))
	require.Equal(t, exported.Bytes(), restored.Bytes())

	snapshot, err := source.BeginMetadataSnapshot(ctx)
	require.NoError(t, err)
	var backup bytes.Buffer
	require.NoError(t, snapshot.ExportBackup(ctx, &backup))
	require.NoError(t, snapshot.Close())
	require.Equal(t, exported.Bytes(), backup.Bytes())
	backupTarget, err := Open(filepath.Join(t.TempDir(), "backup.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, backupTarget.Close()) })
	require.NoError(t, backupTarget.ImportMetadata(ctx, bytes.NewReader(backup.Bytes())))
	var restoredBackup bytes.Buffer
	require.NoError(t, backupTarget.ExportMetadata(ctx, &restoredBackup))
	require.Equal(t, backup.Bytes(), restoredBackup.Bytes())
}

func TestTagConceptMetadataRejectsHierarchyCycle(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "cycle.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	first, err := s.CreateTag(t.Context(), "First")
	require.NoError(t, err)
	second, err := s.CreateTag(t.Context(), "Second")
	require.NoError(t, err)
	for _, pair := range [][2]string{{first.ID, second.ID}, {second.ID, first.ID}} {
		_, err := s.db.ExecContext(t.Context(), `INSERT INTO tag_concept_edges(parent_tag_id,child_tag_id,kind) VALUES(?,?,'broader')`, pair[0], pair[1])
		require.NoError(t, err)
	}
	var exported bytes.Buffer
	require.ErrorContains(t, s.ExportMetadata(t.Context(), &exported), "cycle")
}

func TestTagConceptMetadataRejectsMalformedMergePreview(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "audit.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	target, err := s.CreateTag(t.Context(), "Target")
	require.NoError(t, err)
	sourceID, err := newUUIDv4()
	require.NoError(t, err)
	mergeID, err := newUUIDv4()
	require.NoError(t, err)
	_, err = s.db.ExecContext(t.Context(), `INSERT INTO tag_merge_audit(merge_id,source_tag_id,target_tag_id,source_revision,target_revision,preview_json,committed_at) VALUES(?,?,?,?,?,?,?)`,
		mergeID, sourceID, target.ID, 1, 1, []byte(`{}`), "2026-09-23T00:00:00.000000000Z")
	require.NoError(t, err)
	var exported bytes.Buffer
	require.ErrorContains(t, s.ExportMetadata(t.Context(), &exported), "preview")
}

func TestTagConceptMetadataRejectsOrphanRedirect(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "redirect.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	target, err := s.CreateTag(t.Context(), "Target")
	require.NoError(t, err)
	sourceID, err := newUUIDv4()
	require.NoError(t, err)
	mergeID, err := newUUIDv4()
	require.NoError(t, err)
	_, err = s.db.ExecContext(t.Context(), `INSERT INTO tag_redirects(source_tag_id,target_tag_id,source_name,merged_at,merge_id) VALUES(?,?,?,?,?)`,
		sourceID, target.ID, "Old target", "2026-09-23T00:00:00.000000000Z", mergeID)
	require.NoError(t, err)
	var exported bytes.Buffer
	require.ErrorContains(t, s.ExportMetadata(t.Context(), &exported), "redirect")
}

func TestTagConceptMergeBackupRestoresRedirect(t *testing.T) {
	ctx := t.Context()
	source, err := Open(filepath.Join(t.TempDir(), "merge-source.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, source.Close()) })
	from, err := source.CreateTag(ctx, "Old topic")
	require.NoError(t, err)
	to, err := source.CreateTag(ctx, "New topic")
	require.NoError(t, err)
	preview, err := source.PreviewTagMerge(ctx, from.ID, to.ID)
	require.NoError(t, err)
	receipt, err := source.CommitTagMerge(ctx, preview)
	require.NoError(t, err)

	snapshot, err := source.BeginMetadataSnapshot(ctx)
	require.NoError(t, err)
	var backup bytes.Buffer
	require.NoError(t, snapshot.ExportBackup(ctx, &backup))
	require.NoError(t, snapshot.Close())
	target, err := Open(filepath.Join(t.TempDir(), "merge-target.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, target.Close()) })
	require.NoError(t, target.ImportMetadata(ctx, bytes.NewReader(backup.Bytes())))
	require.NoError(t, target.ValidateMetadata(ctx))
	redirect, err := target.ResolveTagRedirect(ctx, receipt.SourceTagID)
	require.NoError(t, err)
	require.Equal(t, receipt.TargetTagID, redirect)
	resolved, err := target.ResolveTagConcept(ctx, "Old topic")
	require.NoError(t, err)
	require.Equal(t, to.ID, resolved.ID)
	var restored bytes.Buffer
	require.NoError(t, target.ExportMetadata(ctx, &restored))
	require.Equal(t, backup.Bytes(), restored.Bytes())
}

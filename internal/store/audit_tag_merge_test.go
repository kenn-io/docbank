package store

import (
	"bytes"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/query"
)

// A reviewed merge must remain replayable when its source was assigned to an
// audited node; reversing it must restore that exact tag identity and assignment.
func TestAuditedTagMergeReverseRoundTripsAssignedIdentity(t *testing.T) {
	s, source, node := newAuditedTagStore(t)
	ctx := t.Context()
	target, err := s.CreateTag(ctx, "destination")
	require.NoError(t, err)
	_, err = s.AssignTag(ctx, source.ID, node.ID, node.Revision)
	require.NoError(t, err)
	preview, err := s.PreviewTagMerge(ctx, source.ID, target.ID)
	require.NoError(t, err)
	require.Equal(t, []int64{node.ID}, preview.DocumentNodeIDs)

	receipt, err := s.CommitTagMerge(ctx, preview)
	require.NoError(t, err)
	require.NoError(t, s.ValidateMetadata(ctx))
	tags, _, err := s.NodeTags(ctx, node.ID, 10, 0)
	require.NoError(t, err)
	require.Contains(t, tagIDs(tags), target.ID)
	require.NotContains(t, tagIDs(tags), source.ID)

	_, err = s.ReverseTagMerge(ctx, receipt.MergeID)
	require.NoError(t, err)
	require.NoError(t, s.ValidateMetadata(ctx))
	tags, _, err = s.NodeTags(ctx, node.ID, 10, 0)
	require.NoError(t, err)
	require.Contains(t, tagIDs(tags), source.ID)
	require.NotContains(t, tagIDs(tags), target.ID)

	var exported bytes.Buffer
	require.NoError(t, s.ExportMetadata(ctx, &exported))
	restored, err := Open(filepath.Join(t.TempDir(), "restored.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, restored.Close()) })
	require.NoError(t, restored.ImportMetadata(ctx, bytes.NewReader(exported.Bytes())))
	var roundTrip bytes.Buffer
	require.NoError(t, restored.ExportMetadata(ctx, &roundTrip))
	require.Equal(t, exported.Bytes(), roundTrip.Bytes())
}

func tagIDs(tags []Tag) []string {
	ids := make([]string, len(tags))
	for i, tag := range tags {
		ids[i] = tag.ID
	}
	return ids
}

func TestAuditedTagMergeReplaysAliasQueryAndMapRows(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "vault.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	ctx := t.Context()
	seedMetadataRoundTrip(t, s)
	source, err := s.CreateTag(ctx, "source")
	require.NoError(t, err)
	target, err := s.CreateTag(ctx, "target")
	require.NoError(t, err)
	_, err = s.AddTagAlias(ctx, source.ID, 1, "former topic")
	require.NoError(t, err)
	report, err := s.NodeByPath(ctx, "/Projects/report.txt")
	require.NoError(t, err)
	_, err = s.AssignTag(ctx, source.ID, report.ID, report.Revision)
	require.NoError(t, err)
	q, err := query.Parse([]byte(fmt.Sprintf(`{"filters":{"tag_ids":[%q]}}`, source.ID)))
	require.NoError(t, err)
	payload, err := query.Canonical(q)
	require.NoError(t, err)
	saved, err := s.CreateSavedQuery(ctx, "saved topic", "", SavedQueryKindQuery, payload)
	require.NoError(t, err)
	identity, err := s.EnsureDocumentIdentity(ctx, report.ID)
	require.NoError(t, err)
	definition := mapTestDefinition(document.ContentMapPin{DocumentUID: identity.DocumentUID, Mode: document.MapPinFollowCurrent})
	selector := query.Query{V: 1, Syntax: "simple", Mode: "lexical", Sort: query.Sort{Field: "name", Direction: "asc"}, Filters: query.Filters{TagIDs: []string{source.ID}}}
	definition.Sections[0].Selector = &selector
	access := MapAccess{Owner: "local", AllSources: true}
	plan, err := s.PreviewContentMap(ctx, access, definition)
	require.NoError(t, err)
	contentMap, err := s.CreateContentMap(ctx, access, definition, plan.DefinitionDigest)
	require.NoError(t, err)
	projects, err := s.NodeByPath(ctx, "/Projects")
	require.NoError(t, err)
	seedInitialAuditAuthority(t, s, projects.ID)

	preview, err := s.PreviewTagMerge(ctx, source.ID, target.ID)
	require.NoError(t, err)
	require.Equal(t, []string{saved.ID}, preview.SavedQueryIDs)
	require.Equal(t, []string{contentMap.ID}, preview.ContentMapIDs)
	_, err = s.CommitTagMerge(ctx, preview)
	require.NoError(t, err)
	require.NoError(t, s.ValidateMetadata(ctx))
	resolved, err := s.ResolveTagConcept(ctx, "former topic")
	require.NoError(t, err)
	require.Equal(t, target.ID, resolved.ID)
	updatedQuery, err := s.SavedQueryByID(ctx, saved.ID)
	require.NoError(t, err)
	updatedPayload, err := query.Parse(updatedQuery.Payload)
	require.NoError(t, err)
	require.Equal(t, []string{target.ID}, updatedPayload.Filters.TagIDs)
	updatedMap, err := s.ContentMapByID(ctx, access, contentMap.ID)
	require.NoError(t, err)
	require.Equal(t, []string{target.ID}, updatedMap.Definition.Sections[0].Selector.Filters.TagIDs)

	var exported bytes.Buffer
	require.NoError(t, s.ExportMetadata(ctx, &exported))
	restored, err := Open(filepath.Join(t.TempDir(), "restored.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, restored.Close()) })
	require.NoError(t, restored.ImportMetadata(ctx, bytes.NewReader(exported.Bytes())))
	var repeated bytes.Buffer
	require.NoError(t, restored.ExportMetadata(ctx, &repeated))
	require.Equal(t, exported.Bytes(), repeated.Bytes())
	_, err = restored.db.ExecContext(ctx, `UPDATE saved_queries SET description='tampered' WHERE id=?`, saved.ID)
	require.NoError(t, err)
	require.ErrorContains(t, restored.ValidateMetadata(ctx), "replayed tag merge queries or maps")
	mapRestored, err := Open(filepath.Join(t.TempDir(), "map-restored.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, mapRestored.Close()) })
	require.NoError(t, mapRestored.ImportMetadata(ctx, bytes.NewReader(exported.Bytes())))
	_, err = mapRestored.db.ExecContext(ctx, `UPDATE content_maps SET revision=revision+1 WHERE id=?`, contentMap.ID)
	require.NoError(t, err)
	require.ErrorContains(t, mapRestored.ValidateMetadata(ctx), "replayed tag merge queries or maps")
}

func TestAuditedTagMergeReplaysPassageAndBackup(t *testing.T) {
	s, versions := newRenditionCatalogFixture(t)
	ctx := t.Context()
	profile := catalogProcessingProfile(t, false)
	build := catalogRenditionBuild(s, profile)
	require.NoError(t, s.StageRenditionBuild(ctx, build))
	attachment := RenditionAttachmentRecord{ID: catalogAttachmentFirst, VaultID: s.VaultID(),
		ContentVersionID: versions[0], BuildID: build.ID, Profile: profile,
		AttachedAt: "2026-08-22T10:00:00.000000000Z"}
	require.NoError(t, publishRenditionForTest(t, s, attachment,
		"2026-08-22T10:01:00.000000000Z", testSHA256([]byte("merge-passage-generation"))))
	node, err := s.NodeByPath(ctx, "/synthetic-source-a.pdf")
	require.NoError(t, err)
	identity, err := s.EnsureDocumentIdentity(ctx, node.ID)
	require.NoError(t, err)
	ref := document.PassageRefV1{Version: 1, VaultUID: s.VaultID(), DocumentUID: identity.DocumentUID,
		ContentVersionID: versions[0], SourceSHA256: build.SourceSHA256,
		RenditionBuildID: build.ID, AttachmentID: attachment.ID,
		BodySHA256: testSHA256([]byte("body")), ByteStart: 0, ByteEnd: 1,
		QuoteSHA256: testSHA256([]byte("b"))}
	source, err := s.CreateTag(ctx, "passage source")
	require.NoError(t, err)
	target, err := s.CreateTag(ctx, "passage target")
	require.NoError(t, err)
	_, err = s.AssignPassageTag(ctx, source.ID, ref)
	require.NoError(t, err)
	seedInitialAuditAuthority(t, s, s.RootID())
	preview, err := s.PreviewTagMerge(ctx, source.ID, target.ID)
	require.NoError(t, err)
	require.Len(t, preview.PassageIDs, 1)
	_, err = s.CommitTagMerge(ctx, preview)
	require.NoError(t, err)
	require.NoError(t, s.ValidateMetadata(ctx))
	tags, err := s.PassageTags(ctx, ref)
	require.NoError(t, err)
	require.Equal(t, []string{target.ID}, tagIDs(tags))
	snapshot, err := s.BeginMetadataSnapshot(ctx)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, snapshot.Close()) })
	var backup bytes.Buffer
	require.NoError(t, snapshot.ExportBackup(ctx, &backup))
	restored := newTestStore(t)
	require.NoError(t, restored.ImportMetadata(ctx, bytes.NewReader(backup.Bytes())))
	require.NoError(t, restored.ValidateMetadata(ctx))
	restoredTags, err := restored.PassageTags(ctx, ref)
	require.NoError(t, err)
	require.Equal(t, []string{target.ID}, tagIDs(restoredTags))
}

func TestAuditedTagMergeStalePreviewLeavesHistoryAndAssignments(t *testing.T) {
	s, source, node := newAuditedTagStore(t)
	ctx := t.Context()
	target, err := s.CreateTag(ctx, "destination")
	require.NoError(t, err)
	_, err = s.AssignTag(ctx, source.ID, node.ID, node.Revision)
	require.NoError(t, err)
	preview, err := s.PreviewTagMerge(ctx, source.ID, target.ID)
	require.NoError(t, err)
	_, err = s.RenameTag(ctx, source.ID, preview.SourceRevision, "updated source")
	require.NoError(t, err)
	var before int64
	require.NoError(t, s.db.QueryRowContext(ctx, `SELECT operation_sequence_high_water FROM audit_authority`).Scan(&before))
	_, err = s.CommitTagMerge(ctx, preview)
	require.ErrorIs(t, err, ErrStaleRevision)
	var after int64
	require.NoError(t, s.db.QueryRowContext(ctx, `SELECT operation_sequence_high_water FROM audit_authority`).Scan(&after))
	require.Equal(t, before, after)
	tags, _, err := s.NodeTags(ctx, node.ID, 10, 0)
	require.NoError(t, err)
	require.Contains(t, tagIDs(tags), source.ID)
	require.NotContains(t, tagIDs(tags), target.ID)
	require.NoError(t, s.ValidateMetadata(ctx))
}

func TestAuditedTagMergeAuditFailureRollsBackEveryTable(t *testing.T) {
	s, source, node := newAuditedTagStore(t)
	ctx := t.Context()
	target, err := s.CreateTag(ctx, "destination")
	require.NoError(t, err)
	_, err = s.AssignTag(ctx, source.ID, node.ID, node.Revision)
	require.NoError(t, err)
	preview, err := s.PreviewTagMerge(ctx, source.ID, target.ID)
	require.NoError(t, err)
	var before int64
	require.NoError(t, s.db.QueryRowContext(ctx, `SELECT operation_sequence_high_water FROM audit_authority`).Scan(&before))
	_, err = s.db.ExecContext(ctx, `CREATE TRIGGER reject_merge_audit BEFORE UPDATE ON audit_authority
		BEGIN SELECT RAISE(ABORT, 'forced merge audit failure'); END`)
	require.NoError(t, err)
	_, err = s.CommitTagMerge(ctx, preview)
	require.ErrorContains(t, err, "forced merge audit failure")
	var after, redirects, receipts int64
	require.NoError(t, s.db.QueryRowContext(ctx, `SELECT operation_sequence_high_water FROM audit_authority`).Scan(&after))
	require.Equal(t, before, after)
	require.NoError(t, s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM tag_redirects`).Scan(&redirects))
	require.NoError(t, s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM tag_merge_audit`).Scan(&receipts))
	require.Zero(t, redirects)
	require.Zero(t, receipts)
	tags, _, err := s.NodeTags(ctx, node.ID, 10, 0)
	require.NoError(t, err)
	require.Contains(t, tagIDs(tags), source.ID)
	require.NotContains(t, tagIDs(tags), target.ID)
	require.NoError(t, s.ValidateMetadata(ctx))
}

func TestAuditedTagMergeConflictingDescriptionsStayClosed(t *testing.T) {
	s, source, _ := newAuditedTagStore(t)
	ctx := t.Context()
	target, err := s.CreateTag(ctx, "destination")
	require.NoError(t, err)
	_, err = s.SetTagConcept(ctx, source.ID, 1, "Source meaning")
	require.NoError(t, err)
	_, err = s.SetTagConcept(ctx, target.ID, 1, "Different meaning")
	require.NoError(t, err)
	preview, err := s.PreviewTagMerge(ctx, source.ID, target.ID)
	require.NoError(t, err)
	var before int64
	require.NoError(t, s.db.QueryRowContext(ctx, `SELECT operation_sequence_high_water FROM audit_authority`).Scan(&before))
	_, err = s.CommitTagMerge(ctx, preview)
	require.ErrorIs(t, err, ErrInvalidTag)
	var after int64
	require.NoError(t, s.db.QueryRowContext(ctx, `SELECT operation_sequence_high_water FROM audit_authority`).Scan(&after))
	require.Equal(t, before, after)
	require.NoError(t, s.ValidateMetadata(ctx))
}

func TestAuditedTagMergeAdvancedQueryStaysClosed(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "vault.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	ctx := t.Context()
	seedMetadataRoundTrip(t, s)
	source, err := s.CreateTag(ctx, "source")
	require.NoError(t, err)
	target, err := s.CreateTag(ctx, "target")
	require.NoError(t, err)
	q, err := query.Parse([]byte(fmt.Sprintf(`{"syntax":"advanced","text":%q}`, "tag:"+source.ID)))
	require.NoError(t, err)
	payload, err := query.Canonical(q)
	require.NoError(t, err)
	_, err = s.CreateSavedQuery(ctx, "advanced", "", SavedQueryKindQuery, payload)
	require.NoError(t, err)
	projects, err := s.NodeByPath(ctx, "/Projects")
	require.NoError(t, err)
	seedInitialAuditAuthority(t, s, projects.ID)
	preview, err := s.PreviewTagMerge(ctx, source.ID, target.ID)
	require.NoError(t, err)
	require.Len(t, preview.UnmigratableSavedQueryIDs, 1)
	var before int64
	require.NoError(t, s.db.QueryRowContext(ctx, `SELECT operation_sequence_high_water FROM audit_authority`).Scan(&before))
	_, err = s.CommitTagMerge(ctx, preview)
	require.ErrorIs(t, err, ErrInvalidTag)
	var after int64
	require.NoError(t, s.db.QueryRowContext(ctx, `SELECT operation_sequence_high_water FROM audit_authority`).Scan(&after))
	require.Equal(t, before, after)
	require.NoError(t, s.ValidateMetadata(ctx))
}

func TestAuditedTagMergeReverseRejectsChangedTarget(t *testing.T) {
	s, source, _ := newAuditedTagStore(t)
	ctx := t.Context()
	target, err := s.CreateTag(ctx, "destination")
	require.NoError(t, err)
	preview, err := s.PreviewTagMerge(ctx, source.ID, target.ID)
	require.NoError(t, err)
	receipt, err := s.CommitTagMerge(ctx, preview)
	require.NoError(t, err)
	current, err := s.TagByID(ctx, target.ID)
	require.NoError(t, err)
	_, err = s.RenameTag(ctx, target.ID, current.Revision, "changed target")
	require.NoError(t, err)
	_, err = s.ReverseTagMerge(ctx, receipt.MergeID)
	require.ErrorIs(t, err, ErrStaleRevision)
	_, err = s.TagByID(ctx, source.ID)
	require.ErrorIs(t, err, ErrNotFound)
	require.NoError(t, s.ValidateMetadata(ctx))
}

func TestAuditedTagMergeReverseWithoutAssignments(t *testing.T) {
	s, source, _ := newAuditedTagStore(t)
	ctx := t.Context()
	target, err := s.CreateTag(ctx, "destination")
	require.NoError(t, err)
	preview, err := s.PreviewTagMerge(ctx, source.ID, target.ID)
	require.NoError(t, err)
	require.Empty(t, preview.DocumentNodeIDs)
	receipt, err := s.CommitTagMerge(ctx, preview)
	require.NoError(t, err)
	require.NoError(t, s.ValidateMetadata(ctx))
	_, err = s.ReverseTagMerge(ctx, receipt.MergeID)
	require.NoError(t, err)
	require.NoError(t, s.ValidateMetadata(ctx))
	restored, err := s.TagByID(ctx, source.ID)
	require.NoError(t, err)
	require.Equal(t, source.Name, restored.Name)
}

func TestAuditedTagMergeAdvancesEveryAffectedScope(t *testing.T) {
	s, source, first, second := newMultiScopeAuditedTagStore(t)
	ctx := t.Context()
	target, err := s.CreateTag(ctx, "destination")
	require.NoError(t, err)
	preview, err := s.PreviewTagMerge(ctx, source.ID, target.ID)
	require.NoError(t, err)
	require.Equal(t, 2, preview.DocumentAssignments)
	_, err = s.CommitTagMerge(ctx, preview)
	require.NoError(t, err)
	require.NoError(t, s.ValidateMetadata(ctx))
	for _, node := range []Node{first, second} {
		tags, _, err := s.NodeTags(ctx, node.ID, 10, 0)
		require.NoError(t, err)
		require.Contains(t, tagIDs(tags), target.ID)
		require.NotContains(t, tagIDs(tags), source.ID)
	}
}

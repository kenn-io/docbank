package store

import (
	"database/sql"
	"encoding/json/v2"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/query"
)

func TestContentMapWriteChecksCurrentPinInsideTransaction(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	access := MapAccess{Owner: "local", AllSources: true}
	node, err := s.CreateFile(ctx, s.RootID(), "source.txt", fakeHash("map-transaction"), 4, "text/plain")
	require.NoError(t, err)
	identity, err := s.EnsureDocumentIdentity(ctx, node.ID)
	require.NoError(t, err)
	definition := mapTestDefinition(document.ContentMapPin{DocumentUID: identity.DocumentUID, Mode: document.MapPinFollowCurrent})
	_, err = s.PreviewContentMap(ctx, access, definition)
	require.NoError(t, err)
	require.NoError(t, s.withStorageTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE nodes SET trashed_at=? WHERE id=?`, nowRFC3339(), node.ID)
		if err != nil {
			return err
		}
		err = s.validateMapSourcesTx(ctx, tx, access, definition)
		require.ErrorIs(t, err, ErrNotFound)
		return nil
	}))
}

func mapTestDefinition(pins ...document.ContentMapPin) ContentMapDefinition {
	return ContentMapDefinition{Title: "Synthetic research", Scope: "local",
		Sections: []ContentMapSection{{ID: "core", Heading: "Core", Include: pins,
			Ordering: "explicit", MaxEntries: 100}}}
}

func TestContentMapDefinitionBoundsAndStableSectionIDs(t *testing.T) {
	const docUID = "00000000-0000-4000-8000-000000000001"
	definition := mapTestDefinition(document.ContentMapPin{DocumentUID: docUID, Mode: document.MapPinFollowCurrent})
	definition.Sections = append(definition.Sections, ContentMapSection{ID: "more", Heading: "Core",
		Include: definition.Sections[0].Include, Ordering: "explicit", MaxEntries: 100})
	plan, _, err := normalizedMapDefinition(definition)
	require.NoError(t, err)
	assert.Equal(t, "core", plan.Definition.Sections[0].ID)
	assert.Equal(t, "more", plan.Definition.Sections[1].ID)
	assert.Equal(t, plan.Definition.Sections[0].Heading, plan.Definition.Sections[1].Heading)

	definition.Sections[1].ID = "core"
	_, _, err = normalizedMapDefinition(definition)
	require.ErrorIs(t, err, ErrInvalidContentMap)
	definition.Sections[1].ID = "more"
	definition.Sections[0].Description = strings.Repeat("x", document.MaxContentMapDescriptionBytes)
	_, _, err = normalizedMapDefinition(definition)
	require.NoError(t, err)
	definition.Sections[0].Description += "x"
	_, _, err = normalizedMapDefinition(definition)
	require.ErrorIs(t, err, ErrInvalidContentMap)
	definition.Sections[0].Description = ""
	definition.Sections[0].MaxEntries = 0
	_, _, err = normalizedMapDefinition(definition)
	require.ErrorIs(t, err, ErrInvalidContentMap)
	definition.Sections[0].MaxEntries = 100

	for len(definition.Sections) < document.MaxContentMapSections {
		section := definition.Sections[0]
		section.ID = fmt.Sprintf("s%d", len(definition.Sections))
		definition.Sections = append(definition.Sections, section)
	}
	_, _, err = normalizedMapDefinition(definition)
	require.NoError(t, err)
	section := definition.Sections[0]
	section.ID = "overflow"
	definition.Sections = append(definition.Sections, section)
	_, _, err = normalizedMapDefinition(definition)
	require.ErrorIs(t, err, ErrInvalidContentMap)

	definition = mapTestDefinition()
	definition.Sections[0].MaxEntries = document.MaxContentMapPins
	for i := range document.MaxContentMapPins {
		definition.Sections[0].Include = append(definition.Sections[0].Include, document.ContentMapPin{
			DocumentUID: fmt.Sprintf("00000000-0000-4000-8000-%012x", i), Mode: document.MapPinFollowCurrent,
		})
	}
	_, _, err = normalizedMapDefinition(definition)
	require.NoError(t, err)
	definition.Sections[0].Include = append(definition.Sections[0].Include, document.ContentMapPin{
		DocumentUID: "00000000-0000-4000-8000-000000001001", Mode: document.MapPinFollowCurrent,
	})
	_, _, err = normalizedMapDefinition(definition)
	require.ErrorIs(t, err, ErrInvalidContentMap)
}

func TestContentMapRejectsContradictoryAndOversizedScope(t *testing.T) {
	const uid = "00000000-0000-4000-8000-000000000001"
	pin := document.ContentMapPin{DocumentUID: uid, Mode: document.MapPinFollowCurrent}
	definition := mapTestDefinition(pin)
	definition.Sections[0].Exclude = []document.ContentMapPin{pin}
	_, _, err := normalizedMapDefinition(definition)
	require.ErrorIs(t, err, ErrInvalidContentMap)
	definition.Sections[0].Exclude = []document.ContentMapPin{{DocumentUID: uid,
		Mode: document.MapPinVersionPinned, ContentVersionID: "00000000-0000-4000-8000-000000000002"}}
	_, _, err = normalizedMapDefinition(definition)
	require.ErrorIs(t, err, ErrInvalidContentMap, "different pin modes cannot contradict for one document")

	access := MapAccess{Owner: "local", PermittedVersionIDs: make([]string, MaxSearchSourceFenceIDs)}
	for i := range access.PermittedVersionIDs {
		access.PermittedVersionIDs[i] = fmt.Sprintf("00000000-0000-4000-8000-%012x", i)
	}
	require.NoError(t, validateMapAccess(access))
	access.PermittedVersionIDs = append(access.PermittedVersionIDs, "00000000-0000-4000-8000-000000001001")
	err = validateMapAccess(access)
	require.ErrorIs(t, err, ErrProcessingSourceFenceScopeTooLarge)

	paths := make([]string, 64)
	for i := range paths {
		paths[i] = fmt.Sprintf("/path-%02d-%s", i, strings.Repeat("x", 500))
	}
	selector := query.Query{V: 1, Syntax: "simple", Mode: "lexical",
		Sort: query.Sort{Field: "name", Direction: "asc"}, Filters: query.Filters{Paths: paths}}
	definition = ContentMapDefinition{Title: "Large map", Scope: "local"}
	for i := range document.MaxContentMapSections {
		definition.Sections = append(definition.Sections, ContentMapSection{ID: fmt.Sprintf("s%d", i),
			Heading: "Section", Selector: &selector, Ordering: "explicit", MaxEntries: 10})
	}
	_, _, err = normalizedMapDefinition(definition)
	require.ErrorIs(t, err, ErrInvalidContentMap, "definition metadata must stay within 1 MiB")
}

func TestContentMapRevisionSnapshotsAndSourceChanges(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	access := MapAccess{Owner: "local", AllSources: true}
	node, err := s.CreateFile(ctx, s.RootID(), "evidence.txt", fakeHash("map-v1"), 12, "text/plain")
	require.NoError(t, err)
	identity, err := s.EnsureDocumentIdentity(ctx, node.ID)
	require.NoError(t, err)
	definition := ContentMapDefinition{Title: "Synthetic evidence", Scope: "local", Sections: []ContentMapSection{
		{ID: "current", Heading: "Repeated", Include: []document.ContentMapPin{{DocumentUID: identity.DocumentUID,
			Mode: document.MapPinFollowCurrent}}, Ordering: "explicit", MaxEntries: 10},
		{ID: "fixed", Heading: "Repeated", Include: []document.ContentMapPin{{DocumentUID: identity.DocumentUID,
			Mode: document.MapPinVersionPinned, ContentVersionID: node.CurrentVersionID}}, Ordering: "explicit", MaxEntries: 10},
	}}
	plan, err := s.PreviewContentMap(ctx, access, definition)
	require.NoError(t, err)
	created, err := s.CreateContentMap(ctx, access, definition, plan.DefinitionDigest)
	require.NoError(t, err)
	assert.Equal(t, int64(1), created.Revision)
	_, err = s.CreateContentMap(ctx, access, definition, "wrong")
	require.ErrorIs(t, err, ErrStaleRevision)
	first, err := s.CreateContentMapSnapshot(ctx, access, created.ID, 1)
	require.NoError(t, err)
	require.Len(t, first.Sections, 2)
	assert.Equal(t, "current", first.Sections[0].ID)
	assert.Equal(t, "fixed", first.Sections[1].ID)
	assert.Equal(t, node.CurrentVersionID, first.Sections[0].Entries[0].Member.ContentVersionID)
	assert.Equal(t, first.Sections[0].Entries[0].Member, first.Sections[1].Entries[0].Member)

	archive, err := s.Mkdir(ctx, s.RootID(), "archive")
	require.NoError(t, err)
	_, _, err = s.Move(ctx, node.ID, archive.ID, "moved.txt", node.Revision)
	require.NoError(t, err)
	moved, err := s.NodeByID(ctx, node.ID)
	require.NoError(t, err)
	_, replacement, err := s.ReplaceContent(ctx, node.ID, moved.Revision, fakeHash("map-v2"), 14, "text/plain")
	require.NoError(t, err)
	second, err := s.CreateContentMapSnapshot(ctx, access, created.ID, 1)
	require.NoError(t, err)
	assert.Equal(t, replacement.ID, second.Sections[0].Entries[0].Member.ContentVersionID)
	assert.Equal(t, node.CurrentVersionID, second.Sections[1].Entries[0].Member.ContentVersionID)
	assert.Equal(t, "/archive/moved.txt", second.Sections[0].Entries[0].Path)
	storedFirst, err := s.ContentMapSnapshotByID(ctx, access, first.ID)
	require.NoError(t, err)
	assert.Equal(t, first, storedFirst)
	assert.Equal(t, "/evidence.txt", storedFirst.Sections[0].Entries[0].Path)
	assert.NotEqual(t, first.MemberHash, second.MemberHash)

	definition.Title = "Edited by first editor"
	newPlan, err := s.PreviewContentMap(ctx, access, definition)
	require.NoError(t, err)
	updated, err := s.UpdateContentMap(ctx, access, created.ID, 1, definition, newPlan.DefinitionDigest)
	require.NoError(t, err)
	assert.Equal(t, int64(2), updated.Revision)
	_, err = s.UpdateContentMap(ctx, access, created.ID, 1, definition, newPlan.DefinitionDigest)
	require.ErrorIs(t, err, ErrStaleRevision)
	_, err = s.CreateContentMapSnapshot(ctx, access, created.ID, 1)
	require.ErrorIs(t, err, ErrStaleRevision)
	_, _, err = s.Trash(ctx, node.ID, UnconditionalRev)
	require.NoError(t, err)
	tombstone, err := s.CreateContentMapSnapshot(ctx, access, created.ID, 2)
	require.NoError(t, err)
	assert.Equal(t, mapAvailabilityUnavailable, tombstone.Sections[0].Entries[0].Availability)
	assert.Equal(t, node.CurrentVersionID, tombstone.Sections[1].Entries[0].RequestedContentVersionID)
	_, err = s.ArchiveContentMap(ctx, access, created.ID, 2)
	require.NoError(t, err)
	_, err = s.CreateContentMapSnapshot(ctx, access, created.ID, 3)
	require.ErrorIs(t, err, ErrNotFound)
	_, err = s.ContentMapSnapshotByID(ctx, access, first.ID)
	require.NoError(t, err)
}

func TestContentMapAuthorizationAndSelectorReceipt(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	first, err := s.CreateFile(ctx, s.RootID(), "visible.txt", fakeHash("visible"), 7, "text/plain")
	require.NoError(t, err)
	second, err := s.CreateFile(ctx, s.RootID(), "hidden.txt", fakeHash("hidden"), 6, "text/plain")
	require.NoError(t, err)
	firstID, err := s.EnsureDocumentIdentity(ctx, first.ID)
	require.NoError(t, err)
	secondID, err := s.EnsureDocumentIdentity(ctx, second.ID)
	require.NoError(t, err)
	scoped := MapAccess{Owner: "reader", PermittedVersionIDs: []string{first.CurrentVersionID}}
	_, err = s.PreviewContentMap(ctx, scoped, mapTestDefinition(document.ContentMapPin{
		DocumentUID: secondID.DocumentUID, Mode: document.MapPinFollowCurrent}))
	require.ErrorIs(t, err, ErrNotFound)
	definition := mapTestDefinition(document.ContentMapPin{DocumentUID: firstID.DocumentUID,
		Mode: document.MapPinVersionPinned, ContentVersionID: first.CurrentVersionID})
	plan, err := s.PreviewContentMap(ctx, scoped, definition)
	require.NoError(t, err)
	created, err := s.CreateContentMap(ctx, scoped, definition, plan.DefinitionDigest)
	require.NoError(t, err)
	_, err = s.ContentMapByID(ctx, MapAccess{Owner: "reader"}, created.ID)
	require.ErrorIs(t, err, ErrNotFound)
	snapshot, err := s.CreateContentMapSnapshot(ctx, scoped, created.ID, 1)
	require.NoError(t, err)
	readBack, err := s.ContentMapSnapshotByID(ctx, scoped, snapshot.ID)
	require.NoError(t, err)
	assert.Equal(t, snapshot, readBack)
	_, err = s.ContentMapSnapshotByID(ctx, MapAccess{Owner: "reader", PermittedVersionIDs: []string{
		first.CurrentVersionID, second.CurrentVersionID,
	}}, snapshot.ID)
	require.ErrorIs(t, err, ErrNotFound, "a broader scoped grant must not disclose frozen exclusions")
	_, err = s.ContentMapSnapshotByID(ctx, MapAccess{Owner: "reader"}, snapshot.ID)
	require.ErrorIs(t, err, ErrNotFound, "a revoked grant must not disclose the snapshot")
	_, err = s.ContentMapSnapshotByID(ctx, MapAccess{Owner: "other", AllSources: true}, snapshot.ID)
	require.ErrorIs(t, err, ErrNotFound)

	selector, err := query.Parse([]byte(`{}`))
	require.NoError(t, err)
	definition = mapTestDefinition()
	definition.Sections[0].Selector = &selector
	_, err = s.PreviewContentMap(ctx, scoped, definition)
	require.ErrorIs(t, err, ErrNotFound)
	full := MapAccess{Owner: "local", AllSources: true}
	plan, err = s.PreviewContentMap(ctx, full, definition)
	require.NoError(t, err)
	created, err = s.CreateContentMap(ctx, full, definition, plan.DefinitionDigest)
	require.NoError(t, err)
	snapshot, err = s.CreateContentMapSnapshot(ctx, full, created.ID, 1)
	require.NoError(t, err)
	require.Len(t, snapshot.Sections[0].Entries, 2)
	assert.Len(t, snapshot.MemberHash, 64) // query member hash is a bare SHA-256 hex digest
}

func TestContentMapTopicFrequencyOrdersPinnedDocumentsByAssignedTags(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	access := MapAccess{Owner: "local", AllSources: true}
	lessTagged, err := s.CreateFile(ctx, s.RootID(), "one.txt", fakeHash("one"), 3, "text/plain")
	require.NoError(t, err)
	moreTagged, err := s.CreateFile(ctx, s.RootID(), "two.txt", fakeHash("two"), 3, "text/plain")
	require.NoError(t, err)
	firstTag, err := s.CreateTag(ctx, "research")
	require.NoError(t, err)
	secondTag, err := s.CreateTag(ctx, "evidence")
	require.NoError(t, err)
	_, err = s.AssignTag(ctx, firstTag.ID, lessTagged.ID, lessTagged.Revision)
	require.NoError(t, err)
	change, err := s.AssignTag(ctx, firstTag.ID, moreTagged.ID, moreTagged.Revision)
	require.NoError(t, err)
	_, err = s.AssignTag(ctx, secondTag.ID, moreTagged.ID, change.Node.Revision)
	require.NoError(t, err)
	lessIdentity, err := s.EnsureDocumentIdentity(ctx, lessTagged.ID)
	require.NoError(t, err)
	moreIdentity, err := s.EnsureDocumentIdentity(ctx, moreTagged.ID)
	require.NoError(t, err)
	definition := mapTestDefinition(
		document.ContentMapPin{DocumentUID: lessIdentity.DocumentUID, Mode: document.MapPinVersionPinned,
			ContentVersionID: lessTagged.CurrentVersionID},
		document.ContentMapPin{DocumentUID: moreIdentity.DocumentUID, Mode: document.MapPinVersionPinned,
			ContentVersionID: moreTagged.CurrentVersionID},
	)
	definition.Sections[0].Ordering = "topic_frequency"
	plan, err := s.PreviewContentMap(ctx, access, definition)
	require.NoError(t, err)
	created, err := s.CreateContentMap(ctx, access, definition, plan.DefinitionDigest)
	require.NoError(t, err)
	snapshot, err := s.CreateContentMapSnapshot(ctx, access, created.ID, created.Revision)
	require.NoError(t, err)
	require.Len(t, snapshot.Sections[0].Entries, 2)
	assert.Equal(t, moreIdentity.DocumentUID, snapshot.Sections[0].Entries[0].DocumentUID)
	assert.Equal(t, 2, snapshot.Sections[0].Entries[0].TagCount)
	assert.Equal(t, lessIdentity.DocumentUID, snapshot.Sections[0].Entries[1].DocumentUID)
	assert.Equal(t, 1, snapshot.Sections[0].Entries[1].TagCount)
}

func TestContentMapScopedVersionPinnedTombstone(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	node, err := s.CreateFile(ctx, s.RootID(), "removed.txt", fakeHash("removed"), 7, "text/plain")
	require.NoError(t, err)
	identity, err := s.EnsureDocumentIdentity(ctx, node.ID)
	require.NoError(t, err)
	access := MapAccess{Owner: "reader", PermittedVersionIDs: []string{node.CurrentVersionID}}
	definition := mapTestDefinition(document.ContentMapPin{DocumentUID: identity.DocumentUID,
		Mode: document.MapPinVersionPinned, ContentVersionID: node.CurrentVersionID})
	plan, err := s.PreviewContentMap(ctx, access, definition)
	require.NoError(t, err)
	created, err := s.CreateContentMap(ctx, access, definition, plan.DefinitionDigest)
	require.NoError(t, err)
	_, _, err = s.Trash(ctx, node.ID, UnconditionalRev)
	require.NoError(t, err)
	snapshot, err := s.CreateContentMapSnapshot(ctx, access, created.ID, 1)
	require.NoError(t, err)
	require.Equal(t, mapAvailabilityUnavailable, snapshot.Sections[0].Entries[0].Availability)
	require.Equal(t, node.CurrentVersionID, snapshot.Sections[0].Entries[0].RequestedContentVersionID)
	readBack, err := s.ContentMapSnapshotByID(ctx, access, snapshot.ID)
	require.NoError(t, err)
	assert.Equal(t, snapshot, readBack)
	_, err = s.ContentMapSnapshotByID(ctx, MapAccess{Owner: "reader"}, snapshot.ID)
	require.ErrorIs(t, err, ErrNotFound)
}

func TestContentMapSnapshotMetadataLimit(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	access := MapAccess{Owner: "local", AllSources: true}
	node, err := s.CreateFile(ctx, s.RootID(), "evidence.txt", fakeHash("metadata"), 8, "text/plain")
	require.NoError(t, err)
	identity, err := s.EnsureDocumentIdentity(ctx, node.ID)
	require.NoError(t, err)
	definition := mapTestDefinition(document.ContentMapPin{DocumentUID: identity.DocumentUID,
		Mode: document.MapPinVersionPinned, ContentVersionID: node.CurrentVersionID})
	plan, err := s.PreviewContentMap(ctx, access, definition)
	require.NoError(t, err)
	created, err := s.CreateContentMap(ctx, access, definition, plan.DefinitionDigest)
	require.NoError(t, err)
	snapshot, err := s.CreateContentMapSnapshot(ctx, access, created.ID, 1)
	require.NoError(t, err)
	legacy := snapshot
	legacy.ID, err = newUUIDv4()
	require.NoError(t, err)
	legacyJSON, err := json.Marshal(legacy)
	require.NoError(t, err)
	_, err = s.db.ExecContext(ctx, `INSERT INTO content_map_snapshots(id,map_id,map_revision,owner,scope_digest,definition_digest,snapshot_json,member_hash,created_at)
		VALUES(?,?,?,?,?,?,?,?,?)`, legacy.ID, legacy.MapID, legacy.MapRevision, legacy.Owner, legacy.ScopeDigest,
		legacy.DefinitionDigest, legacyJSON, legacy.MemberHash, legacy.CreatedAt)
	require.NoError(t, err)
	_, err = s.ContentMapSnapshotByID(ctx, access, legacy.ID)
	require.ErrorIs(t, err, ErrInvalidContentMap, "missing frozen dependency receipt must fail closed")

	for _, size := range []int{document.MaxContentMapMetadataBytes, document.MaxContentMapMetadataBytes + 1} {
		stored := snapshot
		stored.ID, err = newUUIDv4()
		require.NoError(t, err)
		dependencies := make([]document.ContentMapPin, 0)
		encoded, err := json.Marshal(contentMapSnapshotStorage{ContentMapSnapshot: stored, Dependencies: &dependencies})
		require.NoError(t, err)
		require.Less(t, len(encoded), size)
		encoded = append(encoded, []byte(strings.Repeat(" ", size-len(encoded)))...)
		_, err = s.db.ExecContext(ctx, `INSERT INTO content_map_snapshots(id,map_id,map_revision,owner,scope_digest,definition_digest,snapshot_json,member_hash,created_at)
			VALUES(?,?,?,?,?,?,?,?,?)`, stored.ID, stored.MapID, stored.MapRevision, stored.Owner, stored.ScopeDigest,
			stored.DefinitionDigest, encoded, stored.MemberHash, stored.CreatedAt)
		require.NoError(t, err)
		readBack, readErr := s.ContentMapSnapshotByID(ctx, access, stored.ID)
		if size == document.MaxContentMapMetadataBytes {
			require.NoError(t, readErr, "exactly 1 MiB must remain readable")
			assert.Equal(t, stored, readBack)
		} else {
			require.ErrorIs(t, readErr, ErrInvalidContentMap)
		}
	}

	member := snapshot.Sections[0].Entries[0].Member
	for len(snapshot.Sections) < document.MaxContentMapSections {
		section := snapshot.Sections[0]
		section.ID = fmt.Sprintf("s%d", len(snapshot.Sections))
		section.Entries = nil
		for i := range 100 {
			entry := ContentMapSnapshotEntry{DocumentUID: fmt.Sprintf("00000000-0000-4000-8000-%012x", i),
				Availability: mapAvailabilityAvailable, Member: member,
				Name: strings.Repeat("n", 160), Path: "/" + strings.Repeat("n", 160)}
			section.Entries = append(section.Entries, entry)
		}
		sectionMembers := make([]SnapshotMember, len(section.Entries))
		for i := range sectionMembers {
			sectionMembers[i] = member
		}
		section.MemberHash = snapshotMemberHash(sectionMembers)
		snapshot.Sections = append(snapshot.Sections, section)
	}
	allMembers := make([]SnapshotMember, 0)
	for _, section := range snapshot.Sections {
		for _, entry := range section.Entries {
			allMembers = append(allMembers, entry.Member)
		}
	}
	snapshot.MemberHash = snapshotMemberHash(allMembers)
	encoded, err := json.Marshal(snapshot)
	require.NoError(t, err)
	require.Greater(t, len(encoded), document.MaxContentMapMetadataBytes)
	require.ErrorIs(t, ValidateContentMapSnapshotRecord(snapshot), ErrInvalidContentMap,
		"repeated section entries count toward the cumulative metadata limit")
}

func TestContentMapPreviewRejectsUnresolvedSelector(t *testing.T) {
	s := newTestStore(t)
	selector, err := query.Parse([]byte(`{"filters":{"tag_ids":["00000000-0000-4000-8000-000000000099"]}}`))
	require.NoError(t, err)
	definition := mapTestDefinition()
	definition.Sections[0].Selector = &selector
	access := MapAccess{Owner: "local", AllSources: true}
	_, err = s.PreviewContentMap(t.Context(), access, definition)
	require.Error(t, err, "preview must resolve selector references before accepting a plan")
	plan, _, err := normalizedMapDefinition(definition)
	require.NoError(t, err)
	_, err = s.CreateContentMap(t.Context(), access, definition, plan.DefinitionDigest)
	require.Error(t, err, "create must re-check selector references after preview")
}

func TestContentMapSnapshotRetainsExactPassageAfterSourceChange(t *testing.T) {
	s, versions := newRenditionCatalogFixture(t)
	profile := catalogProcessingProfile(t, false)
	build := catalogRenditionBuild(s, profile)
	require.NoError(t, s.StageRenditionBuild(t.Context(), build))
	attachment := RenditionAttachmentRecord{ID: catalogAttachmentFirst, VaultID: s.VaultID(),
		ContentVersionID: versions[0], BuildID: build.ID, Profile: profile,
		AttachedAt: "2026-08-22T10:00:00.000000000Z"}
	require.NoError(t, publishRenditionForTest(t, s, attachment,
		"2026-08-22T10:01:00.000000000Z", testSHA256([]byte("map-passage-generation"))))
	node, err := s.NodeByPath(t.Context(), "/synthetic-source-a.pdf")
	require.NoError(t, err)
	identity, err := s.EnsureDocumentIdentity(t.Context(), node.ID)
	require.NoError(t, err)
	ref := document.PassageRefV1{Version: 1, VaultUID: s.VaultID(), DocumentUID: identity.DocumentUID,
		ContentVersionID: versions[0], SourceSHA256: build.SourceSHA256,
		RenditionBuildID: build.ID, AttachmentID: attachment.ID,
		BodySHA256: testSHA256([]byte("body")), ByteStart: 0, ByteEnd: 1,
		QuoteSHA256: testSHA256([]byte("b"))}
	pin := document.ContentMapPin{DocumentUID: identity.DocumentUID, Mode: document.MapPinVersionPinned,
		ContentVersionID: versions[0], Passage: &ref}
	access := MapAccess{Owner: "local", AllSources: true}
	definition := mapTestDefinition(pin)
	plan, err := s.PreviewContentMap(t.Context(), access, definition)
	require.NoError(t, err)
	created, err := s.CreateContentMap(t.Context(), access, definition, plan.DefinitionDigest)
	require.NoError(t, err)
	first, err := s.CreateContentMapSnapshot(t.Context(), access, created.ID, 1)
	require.NoError(t, err)
	require.Equal(t, ref, *first.Sections[0].Entries[0].Passage)
	_, _, err = s.ReplaceContent(t.Context(), node.ID, node.Revision,
		testSHA256([]byte("changed source")), 14, "application/pdf")
	require.NoError(t, err)
	second, err := s.CreateContentMapSnapshot(t.Context(), access, created.ID, 1)
	require.NoError(t, err)
	assert.Equal(t, ref, *second.Sections[0].Entries[0].Passage)
	assert.Equal(t, versions[0], second.Sections[0].Entries[0].Member.ContentVersionID)
	readBack, err := s.ContentMapSnapshotByID(t.Context(), access, first.ID)
	require.NoError(t, err)
	assert.Equal(t, first, readBack)
}

func TestContentMapSnapshotResolvesAdoptedPassageToLocalMember(t *testing.T) {
	source, sourceVersions := newRenditionCatalogFixture(t)
	target, targetVersions := newRenditionCatalogFixture(t)
	profile := catalogProcessingProfile(t, false)
	sourceBuild := catalogRenditionBuild(source, profile)
	targetBuild := catalogRenditionBuild(target, profile)
	targetBuild.ID = catalogBuildReplacement
	require.NoError(t, target.StageRenditionBuild(t.Context(), targetBuild))
	targetAttachment := RenditionAttachmentRecord{ID: catalogAttachmentSecond, VaultID: target.VaultID(),
		ContentVersionID: targetVersions[0], BuildID: targetBuild.ID, Profile: profile,
		AttachedAt: "2026-08-22T10:00:00.000000000Z"}
	require.NoError(t, publishRenditionForTest(t, target, targetAttachment,
		"2026-08-22T10:01:00.000000000Z", testSHA256([]byte("adopted-map-generation"))))
	sourceNode, err := source.NodeByPath(t.Context(), "/synthetic-source-a.pdf")
	require.NoError(t, err)
	sourceIdentity, err := source.EnsureDocumentIdentity(t.Context(), sourceNode.ID)
	require.NoError(t, err)
	targetNode, err := target.NodeByPath(t.Context(), "/synthetic-source-a.pdf")
	require.NoError(t, err)
	targetIdentity, err := target.EnsureDocumentIdentity(t.Context(), targetNode.ID)
	require.NoError(t, err)
	ref := document.PassageRefV1{Version: 1, VaultUID: source.VaultID(), DocumentUID: sourceIdentity.DocumentUID,
		ContentVersionID: sourceVersions[0], SourceSHA256: sourceBuild.SourceSHA256,
		RenditionBuildID: sourceBuild.ID, AttachmentID: catalogAttachmentFirst,
		BodySHA256: testSHA256([]byte("body")), ByteStart: 0, ByteEnd: 1,
		QuoteSHA256: testSHA256([]byte("b"))}
	domain := "99999999-9999-4999-8999-999999999999"
	require.NoError(t, target.PutDocumentIdentityAlias(t.Context(), domain, ref.VaultUID,
		ref.DocumentUID, targetIdentity.DocumentUID))
	require.NoError(t, target.PutAdoptedPassageAuthority(t.Context(), domain, ref,
		targetIdentity.DocumentUID, targetVersions[0], targetBuild.ID, targetAttachment.ID))
	ref.FederationDomainUID = domain
	pin := document.ContentMapPin{DocumentUID: ref.DocumentUID, Mode: document.MapPinVersionPinned,
		ContentVersionID: ref.ContentVersionID, Passage: &ref}
	definition := mapTestDefinition(pin)
	access := MapAccess{Owner: "scoped", PermittedVersionIDs: []string{targetVersions[0]}}
	plan, err := target.PreviewContentMap(t.Context(), access, definition)
	require.NoError(t, err)
	created, err := target.CreateContentMap(t.Context(), access, definition, plan.DefinitionDigest)
	require.NoError(t, err)
	snapshot, err := target.CreateContentMapSnapshot(t.Context(), access, created.ID, created.Revision)
	require.NoError(t, err)
	require.Len(t, snapshot.Sections[0].Entries, 1)
	entry := snapshot.Sections[0].Entries[0]
	assert.Equal(t, ref, *entry.Passage)
	assert.Equal(t, ref.DocumentUID, entry.DocumentUID)
	assert.Equal(t, ref.ContentVersionID, entry.RequestedContentVersionID)
	assert.Equal(t, targetNode.ID, entry.Member.NodeID)
	assert.Equal(t, targetVersions[0], entry.Member.ContentVersionID)
	require.NoError(t, ValidateContentMapSnapshotRecord(snapshot))
	readBack, err := target.ContentMapSnapshotByID(t.Context(), access, snapshot.ID)
	require.NoError(t, err)
	assert.Equal(t, snapshot, readBack)
	rollback := errors.New("rollback synthetic authority change")
	err = target.withStorageTx(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(t.Context(), `ALTER TABLE rendition_units RENAME TO unavailable_map_units`)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(t.Context(), `ALTER TABLE rendition_lexical_segments RENAME TO unavailable_map_segments`)
		if err != nil {
			return err
		}
		entry, err := target.resolveMapPinTx(t.Context(), tx, access, pin)
		require.NoError(t, err, "passage authorization must not load rendition units or segments")
		assert.Equal(t, targetVersions[0], entry.Member.ContentVersionID)
		return rollback
	})
	require.ErrorIs(t, err, rollback)
	err = target.withStorageTx(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(t.Context(), `DELETE FROM rendition_artifacts WHERE build_id=? AND role=?`,
			targetBuild.ID, catalogArtifactSanitizedMarkdown)
		if err != nil {
			return err
		}
		err = target.authorizeMapSnapshotTx(t.Context(), tx, access, definition, snapshot)
		require.ErrorIs(t, err, ErrNotFound, "publication must recheck retained rendition evidence")
		return rollback
	})
	require.ErrorIs(t, err, rollback)
	err = target.withStorageTx(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(t.Context(), `DELETE FROM rendition_attachments WHERE attachment_id=?`, targetAttachment.ID)
		if err != nil {
			return err
		}
		err = target.authorizeMapSnapshotTx(t.Context(), tx, access, definition, snapshot)
		require.ErrorIs(t, err, ErrNotFound, "publication must recheck attachment authority")
		return rollback
	})
	require.ErrorIs(t, err, rollback)
	fullAccess := MapAccess{Owner: "local", AllSources: true}
	fullPlan, err := target.PreviewContentMap(t.Context(), fullAccess, definition)
	require.NoError(t, err)
	fullMap, err := target.CreateContentMap(t.Context(), fullAccess, definition, fullPlan.DefinitionDigest)
	require.NoError(t, err)
	reverseDefinition := mapTestDefinition(pin)
	reverseDefinition.Sections[0].Exclude = []document.ContentMapPin{{
		DocumentUID: targetIdentity.DocumentUID, Mode: document.MapPinVersionPinned,
		ContentVersionID: targetVersions[0],
	}}
	t.Run("reverse adopted exclusion preview", func(t *testing.T) {
		_, err := target.PreviewContentMap(t.Context(), fullAccess, reverseDefinition)
		require.ErrorIs(t, err, ErrInvalidContentMap)
	})
	reversePlan, reverseJSON, err := normalizedMapDefinition(reverseDefinition)
	require.NoError(t, err)
	t.Run("reverse adopted exclusion create", func(t *testing.T) {
		_, err := target.CreateContentMap(t.Context(), fullAccess, reverseDefinition, reversePlan.DefinitionDigest)
		require.ErrorIs(t, err, ErrInvalidContentMap)
	})
	t.Run("reverse adopted exclusion snapshot", func(t *testing.T) {
		id, err := newUUIDv4()
		require.NoError(t, err)
		now := nowRFC3339()
		_, err = target.db.ExecContext(t.Context(), `INSERT INTO content_maps(id,owner,revision,definition_json,definition_digest,created_at,updated_at,archived_at)
			VALUES(?,?,?,?,?,?,?,NULL)`, id, fullAccess.Owner, 1, reverseJSON, reversePlan.DefinitionDigest, now, now)
		require.NoError(t, err)
		_, err = target.CreateContentMapSnapshot(t.Context(), fullAccess, id, 1)
		require.ErrorIs(t, err, ErrInvalidContentMap)
	})
	excludedDefinition := mapTestDefinition()
	excludedDefinition.Sections[0].Exclude = []document.ContentMapPin{pin}
	excludedSelector, err := query.Parse([]byte(`{}`))
	require.NoError(t, err)
	excludedDefinition.Sections[0].Selector = &excludedSelector
	excludedPlan, err := target.PreviewContentMap(t.Context(), fullAccess, excludedDefinition)
	require.NoError(t, err)
	excludedMap, err := target.CreateContentMap(t.Context(), fullAccess, excludedDefinition, excludedPlan.DefinitionDigest)
	require.NoError(t, err)
	excludedSnapshot, err := target.CreateContentMapSnapshot(t.Context(), fullAccess, excludedMap.ID, 1)
	require.NoError(t, err)
	for _, entry := range excludedSnapshot.Sections[0].Entries {
		assert.NotEqual(t, targetVersions[0], entry.Member.ContentVersionID,
			"an adopted exclusion must filter its local source")
	}
	publicSnapshot, err := json.Marshal(excludedSnapshot)
	require.NoError(t, err)
	assert.NotContains(t, string(publicSnapshot), ref.DocumentUID)
	assert.NotContains(t, string(publicSnapshot), "dependencies")
	derivedMaps := make([]ContentMap, 0, 2)
	derivedSnapshots := make([]ContentMapSnapshot, 0, 2)
	for _, field := range []string{"heading", "description"} {
		derived := mapTestDefinition()
		derived.Sections[0].Selector = &excludedSelector
		if field == "heading" {
			derived.Sections[0].HeadingSources = []document.ContentMapPin{pin}
		} else {
			derived.Sections[0].Description = "Copied source summary"
			derived.Sections[0].DescriptionSources = []document.ContentMapPin{pin}
		}
		plan, planErr := target.PreviewContentMap(t.Context(), fullAccess, derived)
		require.NoError(t, planErr)
		created, createErr := target.CreateContentMap(t.Context(), fullAccess, derived, plan.DefinitionDigest)
		require.NoError(t, createErr)
		frozen, freezeErr := target.CreateContentMapSnapshot(t.Context(), fullAccess, created.ID, 1)
		require.NoError(t, freezeErr)
		derivedMaps = append(derivedMaps, created)
		derivedSnapshots = append(derivedSnapshots, frozen)
	}
	_, err = target.ContentMapSnapshotByID(t.Context(), MapAccess{Owner: "scoped",
		PermittedVersionIDs: []string{ref.ContentVersionID}}, snapshot.ID)
	require.ErrorIs(t, err, ErrNotFound, "origin version alone must not grant local snapshot bytes")
	_, err = target.db.ExecContext(t.Context(), `DELETE FROM adopted_passage_authorities
		WHERE domain_uid=? AND source_vault_uid=? AND source_document_uid=?`,
		domain, ref.VaultUID, ref.DocumentUID)
	require.NoError(t, err)
	_, err = target.ContentMapSnapshotByID(t.Context(), access, snapshot.ID)
	require.ErrorIs(t, err, ErrNotFound, "revoked adoption must not leave a falsely attributed snapshot")
	_, err = target.CreateContentMapSnapshot(t.Context(), fullAccess, fullMap.ID, fullMap.Revision)
	require.ErrorIs(t, err, ErrNotFound, "revoked adoption must not become a cited tombstone")
	_, err = target.ContentMapByID(t.Context(), fullAccess, excludedMap.ID)
	require.ErrorIs(t, err, ErrNotFound, "excluded source authority still governs definition reads")
	_, err = target.ContentMapSnapshotByID(t.Context(), fullAccess, excludedSnapshot.ID)
	require.ErrorIs(t, err, ErrNotFound, "excluded source authority still governs frozen prose")
	for i := range derivedMaps {
		_, err = target.ContentMapByID(t.Context(), fullAccess, derivedMaps[i].ID)
		require.ErrorIs(t, err, ErrNotFound, "copied prose source still governs definition reads")
		_, err = target.ContentMapSnapshotByID(t.Context(), fullAccess, derivedSnapshots[i].ID)
		require.ErrorIs(t, err, ErrNotFound, "copied prose source still governs snapshot reads")
	}
}

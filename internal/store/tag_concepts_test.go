package store

import (
	"fmt"
	"io"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/query"
)

func TestConceptAliasesAndHierarchy(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	a, err := s.CreateTag(ctx, "Café/blue")
	require.NoError(t, err)
	b, err := s.CreateTag(ctx, "blue")
	require.NoError(t, err)
	c, err := s.CreateTag(ctx, "green")
	require.NoError(t, err)
	concept, err := s.SetTagConcept(ctx, a.ID, 1, "A synthetic topic")
	require.NoError(t, err)
	require.Equal(t, int64(2), concept.Revision)
	require.Equal(t, "A synthetic topic", concept.Description)
	concept, err = s.AddTagAlias(ctx, a.ID, concept.Revision, "cafe\u0301")
	require.NoError(t, err)
	require.Equal(t, int64(3), concept.Revision)
	resolved, err := s.ResolveTagConcept(ctx, "café")
	require.NoError(t, err)
	require.Equal(t, a.ID, resolved.ID)
	_, err = s.AddTagAlias(ctx, b.ID, 1, "café")
	require.ErrorIs(t, err, ErrExists)
	_, err = s.AddTagAlias(ctx, b.ID, 1, "Café/blue")
	require.ErrorIs(t, err, ErrExists)
	require.NoError(t, s.AddConceptEdge(ctx, a.ID, b.ID, "broader"))
	require.NoError(t, s.AddConceptEdge(ctx, b.ID, c.ID, "broader"))
	require.ErrorIs(t, s.AddConceptEdge(ctx, c.ID, a.ID, "broader"), ErrCycle)
	require.ErrorIs(t, s.AddConceptEdge(ctx, a.ID, a.ID, "broader"), ErrCycle)
	require.NoError(t, s.AddConceptEdge(ctx, a.ID, c.ID, "related"))
	got, err := s.ConceptByTagID(ctx, a.ID)
	require.NoError(t, err)
	require.Equal(t, []string{"café"}, got.Aliases)
	require.Equal(t, []string{b.ID}, got.NarrowerIDs)
	require.Equal(t, []string{c.ID}, got.RelatedIDs)
	require.NoError(t, s.RemoveConceptEdge(ctx, a.ID, c.ID, "related"))
	got, err = s.ConceptByTagID(ctx, c.ID)
	require.NoError(t, err)
	require.Empty(t, got.RelatedIDs)
	got, err = s.RemoveTagAlias(ctx, a.ID, gotConceptRevision(t, s, a.ID), "café")
	require.NoError(t, err)
	require.Empty(t, got.Aliases)
}

func TestTagAliasReservesCanonicalName(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	one, err := s.CreateTag(ctx, "one")
	require.NoError(t, err)
	two, err := s.CreateTag(ctx, "two")
	require.NoError(t, err)
	_, err = s.AddTagAlias(ctx, one.ID, 1, "reserved")
	require.NoError(t, err)
	_, err = s.CreateTag(ctx, "reserved")
	require.ErrorIs(t, err, ErrExists)
	_, err = s.RenameTag(ctx, two.ID, two.Revision, "reserved")
	require.ErrorIs(t, err, ErrExists)
}

func gotConceptRevision(t *testing.T, s *Store, id string) int64 {
	t.Helper()
	concept, err := s.ConceptByTagID(t.Context(), id)
	require.NoError(t, err)
	return concept.Revision
}

func TestTagMergePreviewAndRedirectPreserveQueries(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	source, err := s.CreateTag(ctx, "from")
	require.NoError(t, err)
	target, err := s.CreateTag(ctx, "to")
	require.NoError(t, err)
	node, err := s.Mkdir(ctx, s.RootID(), "synthetic")
	require.NoError(t, err)
	_, err = s.AssignTag(ctx, source.ID, node.ID, node.Revision)
	require.NoError(t, err)
	q, err := query.Parse([]byte(fmt.Sprintf(`{"filters":{"tag_ids":[%q]}}`, source.ID)))
	require.NoError(t, err)
	payload, err := query.Canonical(q)
	require.NoError(t, err)
	saved, err := s.CreateSavedQuery(ctx, "synthetic query", "", SavedQueryKindQuery, payload)
	require.NoError(t, err)
	preview, err := s.PreviewTagMerge(ctx, source.ID, target.ID)
	require.NoError(t, err)
	require.Equal(t, []string{saved.ID}, preview.SavedQueryIDs)
	require.Equal(t, 1, preview.DocumentAssignments)
	require.Equal(t, []int64{node.ID}, preview.DocumentNodeIDs)
	_, err = s.RenameTag(ctx, source.ID, preview.SourceRevision, "renamed")
	require.NoError(t, err)
	_, err = s.CommitTagMerge(ctx, preview)
	require.ErrorIs(t, err, ErrStaleRevision)
	preview, err = s.PreviewTagMerge(ctx, source.ID, target.ID)
	require.NoError(t, err)
	beforeNode, err := s.NodeByID(ctx, node.ID)
	require.NoError(t, err)
	merged, err := s.CommitTagMerge(ctx, preview)
	require.NoError(t, err)
	require.Equal(t, target.ID, merged.TargetTagID)
	redirect, err := s.ResolveTagRedirect(ctx, source.ID)
	require.NoError(t, err)
	require.Equal(t, target.ID, redirect)
	tags, _, err := s.NodeTags(ctx, node.ID, 10, 0)
	require.NoError(t, err)
	require.Equal(t, target.ID, tags[0].ID)
	afterNode, err := s.NodeByID(ctx, node.ID)
	require.NoError(t, err)
	require.Greater(t, afterNode.Revision, beforeNode.Revision)
	updated, err := s.SavedQueryByID(ctx, saved.ID)
	require.NoError(t, err)
	parsed, err := query.Parse(updated.Payload)
	require.NoError(t, err)
	require.Equal(t, []string{target.ID}, parsed.Filters.TagIDs)
}

func TestTagMergeKeepsActiveMailboxLabelTag(t *testing.T) {
	for _, state := range []string{"queued", "running"} {
		t.Run(state, func(t *testing.T) {
			s := newTestStore(t)
			ctx := t.Context()
			source, err := s.CreateTag(ctx, "source")
			require.NoError(t, err)
			target, err := s.CreateTag(ctx, "target")
			require.NoError(t, err)
			preview, err := s.PreviewTagMerge(ctx, source.ID, target.ID)
			require.NoError(t, err)

			container := MailboxContainerRequest{ID: "source-container", Owner: "one", SHA256: fakeHash("aa"), Size: 3, Format: "mbox"}
			_, err = s.BeginMailboxContainer(ctx, container)
			require.NoError(t, err)
			require.NoError(t, s.RecordRenditionBlob(ctx, container.SHA256, container.Size,
				BlobPhysical{Encoding: "raw", StoredBytes: container.Size, PackEligible: true, Created: true}))
			require.NoError(t, s.PutMailboxChunk(ctx, container.Owner, container.ID,
				MailboxChunk{Index: 0, SHA256: container.SHA256, Size: container.Size}))
			_, err = s.SealMailboxContainer(ctx, container.Owner, container.ID, container.SHA256, container.Size)
			require.NoError(t, err)
			request := MailboxJobRequest{ID: "job", ContainerID: container.ID, ContainerSHA256: container.SHA256,
				Settings: MailboxSettings{DestinationID: s.RootID(), LabelTags: map[string]string{"Project": source.ID}}}
			_, err = s.BeginMailboxJob(ctx, container.Owner, request)
			require.NoError(t, err)
			if state == "running" {
				_, err = s.ClaimMailboxJob(ctx)
				require.NoError(t, err)
			}

			_, err = s.CommitTagMerge(ctx, preview)
			require.ErrorIs(t, err, ErrMailboxConflict)
			_, err = s.TagByID(ctx, source.ID)
			require.NoError(t, err)
			job, err := s.MailboxJob(ctx, container.Owner, request.ID)
			require.NoError(t, err)
			require.Equal(t, state, job.State)
			require.Equal(t, source.ID, job.Settings.LabelTags["Project"])
			require.NoError(t, s.ValidateMetadata(ctx))
		})
	}
}

func TestTagMergeConceptDescriptionPolicy(t *testing.T) {
	for _, tc := range []struct {
		name              string
		sourceDescription string
		targetDescription string
		wantDescription   string
		wantConflict      bool
	}{
		{name: "transfer to empty target", sourceDescription: "Source explanation", wantDescription: "Source explanation"},
		{name: "reject different descriptions", sourceDescription: "Source explanation", targetDescription: "Target explanation", wantConflict: true},
		{name: "retain equal descriptions", sourceDescription: "Shared explanation", targetDescription: "Shared explanation", wantDescription: "Shared explanation"},
		{name: "retain target when source empty", targetDescription: "Target explanation", wantDescription: "Target explanation"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestStore(t)
			ctx := t.Context()
			source, err := s.CreateTag(ctx, "source")
			require.NoError(t, err)
			target, err := s.CreateTag(ctx, "target")
			require.NoError(t, err)
			if tc.sourceDescription != "" {
				_, err = s.SetTagConcept(ctx, source.ID, 1, tc.sourceDescription)
				require.NoError(t, err)
			}
			if tc.targetDescription != "" {
				_, err = s.SetTagConcept(ctx, target.ID, 1, tc.targetDescription)
				require.NoError(t, err)
			}
			preview, err := s.PreviewTagMerge(ctx, source.ID, target.ID)
			require.NoError(t, err)
			require.Equal(t, tc.sourceDescription, preview.SourceDescription)
			_, err = s.CommitTagMerge(ctx, preview)
			if tc.wantConflict {
				require.ErrorIs(t, err, ErrInvalidTag)
				retained, lookupErr := s.ConceptByTagID(ctx, source.ID)
				require.NoError(t, lookupErr)
				require.Equal(t, tc.sourceDescription, retained.Description)
				unchanged, lookupErr := s.ConceptByTagID(ctx, target.ID)
				require.NoError(t, lookupErr)
				require.Equal(t, tc.targetDescription, unchanged.Description)
				require.Equal(t, preview.TargetConceptRev, unchanged.Revision)
			} else {
				require.NoError(t, err)
				merged, lookupErr := s.ConceptByTagID(ctx, target.ID)
				require.NoError(t, lookupErr)
				require.Equal(t, tc.wantDescription, merged.Description)
				_, lookupErr = s.TagByID(ctx, source.ID)
				require.ErrorIs(t, lookupErr, ErrNotFound)
			}
			require.NoError(t, s.ValidateMetadata(ctx))
		})
	}
}

func TestTagMergeDescriptionDecisionUsesConceptRevisionFence(t *testing.T) {
	for _, edited := range []string{"source", "target"} {
		t.Run(edited, func(t *testing.T) {
			s := newTestStore(t)
			ctx := t.Context()
			source, err := s.CreateTag(ctx, "source")
			require.NoError(t, err)
			target, err := s.CreateTag(ctx, "target")
			require.NoError(t, err)
			_, err = s.SetTagConcept(ctx, source.ID, 1, "Reviewed source")
			require.NoError(t, err)
			preview, err := s.PreviewTagMerge(ctx, source.ID, target.ID)
			require.NoError(t, err)
			if edited == "source" {
				_, err = s.SetTagConcept(ctx, source.ID, preview.SourceConceptRev, "Revised source")
			} else {
				_, err = s.SetTagConcept(ctx, target.ID, preview.TargetConceptRev, "New target")
			}
			require.NoError(t, err)
			_, err = s.CommitTagMerge(ctx, preview)
			require.ErrorIs(t, err, ErrStaleRevision)
			_, err = s.TagByID(ctx, source.ID)
			require.NoError(t, err)
			require.NoError(t, s.ValidateMetadata(ctx))
		})
	}
}

func TestReverseTagMergeOnlyWhenIdentityMoveIsSafe(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	source, err := s.CreateTag(ctx, "source")
	require.NoError(t, err)
	target, err := s.CreateTag(ctx, "target")
	require.NoError(t, err)
	node, err := s.Mkdir(ctx, s.RootID(), "synthetic")
	require.NoError(t, err)
	_, err = s.AssignTag(ctx, source.ID, node.ID, node.Revision)
	require.NoError(t, err)
	preview, err := s.PreviewTagMerge(ctx, source.ID, target.ID)
	require.NoError(t, err)
	require.Zero(t, preview.TargetDocumentAssignments)
	receipt, err := s.CommitTagMerge(ctx, preview)
	require.NoError(t, err)
	reversed, err := s.ReverseTagMerge(ctx, receipt.MergeID)
	require.NoError(t, err)
	require.Equal(t, source.ID, reversed.SourceTagID)
	resolved, err := s.TagByID(ctx, source.ID)
	require.NoError(t, err)
	require.Equal(t, "source", resolved.Name)
	tags, _, err := s.NodeTags(ctx, node.ID, 10, 0)
	require.NoError(t, err)
	require.Equal(t, []string{source.ID}, []string{tags[0].ID})
	_, err = s.ReverseTagMerge(ctx, receipt.MergeID)
	require.Error(t, err)
}

func TestReverseTagMergeRejectsTransferredConceptDescription(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	source, err := s.CreateTag(ctx, "source")
	require.NoError(t, err)
	target, err := s.CreateTag(ctx, "target")
	require.NoError(t, err)
	_, err = s.SetTagConcept(ctx, source.ID, 1, "Source explanation")
	require.NoError(t, err)
	preview, err := s.PreviewTagMerge(ctx, source.ID, target.ID)
	require.NoError(t, err)
	receipt, err := s.CommitTagMerge(ctx, preview)
	require.NoError(t, err)
	_, err = s.ReverseTagMerge(ctx, receipt.MergeID)
	require.ErrorIs(t, err, ErrInvalidTag)
	_, err = s.TagByID(ctx, source.ID)
	require.ErrorIs(t, err, ErrNotFound)
	concept, err := s.ConceptByTagID(ctx, target.ID)
	require.NoError(t, err)
	require.Equal(t, "Source explanation", concept.Description)
	require.NoError(t, s.ValidateMetadata(ctx))
}

func TestTagMergePreviewReportsAdvancedSavedReference(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	source, err := s.CreateTag(ctx, "source")
	require.NoError(t, err)
	target, err := s.CreateTag(ctx, "target")
	require.NoError(t, err)
	q, err := query.Parse([]byte(fmt.Sprintf(`{"syntax":"advanced","text":%q}`, "tag:"+source.ID)))
	require.NoError(t, err)
	payload, err := query.Canonical(q)
	require.NoError(t, err)
	saved, err := s.CreateSavedQuery(ctx, "advanced", "", SavedQueryKindQuery, payload)
	require.NoError(t, err)
	preview, err := s.PreviewTagMerge(ctx, source.ID, target.ID)
	require.NoError(t, err)
	require.Equal(t, []string{saved.ID}, preview.SavedQueryIDs)
	require.Equal(t, []string{saved.ID}, preview.UnmigratableSavedQueryIDs)
	_, err = s.CommitTagMerge(ctx, preview)
	require.ErrorIs(t, err, ErrInvalidTag)
}

func TestTagMergePreviewsAndRewritesContentMapSelectors(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	source, err := s.CreateTag(ctx, "source")
	require.NoError(t, err)
	target, err := s.CreateTag(ctx, "target")
	require.NoError(t, err)
	node, err := s.CreateFile(ctx, s.RootID(), "synthetic.txt", testSHA256([]byte("map-merge")), 10, "text/plain")
	require.NoError(t, err)
	identity, err := s.EnsureDocumentIdentity(ctx, node.ID)
	require.NoError(t, err)
	definition := mapTestDefinition(document.ContentMapPin{DocumentUID: identity.DocumentUID, Mode: document.MapPinFollowCurrent})
	selector := query.Query{V: 1, Syntax: "simple", Mode: "lexical", Sort: query.Sort{Field: "name", Direction: "asc"}, Filters: query.Filters{TagIDs: []string{source.ID}}}
	definition.Sections[0].Selector = &selector
	access := MapAccess{Owner: "local", AllSources: true}
	plan, err := s.PreviewContentMap(ctx, access, definition)
	require.NoError(t, err)
	contentMap, err := s.CreateContentMap(ctx, access, definition, plan.DefinitionDigest)
	require.NoError(t, err)
	preview, err := s.PreviewTagMerge(ctx, source.ID, target.ID)
	require.NoError(t, err)
	require.Equal(t, []string{contentMap.ID}, preview.ContentMapIDs)
	definition.Title = "Revised synthetic research"
	revisedPlan, err := s.PreviewContentMap(ctx, access, definition)
	require.NoError(t, err)
	contentMap, err = s.UpdateContentMap(ctx, access, contentMap.ID, contentMap.Revision, definition, revisedPlan.DefinitionDigest)
	require.NoError(t, err)
	_, err = s.CommitTagMerge(ctx, preview)
	require.ErrorIs(t, err, ErrStaleRevision)
	preview, err = s.PreviewTagMerge(ctx, source.ID, target.ID)
	require.NoError(t, err)
	_, err = s.CommitTagMerge(ctx, preview)
	require.NoError(t, err)
	updated, err := s.ContentMapByID(ctx, access, contentMap.ID)
	require.NoError(t, err)
	require.Equal(t, []string{target.ID}, updated.Definition.Sections[0].Selector.Filters.TagIDs)
	require.Equal(t, contentMap.Revision+1, updated.Revision)
	require.NoError(t, s.ExportMetadata(ctx, io.Discard))
}

func TestTagMergeRejectsAdvancedContentMapReference(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	source, err := s.CreateTag(ctx, "source")
	require.NoError(t, err)
	target, err := s.CreateTag(ctx, "target")
	require.NoError(t, err)
	node, err := s.CreateFile(ctx, s.RootID(), "synthetic.txt", testSHA256([]byte("advanced-map-merge")), 10, "text/plain")
	require.NoError(t, err)
	identity, err := s.EnsureDocumentIdentity(ctx, node.ID)
	require.NoError(t, err)
	definition := mapTestDefinition(document.ContentMapPin{DocumentUID: identity.DocumentUID, Mode: document.MapPinFollowCurrent})
	definition.Sections[0].Selector = &query.Query{V: 1, Syntax: "advanced", Mode: "lexical",
		Text: "tag:" + source.ID, Sort: query.Sort{Field: "name", Direction: "asc"}}
	access := MapAccess{Owner: "local", AllSources: true}
	plan, err := s.PreviewContentMap(ctx, access, definition)
	require.NoError(t, err)
	contentMap, err := s.CreateContentMap(ctx, access, definition, plan.DefinitionDigest)
	require.NoError(t, err)
	preview, err := s.PreviewTagMerge(ctx, source.ID, target.ID)
	require.NoError(t, err)
	require.Equal(t, []string{contentMap.ID}, preview.UnmigratableContentMapIDs)
	_, err = s.CommitTagMerge(ctx, preview)
	require.ErrorIs(t, err, ErrInvalidTag)
}

func TestTagMergeRedirectChainSurvivesLaterMerge(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	a, err := s.CreateTag(ctx, "a")
	require.NoError(t, err)
	b, err := s.CreateTag(ctx, "b")
	require.NoError(t, err)
	c, err := s.CreateTag(ctx, "c")
	require.NoError(t, err)
	first, err := s.PreviewTagMerge(ctx, a.ID, b.ID)
	require.NoError(t, err)
	_, err = s.CommitTagMerge(ctx, first)
	require.NoError(t, err)
	second, err := s.PreviewTagMerge(ctx, b.ID, c.ID)
	require.NoError(t, err)
	_, err = s.CommitTagMerge(ctx, second)
	require.NoError(t, err)
	resolved, err := s.ResolveTagRedirect(ctx, a.ID)
	require.NoError(t, err)
	require.Equal(t, c.ID, resolved)
}

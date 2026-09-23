package store

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestConceptFederationExtensionKeepsOriginAndIdentity(t *testing.T) {
	first := newTestStore(t)
	second := newTestStore(t)
	ctx := t.Context()
	firstTag, err := first.CreateTag(ctx, "shared name")
	require.NoError(t, err)
	secondTag, err := second.CreateTag(ctx, "shared name")
	require.NoError(t, err)
	require.NotEqual(t, firstTag.ID, secondTag.ID)
	_, err = first.AddTagAlias(ctx, firstTag.ID, 1, "alternate")
	require.NoError(t, err)
	domain := "00000000-0000-4000-8000-000000000001"
	firstExport, err := first.ExportConceptFederationExtension(ctx, domain, true)
	require.NoError(t, err)
	secondExport, err := second.ExportConceptFederationExtension(ctx, domain, true)
	require.NoError(t, err)
	require.Equal(t, 1, firstExport.Version)
	require.Equal(t, domain, firstExport.DomainUID)
	require.NotEqual(t, firstExport.OriginVaultUID, secondExport.OriginVaultUID)
	require.NotEqual(t, firstExport.Tags[0].TagID, secondExport.Tags[0].TagID)
	require.Equal(t, "shared name", firstExport.Tags[0].Name)
	require.Equal(t, "shared name", secondExport.Tags[0].Name)
	require.Len(t, firstExport.Aliases, 1)
	require.ErrorIs(t, ValidateConceptFederationExtension(firstExport, false), ErrConceptFederationAuthority)
	require.NoError(t, ValidateConceptFederationExtension(firstExport, true))
	firstExport.Version = 2
	require.Error(t, ValidateConceptFederationExtension(firstExport, true))
}

func TestConceptFederationExtensionRejectsCyclesAndDuplicateAuthority(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	a, err := s.CreateTag(ctx, "a")
	require.NoError(t, err)
	b, err := s.CreateTag(ctx, "b")
	require.NoError(t, err)
	extension, err := s.ExportConceptFederationExtension(ctx, "00000000-0000-4000-8000-000000000001", true)
	require.NoError(t, err)
	extension.Edges = []ConceptFederationEdge{{a.ID, b.ID, "broader"}, {b.ID, a.ID, "broader"}}
	require.Error(t, ValidateConceptFederationExtension(extension, true))
	extension.Edges = nil
	extension.Concepts = []ConceptFederationDetail{{a.ID, "first", 1}, {a.ID, "second", 1}}
	require.Error(t, ValidateConceptFederationExtension(extension, true))
}

func TestConceptFederationExtensionRejectsRedirectWithoutMerge(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	target, err := s.CreateTag(ctx, "target")
	require.NoError(t, err)
	extension, err := s.ExportConceptFederationExtension(ctx, "00000000-0000-4000-8000-000000000001", true)
	require.NoError(t, err)
	extension.Redirects = []ConceptFederationRedirect{{
		SourceTagID: "00000000-0000-4000-8000-000000000002",
		TargetTagID: target.ID,
		SourceName:  "source",
		MergedAt:    "2026-09-23T00:00:00.000000000Z",
		MergeID:     "00000000-0000-4000-8000-000000000003",
	}}
	require.Error(t, ValidateConceptFederationExtension(extension, true))
}

func TestConceptFederationExtensionExportsRedirectChain(t *testing.T) {
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

	extension, err := s.ExportConceptFederationExtension(ctx, "00000000-0000-4000-8000-000000000001", true)
	require.NoError(t, err)
	require.Len(t, extension.Redirects, 2)
	require.Len(t, extension.MergeAudits, 2)
	require.Equal(t, c.ID, extension.Redirects[0].TargetTagID)
	require.Equal(t, c.ID, extension.Redirects[1].TargetTagID)
	require.NoError(t, ValidateConceptFederationExtension(extension, true))
}

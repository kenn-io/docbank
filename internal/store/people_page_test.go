package store

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestPeoplePageRequiresVisiblePhotoEvidence(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	contact, err := s.CreatePerson(ctx, "A Contact", "operator")
	require.NoError(t, err)
	depicted, err := s.CreatePerson(ctx, "B Depicted", "operator")
	require.NoError(t, err)
	node := browsePhotoNode(t, s, "portrait.jpg", fakeHash("page-portrait"), "image/jpeg")
	_, err = s.MarkPersonInPhoto(ctx, node.CurrentVersionID, depicted.PersonID, "")
	require.NoError(t, err)
	// Ordinary name searches retain contacts without photos.
	all, err := s.PeopleByDisplayName(ctx, "", "", "", 24, true, PeopleSearchOptions{})
	require.NoError(t, err)
	require.Len(t, all, 2)
	require.Equal(t, contact.PersonID, all[0].PersonID)
	require.Equal(t, depicted.PersonID, all[1].PersonID)
	// The Photos page must filter before its page limit.
	page, err := s.PeopleByDisplayName(ctx, "", "", "", 1, true, PeopleSearchOptions{PhotoSummary: true, GridRecipe: "grid"})
	require.NoError(t, err)
	require.Len(t, page, 1)
	require.Equal(t, depicted.PersonID, page[0].PersonID)
}

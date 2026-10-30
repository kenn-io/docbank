package processing

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestPhotoDescriptionUndecodablePreviewReason(t *testing.T) {
	t.Parallel()
	f := newPhotoDescriptionFixture(t)
	node := publishRejectedPreview(t, f)("invalid-preview.jpg")
	version, err := f.catalog.ContentVersionByID(t.Context(), node.CurrentVersionID)
	require.NoError(t, err)
	_, capability, err := photoDescriptionInput(t.Context(), f.catalog, f.blobs, version, f.service.profiles["vision"])
	require.ErrorIs(t, err, ErrSourceIneligible)
	require.EqualValues(t, "preview_unavailable", capability.Reason)
	capability, err = f.service.planPhotoDescription(t.Context(), version, f.service.profiles["vision"])
	require.NoError(t, err)
	require.False(t, capability.Eligible)
	require.EqualValues(t, "preview_unavailable", capability.Reason)
}

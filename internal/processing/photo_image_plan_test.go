package processing

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestImageOnlyPhotoPlanDisclosesVaultScope(t *testing.T) {
	t.Parallel()
	f := newPhotoDescriptionFixture(t)
	x := newImageEmbeddingService(t, f)
	plan, err := x.service.Plan(t.Context(), f.selector("images"))
	require.NoError(t, err)
	require.Equal(t, "Granting consent automatically queues eligible photos throughout this vault.", plan.Estimate.Scope)
	require.Equal(t, []string{"byte_length", "content_hash", "detected_media_type"}, plan.Flow[0].RuntimeDisclosure.MetadataClasses)
}

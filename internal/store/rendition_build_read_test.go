package store

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRenditionBuildReadDormantAndValidated(t *testing.T) {
	s, versions := newRenditionCatalogFixture(t)
	profile := catalogProcessingProfile(t, false)
	build := catalogRenditionBuild(s, profile)
	_, err := s.RenditionBuild(t.Context(), build.ID)
	require.ErrorIs(t, err, ErrNotFound)
	_, err = s.RenditionBuild(t.Context(), "invalid")
	require.Error(t, err)
	require.NoError(t, s.StageRenditionBuild(t.Context(), build))
	got, err := s.RenditionBuild(t.Context(), build.ID)
	require.NoError(t, err)
	require.Equal(t, build, got)
	_, err = s.ActiveRendition(t.Context(), versions[0], profile.Fingerprint)
	require.ErrorIs(t, err, ErrNotFound)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = s.RenditionBuild(ctx, build.ID)
	require.ErrorIs(t, err, context.Canceled)
	// The private fixture deliberately bypasses immutable-row protection so the
	// read seam's validation and vault binding can be exercised against corrupt
	// stored state.
	_, err = s.db.Exec("DROP TRIGGER rendition_builds_immutable_update")
	require.NoError(t, err)
	for _, tc := range []struct{ field, value string }{{"completed_at", "invalid"}} {
		t.Run(tc.field, func(t *testing.T) {
			var original string
			require.NoError(t, s.db.QueryRow("SELECT "+tc.field+" FROM rendition_builds WHERE build_id=?", build.ID).Scan(&original))
			_, err := s.db.Exec("UPDATE rendition_builds SET "+tc.field+"=? WHERE build_id=?", tc.value, build.ID)
			require.NoError(t, err)
			_, err = s.RenditionBuild(t.Context(), build.ID)
			require.Error(t, err)
			_, err = s.db.Exec("UPDATE rendition_builds SET "+tc.field+"=? WHERE build_id=?", original, build.ID)
			require.NoError(t, err)
		})
	}
	originalVaultID := s.vaultID
	s.vaultID = "00000000-0000-4000-8000-000000000000"
	_, err = s.RenditionBuild(t.Context(), build.ID)
	require.ErrorContains(t, err, "not store vault")
	s.vaultID = originalVaultID
}

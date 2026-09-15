package store

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestMediaSourceKeyKeepsRemoteIdentitiesSeparate(t *testing.T) {
	vault := "00000000-0000-4000-8000-000000000001"
	a, err := MediaSourceKey("remote_recording", vault, "cap.cloud", "app-a", "recording-a")
	require.NoError(t, err)
	b, err := MediaSourceKey("remote_recording", vault, "cap.cloud", "app-a", "recording-b")
	require.NoError(t, err)
	require.NotEqual(t, a, b)
	c, err := MediaSourceKey("remote_recording", vault, "cap.cloud", "app-b", "recording-a")
	require.NoError(t, err)
	require.NotEqual(t, a, c)
	_, err = MediaSourceKey("supplied_media", vault, "cap.cloud", "", strings.Repeat("a", 64))
	require.Error(t, err)

	s := newTestStore(t)
	var n int
	require.NoError(t, s.db.QueryRow(`SELECT count(*) FROM sqlite_schema
		WHERE type='table' AND name IN ('media_sources','media_source_versions',
		'media_source_heads','media_occurrences','media_visibility_fences',
		'media_input_artifacts','media_operations','media_acquisitions','media_protected_refs')`).Scan(&n))
	require.Equal(t, 9, n)
}

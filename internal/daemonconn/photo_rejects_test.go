package daemonconn

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/store"
)

func TestPhotoRejectsResponse(t *testing.T) {
	t.Parallel()
	digest := strings.Repeat("a", 64)
	valid := api.PhotoRejectsPreflight{Digest: digest, Photos: 2, Movable: 2, Files: 3}
	require.NoError(t, validatePhotoRejectsResponse(&valid, digest))
	valid.Unchanged = 10001
	require.NoError(t, validatePhotoRejectsResponse(&valid, digest))
	valid.Photos, valid.Files, valid.Movable = 1001, 1001, 1000
	require.NoError(t, validatePhotoRejectsResponse(&valid, ""))
	require.NoError(t, validatePhotoRejectsResponse(&valid, digest))
	require.Error(t, validatePhotoRejectsResponse(nil, ""))
	require.Error(t, validatePhotoRejectsResponse(&valid, strings.Repeat("b", 64)))
	for _, bad := range []api.PhotoRejectsPreflight{
		{Digest: "bad"}, {Digest: digest, Photos: 2, Files: 1}, {Digest: digest, MixedCount: 1},
		{Digest: digest, Movable: -1}, {Digest: digest, Photos: 1, Files: 1, Movable: 2},
		{Digest: digest, Photos: 1001, Files: 1001, Movable: 1001},
		{Digest: digest, Unchanged: 21, MixedCount: 21, Mixed: make([]store.PhotoRejectMixed, 21)},
		{Digest: strings.Repeat("A", 64)}, {Digest: digest, Unchanged: -1},
	} {
		require.Error(t, validatePhotoRejectsResponse(&bad, ""))
	}
}

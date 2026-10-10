package daemonconn

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/api"
)

func TestPhotoRejectsResponse(t *testing.T) {
	t.Parallel()
	digest := strings.Repeat("a", 64)
	valid := api.PhotoRejectsPreflight{Digest: digest, Photos: 2, Files: 3}
	require.NoError(t, validatePhotoRejectsResponse(&valid, digest))
	valid.Unchanged = 10001
	require.NoError(t, validatePhotoRejectsResponse(&valid, digest))
	require.Error(t, validatePhotoRejectsResponse(nil, ""))
	require.Error(t, validatePhotoRejectsResponse(&valid, strings.Repeat("b", 64)))
	for _, bad := range []api.PhotoRejectsPreflight{
		{Digest: "bad"}, {Digest: digest, Photos: 2, Files: 1}, {Digest: digest, Files: 1001},
		{Digest: strings.Repeat("A", 64)}, {Digest: digest, Unchanged: -1},
	} {
		require.Error(t, validatePhotoRejectsResponse(&bad, ""))
	}
}

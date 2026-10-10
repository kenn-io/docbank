package daemonconn

import (
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/store"
)

func TestPhotoRejectsResponse(t *testing.T) {
	t.Parallel()
	targets := []store.PhotoRejectTarget{{AssetID: "11111111-1111-4111-8111-111111111111", Revision: 1, MemberRevision: 2}}
	valid := api.PhotoRejectsPreflight{Targets: targets, Photos: 2, Files: 3, Unchanged: 10001}
	require.NoError(t, validatePhotoRejectsResponse(&valid))
	require.Error(t, validatePhotoRejectsResponse(nil))
	for _, bad := range []api.PhotoRejectsPreflight{
		{Targets: targets, Photos: 2, Files: 1}, {Targets: targets, MixedCount: 1},
		{Targets: targets, Photos: 0},
		{Targets: targets, Unchanged: 21, MixedCount: 21, Mixed: make([]store.PhotoRejectMixed, 21)},
		{Targets: targets, Unchanged: -1},
	} {
		require.Error(t, validatePhotoRejectsResponse(&bad))
	}
}

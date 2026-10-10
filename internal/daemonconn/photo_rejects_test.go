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
	valid := api.PhotoRejectsPreflight{Targets: targets, Photos: 2, Movable: 1, Files: 3, Unchanged: 10001}
	require.NoError(t, validatePhotoRejectsResponse(&valid))
	require.Error(t, validatePhotoRejectsResponse(nil))
	for _, bad := range []api.PhotoRejectsPreflight{
		{Targets: targets, Photos: 2, Files: 1}, {Targets: targets, MixedCount: 1},
		{Targets: targets, Movable: -1}, {Targets: targets, Photos: 1, Files: 1, Movable: 2},
		{Targets: targets, Unchanged: 21, MixedCount: 21, Mixed: make([]store.PhotoRejectMixed, 21)},
		{Targets: targets, Unchanged: -1},
	} {
		require.Error(t, validatePhotoRejectsResponse(&bad))
	}
	for _, bad := range [][]store.PhotoRejectTarget{nil, make([]store.PhotoRejectTarget, 1001), {targets[0], targets[0]}, {{AssetID: "bad", Revision: 1, MemberRevision: 1}}, {{AssetID: targets[0].AssetID, Revision: 0, MemberRevision: 1}}, {{AssetID: targets[0].AssetID, Revision: 1}}} {
		require.Error(t, validatePhotoRejectTargets(bad))
	}
}

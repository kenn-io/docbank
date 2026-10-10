package daemonconn

import (
	"fmt"
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
	mixed := make([]store.PhotoRejectMixed, 21)
	for i := range mixed {
		mixed[i] = store.PhotoRejectMixed{
			AssetID: fmt.Sprintf("22222222-2222-4222-8222-%012d", i),
			Members: []store.PhotoRejectMember{
				{FileID: fmt.Sprintf("33333333-3333-4333-8333-%012d", i*2), Flag: "reject"},
				{FileID: fmt.Sprintf("33333333-3333-4333-8333-%012d", i*2+1), Flag: "pick"},
			},
		}
	}
	for _, bad := range []api.PhotoRejectsPreflight{
		{Targets: targets, Photos: 2, Files: 1}, {Targets: targets, Photos: 1, Files: 1, MixedCount: 1},
		{Targets: targets, Photos: 0},
		{Targets: targets, Photos: 1, Files: 1, Unchanged: 21, MixedCount: 21, Mixed: mixed},
	} {
		require.Error(t, validatePhotoRejectsResponse(&bad))
	}
}

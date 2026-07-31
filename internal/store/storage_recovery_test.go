package store

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/packstore"
)

func TestStorageRecoveryRejectsDestinationAuthorityChangedDuringPublication(
	t *testing.T,
) {
	s := newTestStore(t)
	ctx := t.Context()
	secondary, err := s.PrepareSecondaryBlobStore(
		"archive", "filesystem", "archive_nas",
	)
	require.NoError(t, err)
	require.NoError(t, s.RegisterBlobStore(ctx, secondary))

	hash := fakeHash("abcd")
	_, err = s.db.Exec(
		`INSERT INTO blobs(hash,size,created_at) VALUES(?,4,?)`,
		hash, nowRFC3339(),
	)
	require.NoError(t, err)
	_, err = s.db.Exec(`
		INSERT INTO blob_locations(
			blob_hash,store_id,generation,kind,encoding,stored_size,pack_eligible
		) VALUES
			(?,?,?,'loose','raw',4,1),
			(?,?,?,'loose','raw',4,1)`,
		hash, s.primaryStoreID, "40000000-0000-4000-8000-000000000001",
		hash, secondary.ID, "40000000-0000-4000-8000-000000000002",
	)
	require.NoError(t, err)

	plan, err := s.PlanStorageRecovery(ctx, "repair", hash, secondary.ID)
	require.NoError(t, err)
	planJSON, err := json.Marshal(plan)
	require.NoError(t, err)
	operation, err := s.CreateStorageOperation(ctx, StorageOperationCreate{
		Kind: "repair", RequestDigest: plan.Digest,
		RequestJSON: string(planJSON), PlanJSON: string(planJSON), TotalObjects: 1,
	})
	require.NoError(t, err)
	_, err = s.ClaimStorageOperation(ctx, operation.ID)
	require.NoError(t, err)
	require.NoError(t, s.BeginStorageRecoveryPublication(ctx, operation.ID, plan))
	require.NoError(t, s.BeginStorageRecoveryPublication(ctx, operation.ID, plan))

	_, err = s.db.Exec(`
		UPDATE blob_locations SET generation=?
		WHERE blob_hash=? AND store_id=?`,
		"40000000-0000-4000-8000-000000000003", hash, secondary.ID,
	)
	require.NoError(t, err)
	err = s.CommitStorageRecovery(ctx, operation.ID, plan, packstore.ReadLocation{
		StoreID:    packstore.StoreID(secondary.ID),
		Generation: "40000000-0000-4000-8000-000000000004",
		Loose: &packstore.LooseLocation{
			Encoding:    packstore.LooseEncodingRaw,
			LogicalSize: 4,
			StoredSize:  4,
		},
	})
	require.ErrorIs(t, err, ErrStaleRevision)
}

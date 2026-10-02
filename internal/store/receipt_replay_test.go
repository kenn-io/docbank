package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document/bundle"
	"uuid"
)

func TestReplayReceiptDecidesEachLookupOutcome(t *testing.T) {
	t.Parallel()
	conflict := errors.New("synthetic conflict")
	for _, tc := range []struct {
		name      string
		lookupErr error
		same      bool
		want      string
		found     bool
		err       error
	}{
		{name: "bare not found", lookupErr: ErrNotFound, same: true},
		{name: "wrapped not found", lookupErr: fmt.Errorf("document event rebuild %q: %w", "id", ErrNotFound), same: true},
		{name: "no rows", lookupErr: sql.ErrNoRows, same: true},
		{name: "canceled", lookupErr: context.Canceled, same: true, err: context.Canceled},
		{name: "corrupt and different", lookupErr: ErrEmailCorrupt, err: ErrEmailCorrupt},
		{name: "found and different", err: conflict},
		{name: "found and same", same: true, want: "stored", found: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, found, err := replayReceipt("stored", tc.lookupErr, tc.same, conflict)
			require.Equal(t, tc.want, got)
			require.Equal(t, tc.found, found)
			if tc.err == nil {
				require.NoError(t, err)
				return
			}
			require.ErrorIs(t, err, tc.err)
			if !errors.Is(tc.err, conflict) {
				require.NotErrorIs(t, err, conflict)
			}
		})
	}
}

func TestPackageImportLookupReportsUnknownOperation(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	operationID := uuid.New().String()
	_, err := s.PackageImportJob(t.Context(), "owner", operationID)
	require.ErrorIs(t, err, ErrNotFound)
	_, err = s.CancelPackageImportJob(t.Context(), "owner", operationID)
	require.ErrorIs(t, err, ErrNotFound)
}

func TestExportSourceReplayStillExpires(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	n, err := s.CreateFile(t.Context(), s.RootID(), "synthetic.txt", fakeHash("f1"), 12, "text/plain")
	require.NoError(t, err)
	request := bundle.SourceRequest{OperationID: uuid.New().String(), Kind: "explicit", Members: []bundle.Member{{NodeID: n.ID, VersionID: n.CurrentVersionID, SHA256: n.BlobHash, Size: n.Size}}}
	source, err := s.CreateExportSource(t.Context(), "owner", request, nil)
	require.NoError(t, err)
	require.Equal(t, "sealed", source.State)
	past := "2000-01-01T00:00:00.000000000Z"
	_, err = s.db.ExecContext(t.Context(), `UPDATE export_sources SET expires_at=?,canonical_json=json_set(canonical_json,'$.expires_at',?) WHERE id=?`, past, past, source.ID)
	require.NoError(t, err)
	_, err = s.CreateExportSource(t.Context(), "owner", request, nil)
	require.ErrorIs(t, err, bundle.ErrExpired)
}

func TestQueueExportJobReplaysAndRejectsChangedRequest(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	n, err := s.CreateFile(t.Context(), s.RootID(), "synthetic.txt", fakeHash("f2"), 12, "text/plain")
	require.NoError(t, err)
	source, err := s.CreateExportSource(t.Context(), "owner", bundle.SourceRequest{OperationID: uuid.New().String(), Kind: "explicit", Members: []bundle.Member{{NodeID: n.ID, VersionID: n.CurrentVersionID, SHA256: n.BlobHash, Size: n.Size}}}, nil)
	require.NoError(t, err)
	plan, err := s.CreateExportPlan(t.Context(), "owner", bundle.PlanRequest{OperationID: uuid.New().String(), SourceID: source.ID, MemberHash: source.MemberHash, Roles: []bundle.RolePolicy{{Role: "original"}}})
	require.NoError(t, err)
	request := bundle.JobRequest{OperationID: uuid.New().String(), PlanID: plan.ID, Fingerprint: plan.Fingerprint}
	job, err := s.QueueExportJob(t.Context(), "owner", request)
	require.NoError(t, err)
	replayed, err := s.QueueExportJob(t.Context(), "owner", request)
	require.NoError(t, err)
	require.Equal(t, job, replayed)

	changed := request
	changed.Fingerprint = fakeHash("f3")
	_, err = s.QueueExportJob(t.Context(), "owner", changed)
	require.ErrorIs(t, err, bundle.ErrConflict)
	_, err = s.QueueExportJob(t.Context(), "other", changed)
	require.ErrorIs(t, err, ErrNotFound, "owner mismatch answers before the request compare")
	require.NotErrorIs(t, err, bundle.ErrConflict)
}

func TestBatesReservationReplaysAndRejectsChangedRequest(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	snapshot, inputs := batesFixture(t, s)
	ns, err := s.EnsureBatesNamespace(t.Context(), "OUR", "", 6)
	require.NoError(t, err)
	request := batesRequest(t, ns, snapshot, inputs)
	allocation, err := s.ReserveBatesRange(t.Context(), request)
	require.NoError(t, err)
	replayed, err := s.ReserveBatesRange(t.Context(), request)
	require.NoError(t, err)
	require.Equal(t, allocation, replayed)

	changed := request
	changed.StartAt = 5
	_, err = s.ReserveBatesRange(t.Context(), changed)
	require.ErrorIs(t, err, ErrBatesReservationConflict)
	var allocations int
	require.NoError(t, s.db.QueryRowContext(t.Context(), `SELECT count(*) FROM bates_allocations`).Scan(&allocations))
	require.Equal(t, 1, allocations)
}

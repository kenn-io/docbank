package store

import (
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
)

func TestPeopleRebuildReplay(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	operationID, err := newUUIDv4()
	require.NoError(t, err)

	first, err := s.RebuildDocumentPeople(t.Context(), operationID)
	require.NoError(t, err)
	second, err := s.RebuildDocumentPeople(t.Context(), operationID)
	require.NoError(t, err)
	require.Equal(t, first, second)
	require.Equal(t, "running", first.State)
	require.Equal(t, int64(2), first.TargetEpoch)

	var count int
	require.NoError(t, s.db.QueryRow(
		`SELECT COUNT(*) FROM document_people_builds WHERE operation_id=?`, operationID,
	).Scan(&count))
	require.Equal(t, 1, count)
}

func TestDocumentPeopleCoverageCountsCurrentVersionWaitingForEvents(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ingestDocumentEventTarget(t, s, "pending.txt", "b1")
	coverage, err := s.DocumentPeopleCoverage(t.Context())
	require.NoError(t, err)
	require.Equal(t, int64(1), coverage.Pending)
	require.Zero(t, coverage.Published)
}

func TestPeopleRebuildWaitsForFailureRetryAndLateNodeRevision(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	version := seedDocumentPeopleEvent(t, s, "rebuild.txt", "a1", nil)
	operationID, err := newUUIDv4()
	require.NoError(t, err)
	_, err = s.RebuildDocumentPeople(t.Context(), operationID)
	require.NoError(t, err)

	input, err := s.PrepareDocumentPeopleInputs(t.Context(), version.ID)
	require.NoError(t, err)
	require.NoError(t, s.MarkDocumentPeopleFailed(t.Context(), input, "derivation_failed"))
	require.NoError(t, s.RefreshDocumentPeopleBuilds(t.Context()))
	build, err := s.DocumentPeopleBuild(t.Context(), operationID)
	require.NoError(t, err)
	require.Equal(t, "running", build.State)
	require.Equal(t, int64(1), build.Scanned)
	require.Equal(t, int64(1), build.Failed)
	targets, err := s.MissingDocumentPeopleTargetsAfter(t.Context(), document.PersonResolverFingerprint(), "", 25)
	require.NoError(t, err)
	require.Len(t, targets, 1)

	_, err = s.PublishDocumentPeople(t.Context(), documentPeoplePublicationForInput(t, input))
	require.NoError(t, err)
	_, _, err = s.Trash(t.Context(), version.NodeID, version.NodeRevision)
	require.NoError(t, err)
	require.NoError(t, s.RefreshDocumentPeopleBuilds(t.Context()))
	build, err = s.DocumentPeopleBuild(t.Context(), operationID)
	require.NoError(t, err)
	require.Equal(t, "running", build.State, "a node change before the worker cursor keeps the build open")
	coverage, err := s.DocumentPeopleCoverage(t.Context())
	require.NoError(t, err)
	require.Equal(t, int64(1), coverage.Pending)
	require.Zero(t, coverage.Published)

	input, err = s.PrepareDocumentPeopleInputs(t.Context(), version.ID)
	require.NoError(t, err)
	_, err = s.PublishDocumentPeople(t.Context(), documentPeoplePublicationForInput(t, input))
	require.NoError(t, err)
	require.NoError(t, s.RefreshDocumentPeopleBuilds(t.Context()))
	build, err = s.DocumentPeopleBuild(t.Context(), operationID)
	require.NoError(t, err)
	require.Equal(t, "completed", build.State)
	require.Equal(t, int64(1), build.Published)
	require.Zero(t, build.Failed)
	require.NotEmpty(t, build.FinishedAt)

	coverage, err = s.DocumentPeopleCoverage(t.Context())
	require.NoError(t, err)
	require.Equal(t, int64(1), coverage.Published)
	require.Zero(t, coverage.Pending)
}

func TestPeopleRebuildCountsFailureAfterNodeRevisionChanges(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	version := seedDocumentPeopleEvent(t, s, "changed-failure.txt", "a2", nil)
	operationID, err := newUUIDv4()
	require.NoError(t, err)
	_, err = s.RebuildDocumentPeople(t.Context(), operationID)
	require.NoError(t, err)
	_, _, err = s.Trash(t.Context(), version.NodeID, version.NodeRevision)
	require.NoError(t, err)

	input, err := s.PrepareDocumentPeopleInputs(t.Context(), version.ID)
	require.NoError(t, err)
	require.NoError(t, s.MarkDocumentPeopleFailed(t.Context(), input, "derivation_failed"))
	require.NoError(t, s.RefreshDocumentPeopleBuilds(t.Context()))
	build, err := s.DocumentPeopleBuild(t.Context(), operationID)
	require.NoError(t, err)
	require.Equal(t, "running", build.State)
	require.Equal(t, int64(1), build.Scanned)
	require.Equal(t, int64(1), build.Failed)
	coverage, err := s.DocumentPeopleCoverage(t.Context())
	require.NoError(t, err)
	require.Equal(t, int64(1), coverage.Failed)
	require.Zero(t, coverage.Pending)
}

func TestPeopleRebuildStopsRetryingTerminalFailure(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	version := seedDocumentPeopleEvent(t, s, "changed-unavailable.txt", "a3", nil)
	operationID, err := newUUIDv4()
	require.NoError(t, err)
	_, err = s.RebuildDocumentPeople(t.Context(), operationID)
	require.NoError(t, err)
	_, _, err = s.Trash(t.Context(), version.NodeID, version.NodeRevision)
	require.NoError(t, err)

	input, err := s.PrepareDocumentPeopleInputs(t.Context(), version.ID)
	require.NoError(t, err)
	require.NoError(t, s.MarkDocumentPeopleFailed(t.Context(), input, "input_over_limit"))
	require.NoError(t, s.RefreshDocumentPeopleBuilds(t.Context()))
	build, err := s.DocumentPeopleBuild(t.Context(), operationID)
	require.NoError(t, err)
	require.Equal(t, "failed", build.State)
	require.Equal(t, int64(1), build.Scanned)
	require.Equal(t, int64(1), build.Failed)
	coverage, err := s.DocumentPeopleCoverage(t.Context())
	require.NoError(t, err)
	require.Equal(t, int64(1), coverage.Unavailable)
	require.Zero(t, coverage.Pending)
	targets, err := s.MissingDocumentPeopleTargetsAfter(t.Context(), document.PersonResolverFingerprint(), "", 25)
	require.NoError(t, err)
	require.Empty(t, targets)
}

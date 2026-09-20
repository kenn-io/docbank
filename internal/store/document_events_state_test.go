package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
)

func TestDocumentEventDeriverFingerprint(t *testing.T) {
	sum := sha256.Sum256([]byte(DocumentEventsDeriverDescriptor))
	require.Equal(t, DocumentEventsDeriverFingerprint, hex.EncodeToString(sum[:]))
	require.Equal(t,
		"docbank-document-events:f10-metadata+content-version+provenance-binding+actor-evidence:v2",
		DocumentEventsDeriverDescriptor,
	)
}

func TestDirtyRevisionDoesNotInvalidateOtherVersions(t *testing.T) {
	s := newTestStore(t)
	a, err := s.CreateFile(t.Context(), s.RootID(), "a.txt", fakeHash("a1"), 1, "text/plain")
	require.NoError(t, err)
	b, err := s.CreateFile(t.Context(), s.RootID(), "b.txt", fakeHash("b1"), 1, "text/plain")
	require.NoError(t, err)
	mark := func(id string) {
		require.NoError(t, s.withStorageTx(t.Context(), func(tx *sql.Tx) error {
			return markDocumentEventDirtyTx(t.Context(), tx, id, "test")
		}))
	}
	mark(a.CurrentVersionID)
	mark(a.CurrentVersionID)
	var aRevision, bCount int64
	require.NoError(t, s.db.QueryRow(
		`SELECT revision FROM document_event_dirty WHERE content_version_id=?`,
		a.CurrentVersionID,
	).Scan(&aRevision))
	require.GreaterOrEqual(t, aRevision, int64(2))
	require.NoError(t, s.db.QueryRow(
		`SELECT count(*) FROM document_event_dirty WHERE content_version_id=?`,
		b.CurrentVersionID,
	).Scan(&bCount))
	require.Zero(t, bCount)
}

func TestDocumentEventStateAndRecipeAreLazyAndIdempotent(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	firstFingerprint := fakeHash("f1")
	secondFingerprint := fakeHash("f2")

	state, err := s.DocumentEventStateRow(ctx, firstFingerprint)
	require.NoError(t, err)
	require.Equal(t, DocumentEventState{
		ContractVersion:    document.DocumentEventsContractV1,
		DeriverFingerprint: firstFingerprint,
		InputEpoch:         1,
		PublicationEpoch:   1,
	}, state)
	require.Zero(t, documentEventStateCount(t, s))

	require.NoError(t, s.EnsureDocumentEventRecipe(ctx, firstFingerprint))
	require.Zero(t, documentEventStateCount(t, s), "an empty vault stays pristine")

	_, err = s.CreateFile(ctx, s.RootID(), "recipe.txt", fakeHash("recipe"), 1, "text/plain")
	require.NoError(t, err)
	require.NoError(t, s.EnsureDocumentEventRecipe(ctx, firstFingerprint))
	state, err = s.DocumentEventStateRow(ctx, firstFingerprint)
	require.NoError(t, err)
	require.Equal(t, int64(1), state.InputEpoch)
	require.Equal(t, firstFingerprint, state.DeriverFingerprint)
	require.Equal(t, 1, documentEventStateCount(t, s))

	require.NoError(t, s.EnsureDocumentEventRecipe(ctx, firstFingerprint))
	state, err = s.DocumentEventStateRow(ctx, firstFingerprint)
	require.NoError(t, err)
	require.Equal(t, int64(1), state.InputEpoch, "same recipe must not bump")

	state, err = s.DocumentEventStateRow(ctx, secondFingerprint)
	require.NoError(t, err)
	require.Equal(t, int64(2), state.InputEpoch, "read-only state projects a pending recipe change")
	require.Equal(t, secondFingerprint, state.DeriverFingerprint)
	var storedEpoch int64
	var storedFingerprint string
	require.NoError(t, s.db.QueryRow(`SELECT input_epoch,deriver_fingerprint
		FROM document_event_state WHERE singleton=1`).Scan(&storedEpoch, &storedFingerprint))
	require.Equal(t, int64(1), storedEpoch)
	require.Equal(t, firstFingerprint, storedFingerprint)

	require.NoError(t, s.EnsureDocumentEventRecipe(ctx, secondFingerprint))
	require.NoError(t, s.EnsureDocumentEventRecipe(ctx, secondFingerprint))
	state, err = s.DocumentEventStateRow(ctx, secondFingerprint)
	require.NoError(t, err)
	require.Equal(t, int64(2), state.InputEpoch, "a changed recipe bumps exactly once")
	require.Equal(t, secondFingerprint, state.DeriverFingerprint)
	epoch, err := s.BumpDocumentEventInputEpoch(ctx, firstFingerprint)
	require.NoError(t, err)
	require.Equal(t, int64(3), epoch, "a rebuild that also changes recipe bumps only once")
	state, err = s.DocumentEventStateRow(ctx, firstFingerprint)
	require.NoError(t, err)
	require.Equal(t, firstFingerprint, state.DeriverFingerprint)
	require.Equal(t, epoch, state.InputEpoch)
}

func TestDocumentEventIdleMaintenanceNeedsNoWriteTransaction(t *testing.T) {
	for _, state := range []string{"empty", "installed", "running"} {
		t.Run(state, func(t *testing.T) {
			s := newTestStore(t)
			if state != "empty" {
				_, err := s.CreateFile(t.Context(), s.RootID(), "idle.txt", fakeHash("de"), 1, "text/plain")
				require.NoError(t, err)
				require.NoError(t, s.EnsureDocumentEventRecipe(t.Context(), DocumentEventsDeriverFingerprint))
			}
			if state == "running" {
				_, err := s.StartDocumentEventRebuild(t.Context(),
					"94000000-0000-4000-8000-000000000009", fakeHash("db"))
				require.NoError(t, err)
			}
			s.db.SetMaxOpenConns(1)
			_, err := s.db.ExecContext(t.Context(), `PRAGMA query_only=ON`)
			require.NoError(t, err)
			assert.NoError(t, s.EnsureDocumentEventRecipe(t.Context(), DocumentEventsDeriverFingerprint))
			assert.NoError(t, s.RefreshDocumentEventBuilds(t.Context()))
		})
	}
}

func TestMissingDocumentEventTargetsUsesEpochRevisionAndUUIDCursor(t *testing.T) {
	s := newTestStore(t)
	first, err := s.CreateFile(t.Context(), s.RootID(), "first.txt", fakeHash("01"), 11, "text/plain")
	require.NoError(t, err)
	second, err := s.CreateFile(t.Context(), s.RootID(), "second.txt", fakeHash("02"), 12, "")
	require.NoError(t, err)
	fingerprint := fakeHash("f1")

	targets, err := s.MissingDocumentEventTargetsAfter(t.Context(), fingerprint, "", 100)
	require.NoError(t, err)
	require.Len(t, targets, 2)
	byID := make(map[string]DocumentEventTarget, len(targets))
	for _, target := range targets {
		byID[target.ContentVersionID] = target
		require.Equal(t, int64(1), target.InputEpoch)
		require.Zero(t, target.InputRevision)
	}
	require.Equal(t, first.ID, byID[first.CurrentVersionID].NodeID)
	require.Empty(t, byID[second.CurrentVersionID].MIMEType)

	_, err = s.MissingDocumentEventTargetsAfter(t.Context(), fingerprint, "not-a-uuid", 100)
	require.Error(t, err)
	_, err = s.MissingDocumentEventTargetsAfter(t.Context(), fingerprint, "", 0)
	require.Error(t, err)

	_, err = s.db.Exec(`INSERT INTO document_event_attempts(
		content_version_id,input_epoch,input_revision,inputs_sha256,state,diagnostic_json,attempted_at
	) VALUES(?,?,?,?,?,?,?)`, first.CurrentVersionID, 1, 0, fakeHash("a1"),
		"unavailable", []byte(`[]`), "2020-01-01T00:00:00Z")
	require.NoError(t, err)
	targets, err = s.MissingDocumentEventTargetsAfter(t.Context(), fingerprint, "", 100)
	require.NoError(t, err)
	require.Len(t, targets, 1)
	require.Equal(t, second.CurrentVersionID, targets[0].ContentVersionID)
	require.NoError(t, s.EnsureDocumentEventRecipe(t.Context(), fingerprint))
	_, err = s.db.Exec(`INSERT INTO document_event_attempts(
		content_version_id,input_epoch,input_revision,inputs_sha256,state,diagnostic_json,attempted_at
	) VALUES(?,?,?,?,?,?,?)`, second.CurrentVersionID, 1, 0, fakeHash("a2"),
		"failed", []byte(`[]`), "2020-01-01T00:00:00Z")
	require.NoError(t, err)
	targets, err = s.MissingDocumentEventTargetsAfter(t.Context(), fingerprint, "", 100)
	require.NoError(t, err)
	require.Empty(t, targets)
	targets, err = s.MissingDocumentEventTargetsAfter(t.Context(), fakeHash("f2"), "", 100)
	require.NoError(t, err)
	require.Len(t, targets, 2, "a stale recipe fingerprint makes every target pending")
	for _, target := range targets {
		require.Equal(t, int64(2), target.InputEpoch)
	}

	require.NoError(t, s.withStorageTx(t.Context(), func(tx *sql.Tx) error {
		return markDocumentEventDirtyTx(t.Context(), tx, first.CurrentVersionID, "changed")
	}))
	targets, err = s.MissingDocumentEventTargetsAfter(t.Context(), fingerprint, "", 100)
	require.NoError(t, err)
	require.Len(t, targets, 1)
	require.Equal(t, int64(1), byTargetID(targets, first.CurrentVersionID).InputRevision)

	after := targets[0].ContentVersionID
	targets, err = s.MissingDocumentEventTargetsAfter(t.Context(), fingerprint, after, 100)
	require.NoError(t, err)
	for _, target := range targets {
		require.Greater(t, target.ContentVersionID, after)
	}
}

func byTargetID(targets []DocumentEventTarget, id string) DocumentEventTarget {
	for _, target := range targets {
		if target.ContentVersionID == id {
			return target
		}
	}
	return DocumentEventTarget{}
}

func TestDocumentEventRecipeSerializesConcurrentSameRecipe(t *testing.T) {
	s := newTestStore(t)
	_, err := s.CreateFile(t.Context(), s.RootID(), "concurrent.txt", fakeHash("same"), 1, "text/plain")
	require.NoError(t, err)
	fingerprint := fakeHash("aa")

	const callers = 8
	errs := make(chan error, callers)
	var ready sync.WaitGroup
	ready.Add(callers)
	start := make(chan struct{})
	for range callers {
		go func() {
			ready.Done()
			<-start
			errs <- s.EnsureDocumentEventRecipe(context.Background(), fingerprint)
		}()
	}
	ready.Wait()
	close(start)
	for range callers {
		require.NoError(t, <-errs)
	}

	state, err := s.DocumentEventStateRow(t.Context(), fingerprint)
	require.NoError(t, err)
	require.Equal(t, int64(1), state.InputEpoch)
	require.Equal(t, fingerprint, state.DeriverFingerprint)
}

func TestDocumentEventAttemptsFenceInputsAndStopTerminalRetry(t *testing.T) {
	s := newTestStore(t)
	version, _ := ingestDocumentEventTarget(t, s, "attempt.txt", "a4")
	fingerprint := DocumentEventsDeriverFingerprint
	require.NoError(t, s.EnsureDocumentEventRecipe(t.Context(), fingerprint))
	target := requireDocumentEventTarget(t, s, version.ID)
	digest := requireDocumentEventInputsSHA256(t, s, target)
	diagnostics := []byte(`[{"code":"input_too_large"}]`)

	require.NoError(t, s.withStorageTx(t.Context(), func(tx *sql.Tx) error {
		return markDocumentEventDirtyTx(t.Context(), tx, version.ID, "changed while deriving")
	}))
	err := s.RecordDocumentEventAttempt(t.Context(), target, digest, "unavailable", diagnostics)
	require.ErrorIs(t, err, ErrDocumentEventInputsChanged)
	var count int
	require.NoError(t, s.db.QueryRow(`SELECT count(*) FROM document_event_attempts`).Scan(&count))
	require.Zero(t, count)

	record := documentEventRecord(t, s.VaultID(), version.ID, "a")
	record.Diagnostics = []document.DocumentEventDiagnosticV1{{
		Code: "date_unparseable", Detail: "<synthetic>&\u2028\u2029", SourceKey: "metadata/a/created",
	}}
	canonical := mustMarshalDocumentEvents(t, record)
	_, err = s.PublishDocumentEvents(t.Context(), target, fingerprint, digest, canonical)
	require.ErrorIs(t, err, ErrDocumentEventInputsChanged)
	require.NoError(t, s.db.QueryRow(`SELECT count(*) FROM document_event_heads`).Scan(&count))
	require.Zero(t, count)

	target = requireDocumentEventTarget(t, s, version.ID)
	require.NoError(t, s.RecordDocumentEventAttempt(
		t.Context(), target, digest, "unavailable", diagnostics,
	))
	targets, err := s.MissingDocumentEventTargetsAfter(t.Context(), fingerprint, "", 10)
	require.NoError(t, err)
	require.Empty(t, targets, "a matching terminal attempt must prevent hot retry")

	_, err = s.db.Exec(`UPDATE document_event_attempts SET attempted_at=? WHERE content_version_id=?`,
		"2020-01-01T00:00:00Z", version.ID)
	require.NoError(t, err)
	require.NoError(t, s.RecordDocumentEventAttempt(
		t.Context(), target, digest, "unavailable", diagnostics,
	))
	var attemptedAt string
	require.NoError(t, s.db.QueryRow(`SELECT attempted_at FROM document_event_attempts
		WHERE content_version_id=?`, version.ID).Scan(&attemptedAt))
	require.Equal(t, "2020-01-01T00:00:00Z", attemptedAt, "unchanged replay must not churn")

	err = s.RecordDocumentEventAttempt(t.Context(), target, digest, "indexed", []byte(`[]`))
	require.Error(t, err, "indexed attempts require atomic publication proof")
	err = s.RecordDocumentEventAttempt(t.Context(), target, digest, "retrying", []byte(`[]`))
	require.Error(t, err)
	err = s.RecordDocumentEventAttempt(t.Context(), target, digest, "failed", []byte(`[ ]`))
	require.Error(t, err, "diagnostics must use canonical JSON")
	err = s.RecordDocumentEventAttempt(t.Context(), target, digest, "failed",
		make([]byte, maxDocumentEventDiagnosticBytes+1))
	require.Error(t, err)

	epoch, err := s.BumpDocumentEventInputEpoch(t.Context(), fingerprint)
	require.NoError(t, err)
	require.Equal(t, int64(2), epoch)
	target = requireDocumentEventTarget(t, s, version.ID)
	require.Equal(t, epoch, target.InputEpoch)
	generation, err := s.PublishDocumentEvents(t.Context(), target, fingerprint, digest, canonical)
	require.NoError(t, err)
	require.NotEmpty(t, generation.GenerationID)
	require.NoError(t, s.db.QueryRow(`SELECT count(*) FROM document_event_attempts
		WHERE content_version_id=? AND input_epoch=? AND input_revision=?
		AND inputs_sha256=? AND state='indexed'`, version.ID, target.InputEpoch,
		target.InputRevision, digest).Scan(&count))
	require.Equal(t, 1, count)
	var indexedDiagnostics []byte
	require.NoError(t, s.db.QueryRow(`SELECT diagnostic_json FROM document_event_attempts
		WHERE content_version_id=?`, version.ID).Scan(&indexedDiagnostics))
	require.JSONEq(t, `[{"code":"date_unparseable","detail":"\u003csynthetic\u003e\u0026\u2028\u2029","source_key":"metadata/a/created"}]`,
		string(indexedDiagnostics))
	targets, err = s.MissingDocumentEventTargetsAfter(t.Context(), fingerprint, "", 10)
	require.NoError(t, err)
	require.Empty(t, targets)

	state, err := s.DocumentEventStateRow(t.Context(), fingerprint)
	require.NoError(t, err)
	require.Equal(t, int64(2), state.PublicationEpoch)
	_, err = s.PublishDocumentEvents(t.Context(), target, fingerprint, digest, canonical)
	require.NoError(t, err)
	replayedState, err := s.DocumentEventStateRow(t.Context(), fingerprint)
	require.NoError(t, err)
	require.Equal(t, state.PublicationEpoch, replayedState.PublicationEpoch)
}

func TestDocumentEventRebuildReceiptIsAtomicAndIdempotent(t *testing.T) {
	s := newTestStore(t)
	_, err := s.CreateFile(t.Context(), s.RootID(), "rebuild.txt", fakeHash("b1"), 1, "text/plain")
	require.NoError(t, err)
	require.NoError(t, s.EnsureDocumentEventRecipe(t.Context(), fakeHash("a1")))
	operationID := "00000000-0000-4000-8000-000000000001"
	requestDigest := fakeHash("b1")

	first, err := s.StartDocumentEventRebuild(t.Context(), operationID, requestDigest)
	require.NoError(t, err)
	require.Equal(t, DocumentEventsDeriverFingerprint, first.DeriverFingerprint)
	require.Equal(t, "running", first.State)
	require.Equal(t, int64(2), first.TargetEpoch)
	require.Nil(t, first.FinishedAt)

	replayed, err := s.StartDocumentEventRebuild(t.Context(), operationID, requestDigest)
	require.NoError(t, err)
	require.Equal(t, first, replayed)
	state, err := s.DocumentEventStateRow(t.Context(), DocumentEventsDeriverFingerprint)
	require.NoError(t, err)
	require.Equal(t, int64(2), state.InputEpoch, "receipt replay must not bump")

	_, err = s.StartDocumentEventRebuild(t.Context(), operationID, fakeHash("d2"))
	require.ErrorIs(t, err, ErrDocumentEventBuildConflict)
	state, err = s.DocumentEventStateRow(t.Context(), DocumentEventsDeriverFingerprint)
	require.NoError(t, err)
	require.Equal(t, int64(2), state.InputEpoch)

	second, err := s.StartDocumentEventRebuild(t.Context(),
		"00000000-0000-4000-8000-000000000002", requestDigest)
	require.NoError(t, err)
	require.Equal(t, int64(3), second.TargetEpoch, "a new operation with the same request is legal")
	readBack, err := s.DocumentEventBuild(t.Context(), second.OperationID)
	require.NoError(t, err)
	require.Equal(t, second, readBack)

	_, err = s.DocumentEventBuild(t.Context(), "00000000-0000-4000-8000-000000000099")
	require.ErrorIs(t, err, ErrNotFound)
}

func TestDocumentEventRebuildConcurrentReplayBumpsOnce(t *testing.T) {
	s := newTestStore(t)
	_, err := s.CreateFile(t.Context(), s.RootID(), "concurrent-rebuild.txt", fakeHash("c1"), 1, "text/plain")
	require.NoError(t, err)
	operationID := "10000000-0000-4000-8000-000000000001"
	digest := fakeHash("c1")

	const callers = 8
	builds := make(chan DocumentEventBuild, callers)
	errs := make(chan error, callers)
	var ready sync.WaitGroup
	ready.Add(callers)
	start := make(chan struct{})
	for range callers {
		go func() {
			ready.Done()
			<-start
			build, err := s.StartDocumentEventRebuild(context.Background(), operationID, digest)
			builds <- build
			errs <- err
		}()
	}
	ready.Wait()
	close(start)
	var first DocumentEventBuild
	for i := range callers {
		require.NoError(t, <-errs)
		build := <-builds
		if i == 0 {
			first = build
		} else {
			require.Equal(t, first, build)
		}
	}
	state, err := s.DocumentEventStateRow(t.Context(), DocumentEventsDeriverFingerprint)
	require.NoError(t, err)
	require.Equal(t, int64(2), state.InputEpoch)
}

func TestDocumentEventRebuildRollsBackEpochWhenReceiptInsertFails(t *testing.T) {
	s := newTestStore(t)
	_, err := s.CreateFile(t.Context(), s.RootID(), "rollback.txt", fakeHash("d1"), 1, "text/plain")
	require.NoError(t, err)
	_, err = s.db.Exec(`CREATE TRIGGER reject_document_event_build
		BEFORE INSERT ON document_event_builds BEGIN SELECT RAISE(ABORT, 'reject build'); END`)
	require.NoError(t, err)

	_, err = s.StartDocumentEventRebuild(t.Context(),
		"20000000-0000-4000-8000-000000000001", fakeHash("d1"))
	require.ErrorContains(t, err, "reject build")
	require.Zero(t, documentEventStateCount(t, s), "epoch creation must roll back with receipt")
}

func TestDocumentEventBuildRefreshCountsCurrentTerminalAttempts(t *testing.T) {
	s := newTestStore(t)
	first, err := s.CreateFile(t.Context(), s.RootID(), "first-build.txt", fakeHash("e1"), 1, "text/plain")
	require.NoError(t, err)
	second, err := s.CreateFile(t.Context(), s.RootID(), "second-build.txt", fakeHash("e2"), 1, "text/plain")
	require.NoError(t, err)
	build, err := s.StartDocumentEventRebuild(t.Context(),
		"30000000-0000-4000-8000-000000000001", fakeHash("e1"))
	require.NoError(t, err)

	firstTarget := requireDocumentEventTarget(t, s, first.CurrentVersionID)
	secondTarget := requireDocumentEventTarget(t, s, second.CurrentVersionID)
	firstCanonical := mustMarshalDocumentEvents(t,
		documentEventRecord(t, s.VaultID(), first.CurrentVersionID, "e"))
	_, err = s.PublishDocumentEvents(t.Context(), firstTarget, DocumentEventsDeriverFingerprint,
		requireDocumentEventInputsSHA256(t, s, firstTarget), firstCanonical)
	require.NoError(t, err)
	_, err = s.db.Exec(`DELETE FROM document_event_heads WHERE content_version_id=?`,
		first.CurrentVersionID)
	require.NoError(t, err)
	targets, err := s.MissingDocumentEventTargetsAfter(
		t.Context(), DocumentEventsDeriverFingerprint, "", 10,
	)
	require.NoError(t, err)
	require.Equal(t, first.CurrentVersionID,
		byTargetID(targets, first.CurrentVersionID).ContentVersionID,
		"an indexed attempt without its head must be eligible for derivation",
	)
	require.NoError(t, s.RefreshDocumentEventBuilds(t.Context()))
	build, err = s.DocumentEventBuild(t.Context(), build.OperationID)
	require.NoError(t, err)
	require.Zero(t, build.Scanned, "an indexed attempt without its valid head stays pending")
	require.Zero(t, build.Published)
	_, err = s.PublishDocumentEvents(t.Context(), firstTarget, DocumentEventsDeriverFingerprint,
		requireDocumentEventInputsSHA256(t, s, firstTarget), firstCanonical)
	require.NoError(t, err)
	require.NoError(t, s.RefreshDocumentEventBuilds(t.Context()))
	build, err = s.DocumentEventBuild(t.Context(), build.OperationID)
	require.NoError(t, err)
	require.Equal(t, "running", build.State)
	require.Equal(t, int64(1), build.Scanned)
	require.Equal(t, int64(1), build.Published)
	require.Nil(t, build.FinishedAt)

	require.NoError(t, s.RecordDocumentEventAttempt(t.Context(), secondTarget,
		requireDocumentEventInputsSHA256(t, s, secondTarget), "unavailable", []byte(`[{"code":"unsupported"}]`)))
	require.NoError(t, s.RefreshDocumentEventBuilds(t.Context()))
	build, err = s.DocumentEventBuild(t.Context(), build.OperationID)
	require.NoError(t, err)
	require.Equal(t, "failed", build.State)
	require.Equal(t, int64(2), build.Scanned)
	require.Equal(t, int64(1), build.Published)
	require.Equal(t, int64(1), build.Unavailable)
	require.NotNil(t, build.FinishedAt)

	newBuild, err := s.StartDocumentEventRebuild(t.Context(),
		"30000000-0000-4000-8000-000000000002", fakeHash("e2"))
	require.NoError(t, err)
	staleTarget := requireDocumentEventTarget(t, s, second.CurrentVersionID)
	require.NoError(t, s.withStorageTx(t.Context(), func(tx *sql.Tx) error {
		return markDocumentEventDirtyTx(t.Context(), tx, second.CurrentVersionID, "changed")
	}))
	secondCanonical := mustMarshalDocumentEvents(t,
		documentEventRecord(t, s.VaultID(), second.CurrentVersionID, "f"))
	_, err = s.PublishDocumentEvents(t.Context(), staleTarget, DocumentEventsDeriverFingerprint,
		requireDocumentEventInputsSHA256(t, s, staleTarget), secondCanonical)
	require.ErrorIs(t, err, ErrDocumentEventInputsChanged)
	require.NoError(t, s.RefreshDocumentEventBuilds(t.Context()))
	newBuild, err = s.DocumentEventBuild(t.Context(), newBuild.OperationID)
	require.NoError(t, err)
	require.Equal(t, "running", newBuild.State, "a stale terminal attempt cannot complete a build")
	require.Zero(t, newBuild.Scanned)
}

func requireDocumentEventTarget(
	t *testing.T,
	s *Store,
	versionID string,
) DocumentEventTarget {
	t.Helper()
	targets, err := s.MissingDocumentEventTargetsAfter(
		t.Context(), DocumentEventsDeriverFingerprint, "", 100,
	)
	require.NoError(t, err)
	target := byTargetID(targets, versionID)
	require.Equal(t, versionID, target.ContentVersionID)
	return target
}

func documentEventStateCount(t *testing.T, s *Store) int {
	t.Helper()
	var count int
	require.NoError(t, s.db.QueryRow(`SELECT count(*) FROM document_event_state`).Scan(&count))
	return count
}

package processing

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"fmt"
	"io"
	"math"
	"path/filepath"
	"testing"
	"testing/synctest"
	"time"
	"uuid"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/store"
	docsqlite "go.kenn.io/docbank/sqlite"
	"go.kenn.io/kit/embedclient"
)

func TestFaceSuggestionsRecoverAfterQueueSpaceReturns(t *testing.T) {
	t.Parallel()
	x := newFaceEmbeddingFixture(t, false)
	x.matchAll = true
	ctx := t.Context()
	x.enable(t)
	_, err := x.worker.ScanOnce(ctx)
	require.NoError(t, err)
	db, err := store.DefaultSQLiteDriver().Open(x.f.databasePath, docsqlite.OpenOptions{Access: docsqlite.ReadWriteExisting, TransactionMode: docsqlite.Immediate})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	tx, err := db.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	evidence, err := json.Marshal([]store.PersonCandidateOccurrence{{ContentVersionID: x.f.node.CurrentVersionID, Role: "author", EvidenceKind: "source_metadata", EvidenceID: "capacity"}})
	require.NoError(t, err)
	candidate, retained, err := x.f.catalog.OpenPersonCandidate(ctx, tx, store.PersonMatchCandidate{ActorKey: "name_alias:capacity", DisplayName: "Capacity", Reason: "name_only", Evidence: evidence})
	require.NoError(t, err)
	require.True(t, retained)
	// Fill the queue with the same valid evidence as the candidate above.
	_, err = tx.ExecContext(ctx, `WITH RECURSIVE seq(n) AS (
		SELECT 1 UNION ALL SELECT n+1 FROM seq WHERE n < 9999
	) INSERT INTO person_match_candidates(
		candidate_id,actor_key,display_name,suggested_person_id,reason,evidence_json,evidence_sha256,
		occurrence_count,revision,state,decided_person_id,created_at,decided_at
	) SELECT printf('00000000-0000-4000-8000-%012d',n),printf('name_alias:person-%05d',n),printf('person-%05d',n),NULL,
		reason,evidence_json,evidence_sha256,occurrence_count,revision,state,NULL,created_at,NULL
		FROM seq CROSS JOIN person_match_candidates WHERE candidate_id=?`, candidate.CandidateID)
	require.NoError(t, err)
	require.NoError(t, tx.Commit())
	_, full, err := x.f.catalog.OpenPersonCandidateCount(ctx)
	require.NoError(t, err)
	require.True(t, full)
	require.NoError(t, x.service.rebuildFaceSuggestions(ctx))
	groups, _, err := x.f.catalog.FaceCandidates(ctx, 24, 0)
	require.NoError(t, err)
	require.Empty(t, groups)
	before, err := x.f.catalog.FaceMatchingInputs(ctx)
	require.NoError(t, err)
	_, err = x.f.catalog.DecidePersonCandidate(ctx, store.CandidateDecision{CandidateID: candidate.CandidateID, ExpectedRevision: candidate.Revision, Action: "reject"})
	require.NoError(t, err)
	after, err := x.f.catalog.FaceMatchingInputs(ctx)
	require.NoError(t, err)
	require.Equal(t, before.Signature, after.Signature)
	require.NoError(t, x.service.rebuildFaceSuggestions(ctx))
	groups, _, err = x.f.catalog.FaceCandidates(ctx, 24, 0)
	require.NoError(t, err)
	require.Len(t, groups, 1, "freeing a document-candidate slot must admit the waiting face group")
}

func TestHealthyFacesSurviveUnavailableVectorPayload(t *testing.T) {
	t.Parallel()
	x := newFaceEmbeddingFixture(t, false)
	x.matchAll = true
	x.vectorForBox = func(box embedclient.FaceBox) []float32 {
		values := make([]float32, 512)
		if box.Y > .5 {
			values[1] = 1
		} else {
			values[0] = 1
		}
		return values
	}
	ctx := t.Context()
	healthy := x.f.addPhoto(t, "healthy.jpg", 700)
	for _, left := range []float64{.1, .6} {
		_, _, err := x.f.catalog.CreatePhotoFace(ctx, healthy, uuid.New().String(), document.FaceBox{X: left, Y: .6, Width: .2, Height: .3}, "")
		require.NoError(t, err)
	}
	x.enable(t)
	for range 2 {
		_, err := x.worker.ScanOnce(ctx)
		require.NoError(t, err)
	}
	snapshot, err := x.f.catalog.FaceMatchingInputs(ctx)
	require.NoError(t, err)
	require.Len(t, snapshot.Sets, 2)
	for _, item := range snapshot.Sets {
		if item.Set.ContentVersionID == x.f.node.CurrentVersionID {
			require.NoError(t, x.f.blobs.Remove(item.Set.VectorSet.PayloadBlobHash))
		}
	}
	require.ErrorIs(t, x.service.rebuildFaceSuggestions(ctx), store.ErrVectorSetUnavailable)
	groups, _, err := x.f.catalog.FaceCandidates(ctx, 24, 0)
	require.NoError(t, err)
	require.Len(t, groups, 1)
	require.Len(t, groups[0].Faces, 2)
	for _, face := range groups[0].Faces {
		require.Equal(t, healthy, face.ContentVersionID)
	}
}

func TestFaceSuggestionsResumeAfterRestart(t *testing.T) {
	t.Parallel()
	x := newFaceEmbeddingFixture(t, false)
	x.matchAll = true
	ctx := t.Context()
	x.enable(t)
	_, err := x.worker.ScanOnce(ctx)
	require.NoError(t, err)
	var backup bytes.Buffer
	require.NoError(t, x.f.catalog.ExportMetadata(ctx, &backup))
	path := filepath.Join(t.TempDir(), "restart.db")
	restored, err := store.Open(ctx, path)
	require.NoError(t, err)
	require.NoError(t, restored.ImportMetadata(ctx, bytes.NewReader(backup.Bytes())))
	require.NoError(t, restored.Close())
	reopened, err := store.Open(ctx, path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, reopened.Close()) })
	service, err := NewService(ctx, ServiceConfig{Catalog: reopened, Blobs: x.f.blobs, Gate: newTestOperationGate(), SpoolDirectory: t.TempDir(), Lifecycle: ctx})
	require.NoError(t, err)
	t.Cleanup(service.Stop)
	require.Zero(t, reopened.WriteRevision(), "the first pass must run at a reopened store's initial revision")
	_, total, err := reopened.FaceCandidates(ctx, 24, 0)
	require.NoError(t, err)
	require.Zero(t, total, "restart after saving vectors but before publishing groups")
	synctest.Test(t, func(t *testing.T) {
		laneContext, cancel := context.WithCancel(t.Context())
		finished := make(chan error, 1)
		go func() { finished <- service.FaceSuggestionBackfill(laneContext, nil) }()
		defer func() {
			cancel()
			require.ErrorIs(t, <-finished, context.Canceled)
		}()
		require.Eventually(t, func() bool {
			groups, total, err := reopened.FaceCandidates(t.Context(), 24, 0)
			return err == nil && total == 1 && len(groups) == 1 && len(groups[0].Faces) == 2
		}, 5*time.Second, 10*time.Millisecond, "retained vectors must publish without another write")
	})
}

func newFaceSuggestionBatchFixture(t *testing.T) (*faceEmbeddingFixture, store.PhotoFace) {
	t.Helper()
	x := newFaceEmbeddingFixture(t, false)
	x.matchAll = true
	x.worker.maxRows = 1000
	x.worker.maxVectorBlobBytes = 8 << 20
	x.vectorForBox = func(box embedclient.FaceBox) []float32 {
		values := make([]float32, 512)
		values[int(math.Round(box.X*1000))/2] = 1
		return values
	}
	ctx := t.Context()
	var omitted store.PhotoFace
	for i := range 998 {
		face, _, err := x.f.catalog.CreatePhotoFace(ctx, x.f.node.CurrentVersionID, fmt.Sprintf("00000000-0000-4000-8000-%012d", i+1), document.FaceBox{X: float64(i) / 1000, Width: .001, Height: .1}, "")
		require.NoError(t, err)
		if i == 0 {
			omitted = face
		}
	}
	x.enable(t)
	n, err := x.worker.ScanOnce(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, n)
	return x, omitted
}

func TestFaceSuggestionBatchesPublishThenCatchUp(t *testing.T) {
	t.Parallel()
	x, omitted := newFaceSuggestionBatchFixture(t)
	ctx := t.Context()
	service, err := NewService(ctx, ServiceConfig{Catalog: x.f.catalog, Blobs: x.f.blobs, Gate: newTestOperationGate(), SpoolDirectory: t.TempDir(), Lifecycle: ctx})
	require.NoError(t, err)
	t.Cleanup(service.Stop)
	require.NoError(t, service.rebuildFaceSuggestions(ctx))
	page, total, err := x.f.catalog.FaceCandidates(ctx, 24, 0)
	require.NoError(t, err)
	require.Empty(t, page, "partial groups must stay unpublished")
	require.Zero(t, total)
	_, err = x.f.catalog.DecidePhotoFace(ctx, omitted.ContentVersionID, store.PhotoFaceDecisionRequest{FaceID: omitted.FaceID, Revision: omitted.Revision, Action: "dismiss"})
	require.NoError(t, err)
	for attempt := 0; attempt < 32 && service.faceSuggestionsSignature == ""; attempt++ {
		require.NoError(t, service.rebuildFaceSuggestions(ctx))
	}
	require.NotEmpty(t, service.faceSuggestionsSignature)
	_, total, err = x.f.catalog.FaceCandidates(ctx, 1, 0)
	require.NoError(t, err)
	require.EqualValues(t, 498, total, "current reads must hide the dismissed pair before catch-up")
	snapshot, err := x.f.catalog.FaceMatchingInputs(ctx)
	require.NoError(t, err)
	for attempt := 0; attempt < 32 && service.faceSuggestionsSignature != snapshot.Signature; attempt++ {
		require.NoError(t, service.rebuildFaceSuggestions(ctx))
	}
	require.Equal(t, snapshot.Signature, service.faceSuggestionsSignature)
	retired, _, err := x.f.catalog.PersonCandidates(ctx, "superseded", 24, 0)
	require.NoError(t, err)
	require.Len(t, retired, 1, "catch-up must supersede the snapshot's dismissed pair")
}

func TestFaceSuggestionsRetryRepairedPayload(t *testing.T) {
	t.Parallel()
	x := newFaceEmbeddingFixture(t, false)
	x.matchAll = true
	ctx := t.Context()
	x.enable(t)
	_, err := x.worker.ScanOnce(ctx)
	require.NoError(t, err)
	snapshot, err := x.f.catalog.FaceMatchingInputs(ctx)
	require.NoError(t, err)
	require.Len(t, snapshot.Sets, 1)
	hash := snapshot.Sets[0].Set.VectorSet.PayloadBlobHash
	source, err := x.f.blobs.OpenContext(ctx, hash)
	require.NoError(t, err)
	data, err := io.ReadAll(source)
	require.NoError(t, err)
	require.NoError(t, source.Close())
	require.NoError(t, x.f.blobs.Remove(hash))
	synctest.Test(t, func(t *testing.T) {
		laneContext, cancel := context.WithCancel(t.Context())
		finished := make(chan error, 1)
		go func() { finished <- x.service.FaceSuggestionBackfill(laneContext, nil) }()
		defer func() { cancel(); require.ErrorIs(t, <-finished, context.Canceled) }()
		time.Sleep(3 * time.Second)
		_, total, err := x.f.catalog.FaceCandidates(t.Context(), 24, 0)
		require.NoError(t, err)
		require.Zero(t, total)
		before := x.f.catalog.WriteRevision()
		_, err = x.f.blobs.WriteDetailedContext(t.Context(), bytes.NewReader(data))
		require.NoError(t, err)
		require.Equal(t, before, x.f.catalog.WriteRevision())
		time.Sleep(30 * time.Second)
		_, total, err = x.f.catalog.FaceCandidates(t.Context(), 24, 0)
		require.NoError(t, err)
		require.EqualValues(t, 1, total, "restored payload must become a suggestion without a catalog write")
	})
}

func TestFaceSuggestionLaneCatchesUpAfterMidPassWrite(t *testing.T) {
	t.Parallel()
	x, _ := newFaceSuggestionBatchFixture(t)
	ctx := t.Context()
	face := x.inputs[0]
	person, err := x.f.catalog.CreatePerson(ctx, "Ada", "operator")
	require.NoError(t, err)
	_, err = x.f.catalog.DecidePhotoFace(ctx, face.ContentVersionID, store.PhotoFaceDecisionRequest{FaceID: face.FaceID, Revision: face.Revision, Action: "name", PersonID: person.PersonID})
	require.NoError(t, err)
	for attempts := 0; x.service.faceSuggestionsSignature == "" && attempts < 32; attempts++ {
		require.NoError(t, x.service.rebuildFaceSuggestions(ctx))
	}
	require.NotEmpty(t, x.service.faceSuggestionsSignature)
	for i, name := range []string{"Beth", "Cora"} {
		person, err = x.f.catalog.UpdatePerson(ctx, person.PersonID, person.Revision, name)
		require.NoError(t, err)
		if i == 0 {
			require.NoError(t, x.service.rebuildFaceSuggestions(ctx))
			require.NotNil(t, x.service.faceSuggestionsPending, "the next rename must happen during an unfinished pass")
		}
	}
	require.NotNil(t, x.service.faceSuggestionsPending, "the lane starts with the earlier snapshot still pending")
	current, err := x.f.catalog.FaceMatchingInputs(ctx)
	require.NoError(t, err)
	// Both renames change inputs without changing candidate identities. The
	// first publication therefore makes no write that could wake a stale lane.
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		finished := make(chan error, 1)
		go func() { finished <- x.service.FaceSuggestionBackfill(ctx, nil) }()
		defer func() { cancel(); require.ErrorIs(t, <-finished, context.Canceled) }()
		time.Sleep(5 * time.Second)
		synctest.Wait()
		require.Equal(t, current.Signature, x.service.faceSuggestionsSignature, "the lane must publish the final input snapshot without another write")
	})
}

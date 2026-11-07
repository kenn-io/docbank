package processing

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"fmt"
	"math"
	"path/filepath"
	"testing"
	"testing/synctest"
	"time"
	"uuid"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/config"
	"go.kenn.io/docbank/internal/store"
	docsqlite "go.kenn.io/docbank/sqlite"
	"go.kenn.io/kit/embedclient"
)

func TestFaceSuggestionsRecoverAfterQueueSpaceReturns(t *testing.T) {
	t.Parallel()
	x := newFaceEmbeddingFixture(t, false)
	x.matchAll = true
	ctx := t.Context()
	x.f.grant(t, x.service, config.LocalFacesProfile)
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
	// Fill the remaining document-candidate slots, as in the store capacity test.
	_, err = tx.ExecContext(ctx, `WITH RECURSIVE seq(n) AS (
		SELECT 1 UNION ALL SELECT n+1 FROM seq WHERE n < 9999
	) INSERT INTO person_match_candidates(
		candidate_id,actor_key,display_name,suggested_person_id,reason,evidence_json,evidence_sha256,
		occurrence_count,revision,state,decided_person_id,created_at,decided_at
	) SELECT printf('candidate-%05d',n),printf('name_alias:person-%05d',n),'Synthetic Person',NULL,
		'name_only','[]',printf('%064d',n),1,1,'open',NULL,?,NULL FROM seq`, time.Now().UTC().Format(time.RFC3339Nano))
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
	x.f.grant(t, x.service, config.LocalFacesProfile)
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
	// Restore metadata into a fresh catalog so no decoded payload cache can hide
	// the missing immutable blob, matching a restart with one unavailable set.
	var backup bytes.Buffer
	require.NoError(t, x.f.catalog.ExportMetadata(ctx, &backup))
	target, err := store.Open(ctx, filepath.Join(t.TempDir(), "restored.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, target.Close()) })
	require.NoError(t, target.ImportMetadata(ctx, bytes.NewReader(backup.Bytes())))
	restored, err := NewService(ctx, ServiceConfig{Catalog: target, Blobs: x.f.blobs, Gate: newTestOperationGate(), SpoolDirectory: t.TempDir(), Lifecycle: ctx})
	require.NoError(t, err)
	t.Cleanup(restored.Stop)
	require.NoError(t, restored.rebuildFaceSuggestions(ctx))
	groups, _, err := target.FaceCandidates(ctx, 24, 0)
	require.NoError(t, err)
	require.Len(t, groups, 1)
	require.Len(t, groups[0].Faces, 2)
	for _, face := range groups[0].Faces {
		require.Equal(t, healthy, face.ContentVersionID)
	}
}

func TestFaceSuggestionBatchesResumeAndDiscardChangedInputs(t *testing.T) {
	t.Parallel()
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
	x.f.grant(t, x.service, config.LocalFacesProfile)
	n, err := x.worker.ScanOnce(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, n)
	require.NoError(t, x.service.rebuildFaceSuggestions(ctx))
	first := x.service.faceSuggestionsPending
	require.NotNil(t, first)
	page, total, err := x.f.catalog.FaceCandidates(ctx, 24, 0)
	require.NoError(t, err)
	require.Empty(t, page, "partial groups must stay unpublished")
	require.Zero(t, total)
	_, err = x.f.catalog.CreatePerson(ctx, "Unrelated person", "operator")
	require.NoError(t, err)
	require.NoError(t, x.service.rebuildFaceSuggestions(ctx))
	require.Same(t, first, x.service.faceSuggestionsPending, "unrelated writes must preserve progress")
	_, err = x.f.catalog.DecidePhotoFace(ctx, omitted.ContentVersionID, store.PhotoFaceDecisionRequest{FaceID: omitted.FaceID, Revision: omitted.Revision, Action: "dismiss"})
	require.NoError(t, err)
	require.NoError(t, x.service.rebuildFaceSuggestions(ctx))
	require.NotNil(t, x.service.faceSuggestionsPending)
	require.NotSame(t, first, x.service.faceSuggestionsPending, "changed faces must discard stale groups")
	synctest.Test(t, func(t *testing.T) {
		laneContext, cancel := context.WithCancel(ctx)
		finished := make(chan error, 1)
		go func() { finished <- x.service.FaceSuggestionBackfill(laneContext, nil) }()
		defer func() {
			cancel()
			select {
			case err := <-finished:
				require.ErrorIs(t, err, context.Canceled)
			case <-time.After(5 * time.Second):
				t.Error("face backfill did not stop")
			}
		}()
		require.Eventually(t, func() bool {
			_, total, err := x.f.catalog.FaceCandidates(ctx, 24, 0)
			return err == nil && total == 498
		}, 10*time.Second, 10*time.Millisecond, "batches must finish without another write")
		for offset := 0; offset < 498; offset += 250 {
			page, total, err = x.f.catalog.FaceCandidates(ctx, 250, offset)
			require.NoError(t, err)
			require.EqualValues(t, 498, total)
			for _, group := range page {
				for _, face := range group.Faces {
					require.NotEqual(t, omitted.FaceID, face.FaceID)
				}
			}
		}
	})
}

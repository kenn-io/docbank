package store

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestPhotoEventsRenditionCoverage(t *testing.T) {
	t.Parallel()
	for _, transition := range []string{"failure", "recovery", "renewal"} {
		t.Run(transition, func(t *testing.T) {
			s, _, _, profile := collectionCoverageFixture(t, 0)
			ctx := t.Context()
			photo := browsePhotoNode(t, s, "photo.jpg", browseHash("coverage-event"), "image/jpeg")
			asset, err := s.PhotoAssetForNode(ctx, photo.ID)
			require.NoError(t, err)
			request := renditionJobTestRequest(photo.CurrentVersionID, profile)
			request.ExecutionIdentity.Upload.SHA256 = photo.BlobHash
			request.ExecutionIdentity.Authorization.SourceSHA256 = photo.BlobHash
			grantRenditionJobConsent(t, s, request)
			job, _, err := s.EnqueueRenditionJob(ctx, request)
			require.NoError(t, err)
			now := time.Now().UTC().Add(time.Second)
			claim, err := s.ClaimRenditionJob(ctx, job.ID, "coverage-worker", now, time.Minute)
			require.NoError(t, err)
			switch transition {
			case "recovery":
				require.NoError(t, s.MarkRenditionJobFailed(ctx, claim, RenditionFailureConsent, now.Add(time.Second)))
			case "renewal":
				require.NoError(t, s.MarkRenditionJobFailed(ctx, claim, RenditionFailureTerminal, now.Add(time.Second)))
				request.CapturedArtifactPolicy = []byte(strings.Replace(string(request.CapturedArtifactPolicy), `"max_count":1,"min_count":0`, `"max_count":2,"min_count":0`, 1))
				running, _, err := s.EnqueueRenditionJob(ctx, request)
				require.NoError(t, err)
				require.NotEqual(t, job.ID, running.ID)
				claim, err = s.ClaimRenditionJob(ctx, running.ID, "running-worker", now, time.Minute)
				require.NoError(t, err)
				// Keep both exact waiters and the running job older than the failed attempt.
				_, err = s.db.Exec(`UPDATE rendition_job_waiters SET updated_at='2026-01-01T00:00:00.000000000Z'`)
				require.NoError(t, err)
				_, err = s.db.Exec(`UPDATE rendition_jobs SET updated_at='2026-01-01T00:00:00.000000000Z' WHERE job_id=?`, running.ID)
				require.NoError(t, err)
			}
			ch, stop := s.SubscribePhotoEvents()
			defer stop()
			switch transition {
			case "failure":
				require.NoError(t, s.MarkRenditionJobFailed(ctx, claim, RenditionFailureConsent, now.Add(time.Second)))
			case "renewal":
				_, err = s.RenewRenditionJobClaim(ctx, claim, now.Add(2*time.Second), time.Minute)
				require.NoError(t, err)
			default:
				reopened, _, err := s.EnqueueRenditionJob(ctx, request)
				require.NoError(t, err)
				require.Equal(t, RenditionJobQueued, reopened.State)
			}
			event := nextPhotoEvent(t, ch)
			require.True(t, event.Refetch || event.AssetID == asset.ID, "coverage transition must invalidate the affected photo query")
		})
	}
}

func TestPhotoEventsTagAssignmentDefinitions(t *testing.T) {
	t.Parallel()
	for _, mime := range []string{"text/plain", "image/jpeg"} {
		t.Run(mime, func(t *testing.T) {
			s := newTestStore(t)
			ctx := t.Context()
			node, err := s.CreateFile(ctx, s.RootID(), "source", browseHash("tag-event"), 4, mime)
			require.NoError(t, err)
			tag, err := s.CreateTag(ctx, "Keyword")
			require.NoError(t, err)
			ch, stop := s.SubscribePhotoEvents()
			defer stop()
			_, err = s.AssignTag(ctx, tag.ID, node.ID, node.Revision)
			require.NoError(t, err)
			require.Equal(t, PhotoEvent{Refetch: true}, nextPhotoEvent(t, ch))
			noPhotoEvent(t, ch)
		})
	}
}

func TestPhotoEventsBulkDocumentTrashQuiet(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	dir, err := s.Mkdir(ctx, s.RootID(), "documents")
	require.NoError(t, err)
	for i := range 100 {
		_, err = s.CreateFile(ctx, dir.ID, fmt.Sprintf("document-%d.txt", i), browseHash(fmt.Sprintf("document-%d", i)), 4, "text/plain")
		require.NoError(t, err)
	}
	ch, stop := s.SubscribePhotoEvents()
	defer stop()
	_, _, err = s.Trash(ctx, dir.ID, UnconditionalRev)
	require.NoError(t, err)
	noPhotoEvent(t, ch)
}

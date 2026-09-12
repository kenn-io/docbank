package store

import (
	"bytes"
	"path/filepath"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
)

func pageStoreRequest(t *testing.T, s *Store) PageJobRequest {
	t.Helper()
	node, err := s.CreateFile(t.Context(), s.RootID(), "synthetic.pdf", fakeHash("a1"), 123, "application/pdf")
	require.NoError(t, err)
	return PageJobRequest{Source: document.PageSource{VersionID: node.CurrentVersionID, SHA256: node.BlobHash, Size: 123}, NodeID: node.ID, Revision: node.Revision, Pages: []int{1, 2}, DPI: 144, RuntimeFingerprint: fakeHash("b1")}
}
func pageStoreFrames(t *testing.T, r PageJobRequest) []document.PageFrameV1 {
	t.Helper()
	var frames []document.PageFrameV1
	for page := 1; page <= 2; page++ {
		f, err := document.NewPDFPageFrame(r.Source, page, [4]float64{0, 0, 72, 144}, [4]float64{0, 0, 72, 144}, 0)
		require.NoError(t, err)
		frames = append(frames, f)
	}
	return frames
}
func pageStoreRecipe() document.PageRecipeV1 {
	return document.PageRecipeV1{Contract: document.PageImageContractV1, DPI: 144, Format: "png", RendererIdentity: document.PageRendererIdentity{Executable: "synthetic-renderer", Version: "1.0.0", Options: []string{"crop-visible"}}}
}
func pageStoreImage(t *testing.T, f document.PageFrameV1, r document.PageRecipeV1) document.PageImageV1 {
	t.Helper()
	_, fh, err := document.MarshalPageFrameV1(f)
	require.NoError(t, err)
	_, rh, err := document.MarshalPageRecipeV1(r)
	require.NoError(t, err)
	return document.PageImageV1{Contract: document.PageImageContractV1, Source: f.Source, Page: f.Page, FrameSHA256: fh, RecipeSHA256: rh, SHA256: fakeHash("c1"), Size: 10, Width: 144, Height: 288}
}

func TestPagePublicationFencesClaimsAndPreservesPartialAuthority(t *testing.T) {
	s := newTestStore(t)
	request := pageStoreRequest(t, s)
	operation := uuid.NewString()
	first, err := s.QueuePageJob(t.Context(), operation, request)
	require.NoError(t, err)
	retry, err := s.QueuePageJob(t.Context(), uuid.NewString(), request)
	require.NoError(t, err)
	require.Equal(t, first.ID, retry.ID)
	claim, err := s.ClaimPageJob(t.Context())
	require.NoError(t, err)
	frames := pageStoreFrames(t, request)
	require.NoError(t, s.PublishPageFrames(t.Context(), claim, frames))
	recipe := pageStoreRecipe()
	receipt := pageStoreImage(t, frames[0], recipe)
	physical := &BlobPhysical{Encoding: looseEncodingRaw, StoredBytes: 10}
	require.NoError(t, s.PublishPageImage(t.Context(), claim, receipt, recipe, physical))
	require.NoError(t, s.PublishPageImage(t.Context(), claim, receipt, recipe, nil))
	inventory, err := s.PageInventory(t.Context(), request.Binding())
	require.NoError(t, err)
	require.Len(t, inventory.Frames, 2)
	require.Len(t, inventory.Images, 1)
	altered := receipt
	altered.SHA256 = fakeHash("c2")
	require.ErrorIs(t, s.PublishPageImage(t.Context(), claim, altered, recipe, physical), ErrPageConflict)
	require.NoError(t, s.CancelPageJob(t.Context(), first.ID, request.Binding()))
	require.ErrorIs(t, s.PublishPageImage(t.Context(), claim, pageStoreImage(t, frames[1], recipe), recipe, physical), ErrPageFenced)
	roots, err := s.UnreachableBlobs(t.Context())
	require.NoError(t, err)
	for _, root := range roots {
		require.NotEqual(t, receipt.SHA256, root.Hash)
	}
	var metadata bytes.Buffer
	require.NoError(t, s.ExportMetadata(t.Context(), &metadata))
	restored := newTestStore(t)
	require.NoError(t, restored.ImportMetadata(t.Context(), bytes.NewReader(metadata.Bytes())))
	inventory, err = restored.PageInventory(t.Context(), request.Binding())
	require.NoError(t, err)
	require.Len(t, inventory.Frames, 2)
	require.Len(t, inventory.Images, 1)
}

func TestPageJobRestartCannotPublishAnOldClaim(t *testing.T) {
	s := newTestStore(t)
	request := pageStoreRequest(t, s)
	_, err := s.QueuePageJob(t.Context(), uuid.NewString(), request)
	require.NoError(t, err)
	old, err := s.ClaimPageJob(t.Context())
	require.NoError(t, err)
	require.NoError(t, s.RequeuePageJobs(t.Context()))
	current, err := s.ClaimPageJob(t.Context())
	require.NoError(t, err)
	require.Greater(t, current.Epoch, old.Epoch)
	require.ErrorIs(t, s.FinishPageJob(t.Context(), old, "completed", ""), ErrPageFenced)
	require.NoError(t, s.CheckPageClaim(t.Context(), current), "obsolete completion cannot retire the replacement claim")
	require.ErrorIs(t, s.PublishPageFrames(t.Context(), old, pageStoreFrames(t, request)), ErrPageFenced)
	require.NoError(t, s.PublishPageFrames(t.Context(), current, pageStoreFrames(t, request)))
	require.Error(t, s.FinishPageJob(t.Context(), current, "completed", ""), "completion requires every requested receipt")
}

func TestPageJobStaleCompletionRetiresOnlyItsOwnedClaim(t *testing.T) {
	for _, change := range []string{"revision", "head", "trash"} {
		t.Run(change, func(t *testing.T) {
			s := newTestStore(t)
			request := pageStoreRequest(t, s)
			job, err := s.QueuePageJob(t.Context(), uuid.NewString(), request)
			require.NoError(t, err)
			claim, err := s.ClaimPageJob(t.Context())
			require.NoError(t, err)
			frames := pageStoreFrames(t, request)
			require.NoError(t, s.PublishPageFrames(t.Context(), claim, frames))
			recipe := pageStoreRecipe()
			for _, frame := range frames {
				require.NoError(t, s.PublishPageImage(t.Context(), claim, pageStoreImage(t, frame, recipe), recipe, &BlobPhysical{Encoding: looseEncodingRaw, StoredBytes: 10}))
			}
			switch change {
			case "revision":
				_, err = s.db.ExecContext(t.Context(), `UPDATE nodes SET revision=revision+1 WHERE id=?`, request.NodeID)
			case "head":
				_, _, err = s.ReplaceContent(t.Context(), request.NodeID, request.Revision, fakeHash("d1"), 124, "application/pdf")
			case "trash":
				_, _, err = s.Trash(t.Context(), request.NodeID, request.Revision)
			}
			require.NoError(t, err)
			require.ErrorIs(t, s.FinishPageJob(t.Context(), claim, "completed", ""), ErrPageFenced)
			retired, err := loadPageJob(t.Context(), s.db, `id=?`, job.ID)
			require.NoError(t, err)
			require.Equal(t, "failed", retired.State)
			require.Equal(t, "stale_source", retired.FailureCode)
			require.Len(t, retired.Results, 2)
			var token string
			require.NoError(t, s.db.QueryRowContext(t.Context(), `SELECT token FROM page_render_jobs WHERE id=?`, job.ID).Scan(&token))
			require.Empty(t, token)
			var active int
			require.NoError(t, s.db.QueryRowContext(t.Context(), `SELECT count(*) FROM page_render_jobs WHERE state IN ('queued','running')`).Scan(&active))
			require.Zero(t, active)
			for _, receipt := range retired.Results {
				stored, err := loadPageImage(t.Context(), s.db, request.Source.VersionID, receipt.RecipeSHA256, receipt.Page)
				require.NoError(t, err)
				require.Equal(t, receipt, stored.Image)
			}
		})
	}
}

func TestPageJobCompletionDoesNotHideSourceDatabaseFailure(t *testing.T) {
	s := newTestStore(t)
	request := pageStoreRequest(t, s)
	_, err := s.QueuePageJob(t.Context(), uuid.NewString(), request)
	require.NoError(t, err)
	claim, err := s.ClaimPageJob(t.Context())
	require.NoError(t, err)
	frames := pageStoreFrames(t, request)
	require.NoError(t, s.PublishPageFrames(t.Context(), claim, frames))
	recipe := pageStoreRecipe()
	for _, frame := range frames {
		require.NoError(t, s.PublishPageImage(t.Context(), claim, pageStoreImage(t, frame, recipe), recipe, &BlobPhysical{Encoding: looseEncodingRaw, StoredBytes: 10}))
	}
	// A real SQLite source-query failure must not be mislabeled as staleness.
	_, err = s.db.ExecContext(t.Context(), `ALTER TABLE nodes RENAME TO unavailable_nodes`)
	require.NoError(t, err)
	defer func() {
		_, err := s.db.ExecContext(t.Context(), `ALTER TABLE unavailable_nodes RENAME TO nodes`)
		require.NoError(t, err)
	}()
	err = s.FinishPageJob(t.Context(), claim, "completed", "")
	require.Error(t, err)
	require.NotErrorIs(t, err, ErrPageFenced)
	job, err := loadPageJob(t.Context(), s.db, `id=?`, claim.Job.ID)
	require.NoError(t, err)
	require.Equal(t, "running", job.State)
	var token string
	require.NoError(t, s.db.QueryRowContext(t.Context(), `SELECT token FROM page_render_jobs WHERE id=?`, job.ID).Scan(&token))
	require.Equal(t, claim.Token, token)
}

func TestPageJobsConcurrentIdentityLimitsAndSourceFences(t *testing.T) {
	s := newTestStore(t)
	request := pageStoreRequest(t, s)
	var wg sync.WaitGroup
	ids := make(chan string, 8)
	errors := make(chan error, 8)
	for range 8 {
		wg.Go(func() {
			job, err := s.QueuePageJob(t.Context(), uuid.NewString(), request)
			ids <- job.ID
			errors <- err
		})
	}
	wg.Wait()
	close(ids)
	close(errors)
	for err := range errors {
		require.NoError(t, err)
	}
	var original string
	for id := range ids {
		if original == "" {
			original = id
		}
		require.Equal(t, original, id)
	}
	other := request
	other.DPI = 150
	second, err := s.QueuePageJob(t.Context(), uuid.NewString(), other)
	require.NoError(t, err)
	_, err = s.QueuePageJob(t.Context(), second.ID, request)
	require.ErrorIs(t, err, ErrPageConflict)
	for dpi := 151; dpi < 213; dpi++ {
		other.DPI = float64(dpi)
		_, err = s.QueuePageJob(t.Context(), uuid.NewString(), other)
		require.NoError(t, err)
	}
	other.DPI = 214
	_, err = s.QueuePageJob(t.Context(), uuid.NewString(), other)
	require.ErrorIs(t, err, ErrPageLimit)
	claim, err := s.ClaimPageJob(t.Context())
	require.NoError(t, err)
	_, err = s.db.ExecContext(t.Context(), `UPDATE nodes SET revision=revision+1 WHERE id=?`, request.NodeID)
	require.NoError(t, err)
	require.ErrorIs(t, s.PublishPageFrames(t.Context(), claim, pageStoreFrames(t, request)), ErrPageFenced)
}

func TestPageJobRejectsFailureCodeOnQueuedResult(t *testing.T) {
	s := newTestStore(t)
	request := pageStoreRequest(t, s)
	job, err := s.QueuePageJob(t.Context(), uuid.NewString(), request)
	require.NoError(t, err)
	job.FailureCode = "unsupported"
	require.Error(t, validatePageJob(job))
}

func TestPageAuthoritySurvivesCurrentSchemaReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pages.db")
	s, err := Open(path)
	require.NoError(t, err)
	request := pageStoreRequest(t, s)
	_, err = s.QueuePageJob(t.Context(), uuid.NewString(), request)
	require.NoError(t, err)
	claim, err := s.ClaimPageJob(t.Context())
	require.NoError(t, err)
	require.NoError(t, s.PublishPageFrames(t.Context(), claim, pageStoreFrames(t, request)))
	require.NoError(t, s.Close())
	reopened, err := Open(path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, reopened.Close()) })
	inventory, err := reopened.PageInventory(t.Context(), request.Binding())
	require.NoError(t, err)
	require.Len(t, inventory.Frames, 2)
	require.NoError(t, reopened.RequeuePageJobs(t.Context()))
	require.ErrorIs(t, reopened.CheckPageClaim(t.Context(), claim), ErrPageFenced)
}

package api_test

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json/v2"
	"fmt"
	"image"
	"image/png"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/bundle"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/backupapp"
	"go.kenn.io/docbank/internal/blob"
	"go.kenn.io/docbank/internal/config"
	"go.kenn.io/docbank/internal/exporter"
	"go.kenn.io/docbank/internal/store"
	"go.kenn.io/kit/backup"
)

// Pause exactly at the source-open gate so maintenance can acquire its real
// exclusive barrier without a large payload or timing-dependent disk fixture.
type pausedExportGate struct {
	gate        *api.OperationGate
	calls       atomic.Int32
	opening     chan struct{}
	resume      chan struct{}
	interrupted chan struct{}
}

func (g *pausedExportGate) MutateContext(ctx context.Context, fn func() error) error {
	if g.calls.Add(1) == 2 {
		close(g.opening)
		select {
		case <-g.resume:
		case <-ctx.Done():
		}
		err := g.gate.MutateContext(ctx, fn)
		if ctx.Err() != nil {
			close(g.interrupted)
		}
		return err
	}
	return g.gate.MutateContext(ctx, fn)
}

func TestExportRevocationInterruptsActiveWorkBehindMaintenance(t *testing.T) {
	gate := api.NewOperationGate()
	paused := &pausedExportGate{gate: gate, opening: make(chan struct{}), resume: make(chan struct{}), interrupted: make(chan struct{})}
	var worker *exporter.Worker
	ts, s := newTestServer(t, func(d *api.Deps) {
		d.Gate = gate
		var err error
		worker, err = exporter.New(d.Store, d.Blobs, d.VaultRoot, paused)
		require.NoError(t, err)
		d.Exports = worker
	})
	token := issueWebSession(t, ts)
	headers := map[string]string{"X-Api-Key": "", api.WebSessionHeader: token}
	owner := fmt.Sprintf("%x", sha256.Sum256([]byte(token)))
	hash, size, err := s.Blobs.Write(bytes.NewReader([]byte("synthetic cancellation payload")))
	require.NoError(t, err)
	n, err := s.CreateFile(t.Context(), s.RootID(), "cancel.txt", hash, size, "text/plain")
	require.NoError(t, err)
	source, err := s.CreateExportSource(t.Context(), owner, bundle.SourceRequest{OperationID: uuid.NewString(), Kind: "explicit", Members: []bundle.Member{{NodeID: n.ID, VersionID: n.CurrentVersionID, SHA256: hash, Size: size}}}, nil)
	require.NoError(t, err)
	plan, err := s.CreateExportPlan(t.Context(), owner, bundle.PlanRequest{OperationID: uuid.NewString(), SourceID: source.ID, MemberHash: source.MemberHash, Roles: []bundle.RolePolicy{{Role: "original"}}})
	require.NoError(t, err)
	job, err := s.QueueExportJob(t.Context(), owner, bundle.JobRequest{OperationID: uuid.NewString(), PlanID: plan.ID, Fingerprint: plan.Fingerprint})
	require.NoError(t, err)
	finished := make(chan error, 1)
	go func() { _, err := worker.RunOne(t.Context()); finished <- err }()
	select {
	case <-paused.opening:
	case <-time.After(5 * time.Second):
		t.Fatal("worker never reached source gate")
	}
	held, release := make(chan struct{}), make(chan struct{})
	maintenanceDone := make(chan error, 1)
	go func() { maintenanceDone <- gate.Maintain(func() error { close(held); <-release; return nil }) }()
	<-held
	close(paused.resume)
	request, err := http.NewRequestWithContext(t.Context(), http.MethodDelete, ts.URL+"/api/daemon/web-session", nil)
	require.NoError(t, err)
	for key, value := range headers {
		request.Header.Set(key, value)
	}
	type revokeResult struct {
		status int
		err    error
	}
	revoked := make(chan revokeResult, 1)
	go func() {
		response, err := ts.Client().Do(request)
		if err != nil {
			revoked <- revokeResult{err: err}
			return
		}
		revoked <- revokeResult{status: response.StatusCode, err: response.Body.Close()}
	}()
	select {
	case <-paused.interrupted:
	case <-time.After(time.Second):
		close(release)
		<-maintenanceDone
		<-revoked
		<-finished
		t.Fatal("revocation did not interrupt active export while maintenance blocked durable cancellation")
	}
	// The old five-second timeout must not abandon the durable mutation.
	select {
	case <-revoked:
		close(release)
		t.Fatal("revocation abandoned blocked durable cancellation")
	case <-time.After(5100 * time.Millisecond):
	}
	close(release)
	require.NoError(t, <-maintenanceDone)
	result := <-revoked
	require.NoError(t, result.err)
	require.Equal(t, http.StatusNoContent, result.status)
	require.NoError(t, <-finished)
	stored, err := s.ExportJob(t.Context(), owner, job.ID)
	require.NoError(t, err)
	require.Equal(t, "canceled", stored.State)
	require.Nil(t, stored.Receipt)
}

func TestExportAPIWorkerVerifiedTicketPreservesRetainedArchive(t *testing.T) {
	var worker *exporter.Worker
	ts, s := newTestServer(t, func(d *api.Deps) {
		var err error
		d.Gate = api.NewOperationGate()
		worker, err = exporter.New(d.Store, d.Blobs, d.VaultRoot, d.Gate)
		require.NoError(t, err)
		d.Exports = worker
	})
	n := createFileWithContent(t, ts, s, "/synthetic.txt", "synthetic original\n")
	response, body := do(t, ts, http.MethodPost, "/api/v1/exports/sources", nil, bundle.SourceRequest{OperationID: uuid.NewString(), Kind: "explicit", Members: []bundle.Member{{NodeID: n.ID, VersionID: n.CurrentVersionID, SHA256: n.BlobHash, Size: n.Size}}})
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	var source bundle.Source
	require.NoError(t, json.Unmarshal([]byte(body), &source))
	response, body = do(t, ts, http.MethodPost, "/api/v1/exports/plans", nil, bundle.PlanRequest{OperationID: uuid.NewString(), SourceID: source.ID, MemberHash: source.MemberHash, Roles: []bundle.RolePolicy{{Role: "original"}}})
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	var plan bundle.Plan
	require.NoError(t, json.Unmarshal([]byte(body), &plan))
	response, body = do(t, ts, http.MethodPost, "/api/v1/exports/jobs", nil, bundle.JobRequest{OperationID: uuid.NewString(), PlanID: plan.ID, Fingerprint: plan.Fingerprint})
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	var job bundle.Job
	require.NoError(t, json.Unmarshal([]byte(body), &job))
	processed, err := worker.RunOne(t.Context())
	require.NoError(t, err)
	require.True(t, processed)
	response, body = do(t, ts, http.MethodGet, "/api/v1/exports/jobs/"+job.ID, nil, nil)
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	require.NoError(t, json.Unmarshal([]byte(body), &job))
	require.Equal(t, "completed", job.State)
	// Invalid names fail before leasing, so a bad request cannot consume or
	// damage the retained archive. Unknown destination fields are rejected too.
	for _, tc := range []struct {
		request any
		status  int
	}{{bundle.DownloadRequest{Basename: "../unsafe.zip"}, http.StatusBadRequest}, {map[string]string{"destination": "/unsafe.zip"}, http.StatusUnprocessableEntity}} {
		response, body = do(t, ts, http.MethodPost, "/api/v1/exports/jobs/"+job.ID+"/download", nil, tc.request)
		require.Equal(t, tc.status, response.StatusCode, body)
	}
	for _, name := range []string{"", "Review 2026.zip"} {
		response, body = do(t, ts, http.MethodPost, "/api/v1/exports/jobs/"+job.ID+"/download", nil, bundle.DownloadRequest{Basename: name})
		require.Equal(t, http.StatusOK, response.StatusCode, body)
		var ticket struct {
			URL     string         `json:"url"`
			Receipt bundle.Receipt `json:"receipt"`
		}
		require.NoError(t, json.Unmarshal([]byte(body), &ticket))
		require.Equal(t, job.Receipt, &ticket.Receipt)
		resp, err := ts.Client().Get(ts.URL + ticket.URL)
		require.NoError(t, err)
		_, params, err := mime.ParseMediaType(resp.Header.Get("Content-Disposition"))
		require.NoError(t, err)
		if name == "" {
			name = "docbank-bundle.zip"
		}
		require.Equal(t, name, params["filename"])
		require.Equal(t, http.StatusOK, resp.StatusCode)
		archive, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		require.NoError(t, resp.Body.Close())
		z, err := zip.NewReader(bytes.NewReader(archive), int64(len(archive)))
		require.NoError(t, err)
		require.Len(t, z.File, 4)
		resp, err = ts.Client().Get(ts.URL + ticket.URL)
		require.NoError(t, err)
		require.Equal(t, http.StatusNotFound, resp.StatusCode)
		require.NoError(t, resp.Body.Close())
	}
}

func TestExportSavedQueryRetryDoesNotRerunChangedDefinition(t *testing.T) {
	ts, s := newTestServer(t, nil)
	n := createFileWithContent(t, ts, s, "/first.txt", "first synthetic")
	saved, err := s.CreateSavedQuery(t.Context(), "All synthetic", "", store.SavedQueryKindQuery, []byte(`{}`))
	require.NoError(t, err)
	request := bundle.SourceRequest{OperationID: uuid.NewString(), Kind: "saved_query", SavedQueryID: saved.ID, SavedQueryRevision: saved.Revision}
	response, body := do(t, ts, http.MethodPost, "/api/v1/exports/sources", nil, request)
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	var first bundle.Source
	require.NoError(t, json.Unmarshal([]byte(body), &first))
	require.Equal(t, 1, first.Total)
	_, err = s.UpdateSavedQuery(t.Context(), saved.ID, saved.Revision, store.SavedQueryPatch{Payload: new([]byte(`{"text":"nothingmatches"}`))})
	require.NoError(t, err)
	createFileWithContent(t, ts, s, "/second.txt", "second synthetic")
	response, body = do(t, ts, http.MethodPost, "/api/v1/exports/sources", nil, request)
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	var retry bundle.Source
	require.NoError(t, json.Unmarshal([]byte(body), &retry))
	require.Equal(t, first, retry)
	runs, err := s.ListSavedQueryRuns(t.Context(), saved.ID, 100)
	require.NoError(t, err)
	require.Len(t, runs, 1)
	response, body = rawJSONRequest(t, ts.URL, http.MethodPost, "/api/v1/exports/sources", map[string]string{"X-Api-Key": testAPIKey}, fmt.Sprintf(`{"operation_id":%q,"kind":"nodes","node_ids":[%d],"destination":"/arbitrary"}`, uuid.NewString(), n.ID))
	require.True(t, response.StatusCode == http.StatusBadRequest || response.StatusCode == http.StatusUnprocessableEntity, body)
}

func TestExportBrowserOwnersAndRevocationFenceJobs(t *testing.T) {
	var worker *exporter.Worker
	ts, s := newTestServer(t, func(d *api.Deps) {
		d.Gate = api.NewOperationGate()
		var err error
		worker, err = exporter.New(d.Store, d.Blobs, d.VaultRoot, d.Gate)
		require.NoError(t, err)
		d.Exports = worker
	})
	n := createFileWithContent(t, ts, s, "/synthetic.txt", "synthetic")
	first, second := issueWebSession(t, ts), issueWebSession(t, ts)
	headers := map[string]string{"X-Api-Key": "", api.WebSessionHeader: first}
	response, body := do(t, ts, http.MethodPost, "/api/v1/exports/sources", headers, bundle.SourceRequest{OperationID: uuid.NewString(), Kind: "nodes", NodeIDs: []int64{n.ID}})
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	var source bundle.Source
	require.NoError(t, json.Unmarshal([]byte(body), &source))
	response, body = do(t, ts, http.MethodPost, "/api/v1/exports/plans", headers, bundle.PlanRequest{OperationID: uuid.NewString(), SourceID: source.ID, MemberHash: source.MemberHash, Roles: []bundle.RolePolicy{{Role: "original"}}})
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	var plan bundle.Plan
	require.NoError(t, json.Unmarshal([]byte(body), &plan))
	response, body = do(t, ts, http.MethodGet, "/api/v1/exports/plans/"+plan.ID, map[string]string{"X-Api-Key": "", api.WebSessionHeader: second}, nil)
	require.Equal(t, http.StatusNotFound, response.StatusCode, body)
	response, body = do(t, ts, http.MethodPost, "/api/v1/exports/jobs", headers, bundle.JobRequest{OperationID: uuid.NewString(), PlanID: plan.ID, Fingerprint: plan.Fingerprint})
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	claim, err := s.ClaimExportJob(t.Context())
	require.NoError(t, err)
	response, body = do(t, ts, http.MethodDelete, "/api/daemon/web-session", headers, nil)
	require.Equal(t, http.StatusNoContent, response.StatusCode, body)
	require.ErrorIs(t, s.CheckExportClaim(t.Context(), claim), bundle.ErrFenced)
	response, body = do(t, ts, http.MethodPost, "/api/v1/exports/jobs/arbitrary/path", map[string]string{"X-Api-Key": "", api.WebSessionHeader: second}, struct{}{})
	require.Equal(t, http.StatusForbidden, response.StatusCode, body)
}

func TestExportDerivedRolesFreezeReceiptsAcrossHeadReplacement(t *testing.T) {
	var worker *exporter.Worker
	var cfg config.Config
	ts, s := newTestServer(t, func(d *api.Deps) {
		renditionTextConfig(d)
		cfg = d.Cfg
		d.Gate = api.NewOperationGate()
		var err error
		worker, err = exporter.New(d.Store, d.Blobs, d.VaultRoot, d.Gate)
		require.NoError(t, err)
		d.Exports = worker
	})
	fixture := publishRenditionTextFixture(t, s, cfg)
	n := fixture.node
	selected := document.PageSource{VersionID: n.CurrentVersionID, SHA256: n.BlobHash, Size: n.Size}
	request := store.PageJobRequest{NodeID: n.ID, Revision: n.Revision, Source: selected, Pages: []int{1}, DPI: 72, RuntimeFingerprint: testHash("synthetic-page-runtime")}
	_, err := s.QueuePageJob(t.Context(), uuid.NewString(), request)
	require.NoError(t, err)
	claim, err := s.ClaimPageJob(t.Context())
	require.NoError(t, err)
	frame, err := document.NewPDFPageFrame(selected, 1, [4]float64{0, 0, 72, 72}, [4]float64{0, 0, 72, 72}, 0)
	require.NoError(t, err)
	require.NoError(t, s.PublishPageFrames(t.Context(), claim, []document.PageFrameV1{frame}))
	recipe := document.PageRecipeV1{Contract: document.PageImageContractV1, DPI: 72, Format: "png", RendererIdentity: document.PageRendererIdentity{Executable: "synthetic-renderer", Version: "1", Options: []string{"crop-visible"}}}
	_, frameHash, err := document.MarshalPageFrameV1(frame)
	require.NoError(t, err)
	_, recipeHash, err := document.MarshalPageRecipeV1(recipe)
	require.NoError(t, err)
	var pixels bytes.Buffer
	require.NoError(t, png.Encode(&pixels, image.NewRGBA(image.Rect(0, 0, 72, 72))))
	imageHash, imageSize, err := s.Blobs.Write(bytes.NewReader(pixels.Bytes()))
	require.NoError(t, err)
	require.NoError(t, s.PublishPageImage(t.Context(), claim, document.PageImageV1{Contract: document.PageImageContractV1, Source: selected, Page: 1, FrameSHA256: frameHash, RecipeSHA256: recipeHash, SHA256: imageHash, Size: imageSize, Width: 72, Height: 72}, recipe, &store.BlobPhysical{Encoding: "raw", StoredBytes: imageSize}))
	require.NoError(t, s.FinishPageJob(t.Context(), claim, "completed", ""))
	source, err := s.CreateExportSource(t.Context(), "master", bundle.SourceRequest{OperationID: uuid.NewString(), Kind: "explicit", Members: []bundle.Member{{NodeID: n.ID, VersionID: n.CurrentVersionID, SHA256: n.BlobHash, Size: n.Size}}}, nil)
	require.NoError(t, err)
	plan, err := s.CreateExportPlan(t.Context(), "master", bundle.PlanRequest{OperationID: uuid.NewString(), SourceID: source.ID, MemberHash: source.MemberHash, Roles: []bundle.RolePolicy{{Role: "original"}, {Role: "text", ProfileFingerprint: fixture.profile.Fingerprint}, {Role: "pages", RecipeSHA256: recipeHash}}})
	require.NoError(t, err)
	require.Equal(t, 3, plan.RoleEntries)
	_, err = s.PurgeDerivatives(t.Context(), store.PurgeRequest{ContentVersionIDs: []string{n.CurrentVersionID}})
	require.ErrorIs(t, err, bundle.ErrRetained)
	newHash, newSize, err := s.Blobs.Write(bytes.NewReader([]byte("new synthetic head")))
	require.NoError(t, err)
	_, _, err = s.ReplaceContent(t.Context(), n.ID, n.Revision, newHash, newSize, "text/plain")
	require.NoError(t, err)
	job, err := s.QueueExportJob(t.Context(), "master", bundle.JobRequest{OperationID: uuid.NewString(), PlanID: plan.ID, Fingerprint: plan.Fingerprint})
	require.NoError(t, err)
	processed, err := worker.RunOne(t.Context())
	require.NoError(t, err)
	require.True(t, processed)
	response, body := do(t, ts, http.MethodPost, "/api/v1/exports/jobs/"+job.ID+"/download", nil, struct{}{})
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	var ticket struct {
		URL     string         `json:"url"`
		Receipt bundle.Receipt `json:"receipt"`
	}
	require.NoError(t, json.Unmarshal([]byte(body), &ticket))
	resp, err := ts.Client().Get(ts.URL + ticket.URL)
	require.NoError(t, err)
	archive, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	require.Equal(t, ticket.Receipt.Size, int64(len(archive)))
	z, err := zip.NewReader(bytes.NewReader(archive), int64(len(archive)))
	require.NoError(t, err)
	require.Len(t, z.File, 6)
	// Portable backup retains the frozen role authority, but never restores
	// runnable owner handles, private archives, or tickets.
	repository, err := backup.Init(filepath.Join(t.TempDir(), "backup"))
	require.NoError(t, err)
	_, err = backupapp.Create(t.Context(), repository, "synthetic-export-test", s.Store, s.Blobs, backup.CreateOptions{Jobs: 1})
	require.NoError(t, err)
	restoredRoot := filepath.Join(t.TempDir(), "restored")
	_, err = backupapp.Restore(t.Context(), repository, "synthetic-export-test", backup.RestoreOptions{TargetDir: restoredRoot, Jobs: 1})
	require.NoError(t, err)
	restored, err := store.OpenForRestore(filepath.Join(restoredRoot, "docbank.db"), store.DefaultSQLiteDriver())
	require.NoError(t, err)
	defer func() { require.NoError(t, restored.Close()) }()
	restoredBlobs, err := blob.New(store.NewPackCatalog(restored), filepath.Join(restoredRoot, "blobs"))
	require.NoError(t, err)
	defer func() { require.NoError(t, restoredBlobs.Close()) }()
	_, err = restored.ExportPlan(t.Context(), "master", plan.ID)
	require.ErrorIs(t, err, store.ErrNotFound)
	roleCount := 0
	require.NoError(t, restored.WalkExportDocuments(t.Context(), plan.ID, func(doc bundle.Document) error {
		for _, role := range doc.Roles {
			reader, size, err := restoredBlobs.OpenStreamContext(t.Context(), role.SHA256)
			require.NoError(t, err)
			require.Equal(t, role.Size, size)
			n, err := io.Copy(io.Discard, reader)
			require.NoError(t, err)
			require.Equal(t, role.Size, n)
			require.True(t, reader.Verified())
			require.NoError(t, reader.Close())
			roleCount++
		}
		return nil
	}))
	require.Equal(t, 3, roleCount)
	if target := os.Getenv("DOCBANK_EXPORT_PROOF"); target != "" {
		require.NoError(t, os.WriteFile(target, archive, 0600))
		t.Logf("proof_sha256=%s proof_size=%d plan=%s", ticket.Receipt.SHA256, ticket.Receipt.Size, plan.Fingerprint)
	}
}

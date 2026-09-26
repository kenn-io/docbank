package api_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document/bundle"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/exporter"
	"go.kenn.io/docbank/internal/store"
)

// These synthetic RED cases pin the native authenticated byte read. The
// browser's one-use download ticket is a separate operator flow.
type completedArchiveFixture struct {
	server  *httptest.Server
	catalog *testStore
	worker  *exporter.Worker
	node    store.Node
	job     bundle.Job
}

func completedArchiveReadFixture(t *testing.T) completedArchiveFixture {
	t.Helper()
	var worker *exporter.Worker
	ts, s := newTestServer(t, func(d *api.Deps) {
		d.Gate = api.NewOperationGate()
		var err error
		worker, err = exporter.New(d.Store, d.Blobs, d.VaultRoot, d.Gate)
		require.NoError(t, err)
		d.Exports = worker
	})
	node := createFileWithContent(t, ts, s, "/archive-read.txt", "synthetic archive read\n")
	job := completeArchiveReadJob(t, s, worker, node, "master")
	return completedArchiveFixture{server: ts, catalog: s, worker: worker, node: node, job: job}
}

func completeArchiveReadJob(t *testing.T, s *testStore, worker *exporter.Worker, node store.Node, owner string) bundle.Job {
	t.Helper()
	source, err := s.CreateExportSource(t.Context(), owner, bundle.SourceRequest{
		OperationID: uuid.NewString(), Kind: "explicit",
		Members: []bundle.Member{{NodeID: node.ID, VersionID: node.CurrentVersionID, SHA256: node.BlobHash, Size: node.Size}},
	}, nil)
	require.NoError(t, err)
	plan, err := s.CreateExportPlan(t.Context(), owner, bundle.PlanRequest{
		OperationID: uuid.NewString(), SourceID: source.ID, MemberHash: source.MemberHash,
		Roles: []bundle.RolePolicy{{Role: "original"}},
	})
	require.NoError(t, err)
	job, err := s.QueueExportJob(t.Context(), owner, bundle.JobRequest{
		OperationID: uuid.NewString(), PlanID: plan.ID, Fingerprint: plan.Fingerprint,
	})
	require.NoError(t, err)
	processed, err := worker.RunOne(t.Context())
	require.NoError(t, err)
	require.True(t, processed)
	job, err = s.ExportJob(t.Context(), owner, job.ID)
	require.NoError(t, err)
	require.Equal(t, "completed", job.State)
	require.NotNil(t, job.Receipt)
	return job
}

func TestNativeExportArchiveReadAuthenticatesAndHidesOtherOwner(t *testing.T) {
	f := completedArchiveReadFixture(t)
	path := "/api/v1/exports/jobs/" + f.job.ID + "/archive"
	response, body := do(t, f.server, http.MethodGet, path, map[string]string{"X-Api-Key": ""}, nil)
	require.Equal(t, http.StatusUnauthorized, response.StatusCode, body)

	webToken := issueWebSession(t, f.server)
	response, body = do(t, f.server, http.MethodGet, path,
		map[string]string{"X-Api-Key": "", api.WebSessionHeader: webToken}, nil)
	require.Equal(t, http.StatusForbidden, response.StatusCode, body)

	otherJob := completeArchiveReadJob(t, f.catalog, f.worker, f.node, "another-owner")
	response, body = do(t, f.server, http.MethodGet,
		"/api/v1/exports/jobs/"+otherJob.ID+"/archive", nil, nil)
	require.Equal(t, http.StatusNotFound, response.StatusCode, body)
}

func TestNativeExportArchiveReadIsDiscoverableAsBinaryGET(t *testing.T) {
	operation := api.NewOfflineServer().API().OpenAPI().Paths["/api/v1/exports/jobs/{id}/archive"]
	require.NotNil(t, operation)
	require.NotNil(t, operation.Get)
	require.Equal(t, "readExportArchive", operation.Get.OperationID)
	require.NotNil(t, operation.Get.Responses["200"])
	require.NotNil(t, operation.Get.Responses["200"].Content["application/zip"])
	if operation.Post != nil {
		require.Fail(t, "archive bytes must use the authenticated GET, not a second ticket POST")
	}
}

func TestNativeExportArchiveReadGeneratedClientsExposeBinaryGET(t *testing.T) {
	for _, fixture := range []struct {
		name    string
		path    string
		needles []string
	}{
		{"openapi", "../../openapi.yaml", []string{
			"/api/v1/exports/jobs/{id}/archive:", "operationId: readExportArchive", "application/zip:",
		}},
		{"go", "../apiclient/client.gen.go", []string{
			"func (c *Client) ReadExportArchive(", "type ReadExportArchiveResponse = []byte",
		}},
		{"typescript", "../../frontend/src/generated/docbank.ts", []string{
			"export const readExportArchive =", "return sessionResponse<Blob>(getReadExportArchiveUrl",
		}},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			data, err := os.ReadFile(fixture.path)
			require.NoError(t, err)
			for _, needle := range fixture.needles {
				require.Contains(t, string(data), needle)
			}
		})
	}
}

func TestNativeExportArchiveReadStreamsVerifiedCompletedReceipt(t *testing.T) {
	f := completedArchiveReadFixture(t)
	response, body := do(t, f.server, http.MethodGet,
		"/api/v1/exports/jobs/"+f.job.ID+"/archive", nil, nil)
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	require.Equal(t, "application/zip", response.Header.Get("Content-Type"))
	require.Equal(t, `attachment; filename="docbank-export.zip"`, response.Header.Get("Content-Disposition"))
	require.Equal(t, f.job.Receipt.PlanFingerprint, response.Header.Get("Docbank-Plan-Fingerprint"))
	require.Equal(t, f.job.Receipt.SHA256, response.Header.Get("Docbank-Archive-Sha256"))
	require.Equal(t, f.job.Receipt.Size, int64(len(body)))
	digest := sha256.Sum256([]byte(body))
	require.Equal(t, f.job.Receipt.SHA256, hex.EncodeToString(digest[:]))
	require.Equal(t, "no-store", response.Header.Get("Cache-Control"))
}

func TestNativeExportArchiveReadRejectsWithdrawnSourceBeforeBytes(t *testing.T) {
	f := completedArchiveReadFixture(t)
	_, _, err := f.catalog.Trash(t.Context(), f.node.ID, f.node.Revision)
	require.NoError(t, err)
	response, body := do(t, f.server, http.MethodGet,
		"/api/v1/exports/jobs/"+f.job.ID+"/archive", nil, nil)
	require.Equal(t, http.StatusConflict, response.StatusCode, body)
	require.Contains(t, body, `"code":"visibility_changed"`)
	require.NotEqual(t, "application/zip", response.Header.Get("Content-Type"))
}

func TestNativeExportArchiveReadKeepsFrozenVersionAfterHeadReplacement(t *testing.T) {
	f := completedArchiveReadFixture(t)
	hash, size, err := f.catalog.Blobs.Write(bytes.NewReader([]byte("new synthetic head\n")))
	require.NoError(t, err)
	_, _, err = f.catalog.ReplaceContent(t.Context(), f.node.ID, f.node.Revision, hash, size, "text/plain")
	require.NoError(t, err)
	response, body := do(t, f.server, http.MethodGet,
		"/api/v1/exports/jobs/"+f.job.ID+"/archive", nil, nil)
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	require.Equal(t, f.job.Receipt.Size, int64(len(body)))
	digest := sha256.Sum256([]byte(body))
	require.Equal(t, f.job.Receipt.SHA256, hex.EncodeToString(digest[:]))
}

func TestNativeExportArchiveReadRejectsTamperedRetainedBytesBeforeStream(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func([]byte) []byte
	}{
		{name: "same-size hash change", mutate: func(b []byte) []byte { b[len(b)/2] ^= 0x80; return b }},
		{name: "truncated", mutate: func(b []byte) []byte { return b[:len(b)-1] }},
		{name: "overrun", mutate: func(b []byte) []byte { return append(b, 0) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := completedArchiveReadFixture(t)
			archive := filepath.Join(filepath.Dir(f.catalog.DBPath), "export-archives", f.job.ID+".zip")
			original, err := os.ReadFile(archive)
			require.NoError(t, err)
			require.NotEmpty(t, original)
			require.NoError(t, os.WriteFile(archive, tc.mutate(bytes.Clone(original)), 0o600))

			request, err := http.NewRequestWithContext(t.Context(), http.MethodGet,
				f.server.URL+"/api/v1/exports/jobs/"+f.job.ID+"/archive", nil)
			require.NoError(t, err)
			response, err := f.server.Client().Do(request)
			require.NoError(t, err)
			defer func() { require.NoError(t, response.Body.Close()) }()
			require.NotEqual(t, http.StatusOK, response.StatusCode)
			require.Equal(t, "application/problem+json", response.Header.Get("Content-Type"))
			failure, err := io.ReadAll(io.LimitReader(response.Body, 4097))
			require.NoError(t, err)
			require.LessOrEqual(t, len(failure), 4096)
			require.NotEqual(t, original, failure)
		})
	}
}

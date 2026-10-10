package api_test

import (
	"archive/zip"
	"bytes"
	"fmt"
	"image"
	"image/png"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document/bundle"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/apiclient"
	"go.kenn.io/docbank/internal/daemonconn"
	"go.kenn.io/docbank/internal/exporter"
	"go.kenn.io/docbank/internal/query"
	"uuid"
)

func TestPhotoExportAPIPlanZIPAndTicket(t *testing.T) {
	t.Parallel()
	var worker *exporter.Worker
	ts, s := newTestServer(t, func(d *api.Deps) {
		var err error
		d.Gate = api.NewOperationGate()
		worker, err = exporter.New(d.Store, d.Blobs, d.VaultRoot, d.Gate)
		require.NoError(t, err)
		d.Exports = worker
	})
	var pixels bytes.Buffer
	require.NoError(t, png.Encode(&pixels, image.NewNRGBA(image.Rect(0, 0, 9, 6))))
	hash, size, err := s.Blobs.Write(bytes.NewReader(pixels.Bytes()))
	require.NoError(t, err)
	n, err := s.CreateFile(t.Context(), s.RootID(), "synthetic-export.png", hash, size, "image/png")
	require.NoError(t, err)
	client := daemonconn.New(ts.URL, testAPIKey).API()
	selection := bundle.PhotoExportSelection{Query: query.Query{V: 1, Syntax: "advanced", Mode: "lexical", Sort: query.Sort{Field: "name", Direction: "asc"}}}
	source, err := client.CreateExportSource(t.Context(), &apiclient.CreateExportSourceRequestOptions{Body: &bundle.SourceRequest{OperationID: uuid.New().String(), Kind: "photos", Photos: &selection}})
	require.NoError(t, err)
	require.Equal(t, 1, source.Total)
	r := bundle.PlanRequest{OperationID: uuid.New().String(), SourceID: source.ID, MemberHash: source.MemberHash, Roles: []bundle.RolePolicy{{Role: "photo_rendered"}}, PhotoRender: &bundle.PhotoRenderProfile{Format: "png", Quality: 90, LongEdge: 3, IncludeMetadata: true, RemoveGPS: true}}
	plan, err := client.CreateExportPlan(t.Context(), &apiclient.CreateExportPlanRequestOptions{Body: &r})
	require.NoError(t, err)
	retry, err := client.CreateExportPlan(t.Context(), &apiclient.CreateExportPlanRequestOptions{Body: &r})
	require.NoError(t, err)
	require.Equal(t, plan, retry)
	job, err := client.CreateExportJob(t.Context(), &apiclient.CreateExportJobRequestOptions{Body: &bundle.JobRequest{OperationID: uuid.New().String(), PlanID: plan.ID, Fingerprint: plan.Fingerprint}})
	require.NoError(t, err)
	processed, err := worker.RunOne(t.Context())
	require.NoError(t, err)
	require.True(t, processed)
	ticket, err := client.DownloadExportArchive(t.Context(), &apiclient.DownloadExportArchiveRequestOptions{PathParams: &apiclient.DownloadExportArchivePath{ID: job.ID}, Body: &bundle.DownloadRequest{Basename: "shared-photos.zip"}})
	require.NoError(t, err)
	response, err := ts.Client().Get(ts.URL + ticket.URL)
	require.NoError(t, err)
	defer func() { require.NoError(t, response.Body.Close()) }()
	require.Equal(t, http.StatusOK, response.StatusCode)
	data, err := io.ReadAll(response.Body)
	require.NoError(t, err)
	archive, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	require.NoError(t, err)
	want := fmt.Sprintf("documents/%d/%s/photo.png", n.ID, n.CurrentVersionID)
	found := false
	for _, file := range archive.File {
		reader, err := file.Open()
		require.NoError(t, err)
		contents, err := io.ReadAll(reader)
		require.NoError(t, err)
		require.NoError(t, reader.Close())
		if file.Name == want {
			found = true
			decoded, err := png.Decode(bytes.NewReader(contents))
			require.NoError(t, err)
			require.Equal(t, image.Rect(0, 0, 3, 2), decoded.Bounds())
			require.Contains(t, string(contents), "XML:com.adobe.xmp")
		}
		if strings.HasSuffix(file.Name, "manifest.jsonl") {
			require.Contains(t, string(contents), "photo_rendered")
		}
	}
	require.True(t, found)
}

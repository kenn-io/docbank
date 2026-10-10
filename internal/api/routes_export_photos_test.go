package api_test

import (
	"archive/zip"
	"bytes"
	"fmt"
	"image"
	"image/jpeg"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document/bundle"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/apiclient"
	"go.kenn.io/docbank/internal/daemonconn"
	"go.kenn.io/docbank/internal/exporter"
	"go.kenn.io/docbank/internal/query"
	"go.kenn.io/docbank/internal/store"
	"uuid"
)

func TestPhotoExportAPIPlanZIPAndTicket(t *testing.T) {
	t.Parallel()
	var worker *exporter.Worker
	var vault string
	ts, s := newTestServer(t, func(d *api.Deps) {
		var err error
		vault = d.VaultRoot
		d.Gate = api.NewOperationGate()
		worker, err = exporter.New(d.Store, d.Blobs, d.VaultRoot, d.Gate)
		require.NoError(t, err)
		d.Exports = worker
	})
	var pixels bytes.Buffer
	require.NoError(t, jpeg.Encode(&pixels, image.NewNRGBA(image.Rect(0, 0, 9, 6)), nil))
	packet := []byte("http://ns.adobe.com/xap/1.0/\x00" + `<x:xmpmeta xmlns:x="adobe:ns:meta/"><rdf:RDF xmlns:rdf="http://www.w3.org/1999/02/22-rdf-syntax-ns#"><rdf:Description xmlns:dc="http://purl.org/dc/elements/1.1/"><dc:subject><rdf:Bag><rdf:li>Embedded keyword</rdf:li></rdf:Bag></dc:subject></rdf:Description></rdf:RDF></x:xmpmeta>`)
	data := append([]byte{0xff, 0xd8, 0xff, 0xe1, byte((len(packet) + 2) >> 8), byte(len(packet) + 2)}, packet...)
	data = append(data, pixels.Bytes()[2:]...)
	hash, size, err := s.Blobs.Write(bytes.NewReader(data))
	require.NoError(t, err)
	n, err := s.CreateFile(t.Context(), s.RootID(), "synthetic-export.jpg", hash, size, "image/jpeg")
	require.NoError(t, err)
	tag, err := s.CreateTag(t.Context(), "Catalog keyword")
	require.NoError(t, err)
	_, err = s.AssignTag(t.Context(), tag.ID, n.ID, n.Revision)
	require.NoError(t, err)
	current, err := s.NodeByID(t.Context(), n.ID)
	require.NoError(t, err)
	_, err = s.UnassignTag(t.Context(), tag.ID, n.ID, current.Revision)
	require.NoError(t, err)
	asset, err := s.PhotoAssetForNode(t.Context(), n.ID)
	require.NoError(t, err)
	_, err = s.EditPhotoAuthored(t.Context(), []store.PhotoAuthoredTarget{{FileID: asset.Files[0].ID, Revision: 1, Patch: store.PhotoAuthoredPatch{Caption: new("Synthetic private caption"), Creator: new("Synthetic private creator"), Copyright: new("Synthetic private copyright")}}})
	require.NoError(t, err)
	client := daemonconn.New(ts.URL, testAPIKey).API()
	selection := bundle.PhotoExportSelection{Query: query.Query{V: 1, Syntax: "advanced", Mode: "lexical", Sort: query.Sort{Field: "name", Direction: "asc"}}}
	source, err := client.CreateExportSource(t.Context(), &apiclient.CreateExportSourceRequestOptions{Body: &bundle.SourceRequest{OperationID: uuid.New().String(), Kind: "photos", Photos: &selection}})
	require.NoError(t, err)
	require.Equal(t, 1, source.Total)
	r := bundle.PlanRequest{OperationID: uuid.New().String(), SourceID: source.ID, MemberHash: source.MemberHash, Roles: []bundle.RolePolicy{{Role: "photo_rendered"}}, PhotoRender: &bundle.PhotoRenderProfile{Format: "jpeg", Quality: 90, LongEdge: 3, IncludeMetadata: true, RemoveGPS: true}}
	abandoned := filepath.Join(vault, "export-archives", ".photo-export-"+strings.Repeat("a", 32))
	require.NoError(t, os.MkdirAll(abandoned, 0700))
	require.NoError(t, os.WriteFile(filepath.Join(abandoned, "payload.tmp"), []byte("abandoned"), 0600))
	old := time.Now().Add(-25 * time.Hour)
	require.NoError(t, os.Chtimes(abandoned, old, old))
	plan, err := client.CreateExportPlan(t.Context(), &apiclient.CreateExportPlanRequestOptions{Body: &r})
	require.NoError(t, err)
	_, err = os.Stat(abandoned)
	require.ErrorIs(t, err, os.ErrNotExist)
	retry, err := client.CreateExportPlan(t.Context(), &apiclient.CreateExportPlanRequestOptions{Body: &r})
	require.NoError(t, err)
	require.Equal(t, plan, retry)
	job, err := client.CreateExportJob(t.Context(), &apiclient.CreateExportJobRequestOptions{Body: &bundle.JobRequest{OperationID: uuid.New().String(), PlanID: plan.ID, Fingerprint: plan.Fingerprint}})
	require.NoError(t, err)
	processed, err := worker.RunOne(t.Context())
	require.NoError(t, err)
	require.True(t, processed)
	terminal, err := client.GetExportJob(t.Context(), &apiclient.GetExportJobRequestOptions{PathParams: &apiclient.GetExportJobPath{ID: job.ID}})
	require.NoError(t, err)
	require.Equal(t, "completed", terminal.State, "export job failed: %s", terminal.Failure)
	ticket, err := client.DownloadExportArchive(t.Context(), &apiclient.DownloadExportArchiveRequestOptions{PathParams: &apiclient.DownloadExportArchivePath{ID: job.ID}, Body: &bundle.DownloadRequest{Basename: "shared-photos.zip"}})
	require.NoError(t, err)
	response, err := ts.Client().Get(ts.URL + ticket.URL)
	require.NoError(t, err)
	defer func() { require.NoError(t, response.Body.Close()) }()
	require.Equal(t, http.StatusOK, response.StatusCode)
	archiveData, err := io.ReadAll(response.Body)
	require.NoError(t, err)
	archive, err := zip.NewReader(bytes.NewReader(archiveData), int64(len(archiveData)))
	require.NoError(t, err)
	want := fmt.Sprintf("documents/%d/%s/photo.jpg", n.ID, n.CurrentVersionID)
	found := false
	for _, file := range archive.File {
		reader, err := file.Open()
		require.NoError(t, err)
		contents, err := io.ReadAll(reader)
		require.NoError(t, err)
		require.NoError(t, reader.Close())
		if file.Name == want {
			found = true
			require.NotContains(t, string(contents), "Catalog keyword")
		}
		if strings.HasSuffix(file.Name, "manifest.jsonl") {
			require.Contains(t, string(contents), "photo_rendered")
		}
	}
	require.True(t, found)
}

func TestPhotoExportMetadataFailureNamesPhoto(t *testing.T) {
	t.Parallel()
	ts, s := newTestServer(t, nil)
	var pixels bytes.Buffer
	require.NoError(t, jpeg.Encode(&pixels, image.NewRGBA(image.Rect(0, 0, 9, 6)), nil))
	packet := `http://ns.adobe.com/xap/1.0/` + "\x00" + `<x:xmpmeta xmlns:x="adobe:ns:meta/"><rdf:RDF xmlns:rdf="http://www.w3.org/1999/02/22-rdf-syntax-ns#"><rdf:Description xmlns:keep="https://example.org/photo/" keep:Note="` + strings.Repeat("z", 60000) + `"/></rdf:RDF></x:xmpmeta>`
	data := append([]byte{0xff, 0xd8, 0xff, 0xe1, byte((len(packet) + 2) >> 8), byte(len(packet) + 2)}, []byte(packet)...)
	data = append(data, pixels.Bytes()[2:]...)
	hash, size, err := s.Blobs.Write(bytes.NewReader(data))
	require.NoError(t, err)
	n, err := s.CreateFile(t.Context(), s.RootID(), "oversized-credits.jpg", hash, size, "image/jpeg")
	require.NoError(t, err)
	asset, err := s.PhotoAssetForNode(t.Context(), n.ID)
	require.NoError(t, err)
	_, err = s.EditPhotoAuthored(t.Context(), []store.PhotoAuthoredTarget{{FileID: asset.Files[0].ID, Revision: 1, Patch: store.PhotoAuthoredPatch{Caption: new(strings.Repeat("c", store.MaxPhotoAuthoredTextBytes))}}})
	require.NoError(t, err)
	source, err := s.CreateExportSource(t.Context(), "master", bundle.SourceRequest{OperationID: uuid.New().String(), Kind: "explicit", Members: []bundle.Member{{NodeID: n.ID, VersionID: n.CurrentVersionID, SHA256: hash, Size: size}}}, nil)
	require.NoError(t, err)
	r := bundle.PlanRequest{OperationID: uuid.New().String(), SourceID: source.ID, MemberHash: source.MemberHash, Roles: []bundle.RolePolicy{{Role: "photo_rendered"}}, PhotoRender: &bundle.PhotoRenderProfile{Format: "jpeg", Quality: 90, IncludeMetadata: true}}
	response, body := do(t, ts, http.MethodPost, "/api/v1/exports/plans", nil, r)
	require.Equal(t, http.StatusUnprocessableEntity, response.StatusCode, body)
	require.Contains(t, body, "oversized-credits.jpg")
	_, err = s.ExportPlan(t.Context(), "master", r.OperationID)
	require.ErrorIs(t, err, store.ErrNotFound)
}

func TestPhotoExportUnavailableMemberNamesPhoto(t *testing.T) {
	for _, state := range []string{"non-photo", "trashed", "replaced"} {
		t.Run(state, func(t *testing.T) {
			t.Parallel()
			ts, s := newTestServer(t, nil)
			mediaType := "image/jpeg"
			if state == "non-photo" {
				mediaType = "text/plain"
			}
			hash, size, err := s.Blobs.Write(bytes.NewReader([]byte("synthetic member")))
			require.NoError(t, err)
			n, err := s.CreateFile(t.Context(), s.RootID(), "unavailable-member", hash, size, mediaType)
			require.NoError(t, err)
			source, err := s.CreateExportSource(t.Context(), "master", bundle.SourceRequest{OperationID: uuid.New().String(), Kind: "explicit", Members: []bundle.Member{{NodeID: n.ID, VersionID: n.CurrentVersionID, SHA256: hash, Size: size}}}, nil)
			require.NoError(t, err)
			switch state {
			case "trashed":
				_, _, err = s.Trash(t.Context(), n.ID, n.Revision)
			case "replaced":
				hash, size, err = s.Blobs.Write(bytes.NewReader([]byte("replacement member")))
				require.NoError(t, err)
				_, _, err = s.ReplaceContent(t.Context(), n.ID, n.Revision, hash, size, mediaType)
			}
			require.NoError(t, err)
			r := bundle.PlanRequest{OperationID: uuid.New().String(), SourceID: source.ID, MemberHash: source.MemberHash, Roles: []bundle.RolePolicy{{Role: "photo_rendered"}}, PhotoRender: &bundle.PhotoRenderProfile{Format: "jpeg", Quality: 90}}
			response, body := do(t, ts, http.MethodPost, "/api/v1/exports/plans", nil, r)
			require.Equal(t, http.StatusConflict, response.StatusCode, body)
			require.Contains(t, body, fmt.Sprintf("photo %d", n.ID))
			require.Contains(t, body, "no longer exportable")
		})
	}
}

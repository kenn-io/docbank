package api_test

import (
	"archive/zip"
	"bytes"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"image/color"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document/bundle"
	"go.kenn.io/docbank/document/media/mediatest"
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
	ts, s := newTestServer(t, func(d *api.Deps) {
		var err error
		d.Gate = api.NewOperationGate()
		worker, err = exporter.New(d.Store, d.Blobs, d.VaultRoot, d.Gate)
		require.NoError(t, err)
		d.Exports = worker
	})
	pixels := mediatest.JPEG(9, 6, color.Transparent)
	hash, size, err := s.Blobs.Write(bytes.NewReader(pixels))
	require.NoError(t, err)
	n, err := s.CreateFile(t.Context(), s.RootID(), "synthetic-export.jpg", hash, size, "image/jpeg")
	require.NoError(t, err)
	connection := daemonconn.New(ts.URL, testAPIKey)
	client := connection.API()
	require.NoError(t, s.SetupPhotoHidden(t.Context(), "synthetic-passcode"))
	asset, err := s.PhotoAssetForNode(t.Context(), n.ID)
	require.NoError(t, err)
	_, err = connection.SetPhotoAssetHidden(t.Context(), asset.ID, asset.Revision, true, "")
	require.NoError(t, err)
	_, cookie, err := connection.PhotoHidden(t.Context(), "unlock", "synthetic-passcode", "")
	require.NoError(t, err)
	headers := map[string]string{"Cookie": cookie}
	selection := bundle.PhotoExportSelection{Hidden: true, Query: query.Query{V: 1, Syntax: "advanced", Mode: "lexical", Sort: query.Sort{Field: "name", Direction: "asc"}}}
	sourceRequest := bundle.SourceRequest{OperationID: uuid.New().String(), Kind: "photos", Photos: &selection}
	response, body := do(t, ts, http.MethodPost, "/api/v1/exports/sources", nil, sourceRequest)
	require.Equal(t, http.StatusForbidden, response.StatusCode, body)
	sourceRequest.OperationID = uuid.New().String()
	response, body = do(t, ts, http.MethodPost, "/api/v1/exports/sources", headers, sourceRequest)
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	var source bundle.Source
	require.NoError(t, json.Unmarshal([]byte(body), &source))
	require.Equal(t, 1, source.Total)
	r := bundle.PlanRequest{OperationID: uuid.New().String(), SourceID: source.ID, MemberHash: source.MemberHash, Roles: []bundle.RolePolicy{{Role: "photo_rendered"}}, PhotoRender: &bundle.PhotoRenderProfile{Format: "png", Quality: 90, LongEdge: 3, IncludeMetadata: true, RemoveGPS: true}}
	response, body = do(t, ts, http.MethodPost, "/api/v1/exports/plans", nil, r)
	require.Equal(t, http.StatusForbidden, response.StatusCode, body)
	raw := fmt.Sprintf(`{"operation_id":%q,"source_id":%q,"member_hash":%q,"roles":[{"role":"photo_rendered"}],"photo_render":{"format":"png","long_edge":3,"include_metadata":true,"remove_gps":true}}`, r.OperationID, r.SourceID, r.MemberHash)
	response, body = do(t, ts, http.MethodPost, "/api/v1/exports/plans", headers, jsontext.Value(raw))
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	var plan bundle.Plan
	require.NoError(t, json.Unmarshal([]byte(body), &plan))
	func() {
		release, err := s.AcquirePhotoExportPreparation(t.Context())
		require.NoError(t, err)
		defer release()
		ts.Client().Timeout = time.Second
		defer func() { ts.Client().Timeout = 0 }()
		response, body = do(t, ts, http.MethodPost, "/api/v1/exports/plans", headers, jsontext.Value(raw))
	}()
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	var retry bundle.Plan
	require.NoError(t, json.Unmarshal([]byte(body), &retry))
	require.Equal(t, plan, retry)
	_, _, err = connection.PhotoHidden(t.Context(), "lock", "", cookie)
	require.NoError(t, err)
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
	response, err = ts.Client().Get(ts.URL + ticket.URL)
	require.NoError(t, err)
	defer func() { require.NoError(t, response.Body.Close()) }()
	require.Equal(t, http.StatusOK, response.StatusCode)
	archiveData, err := io.ReadAll(response.Body)
	require.NoError(t, err)
	archive, err := zip.NewReader(bytes.NewReader(archiveData), int64(len(archiveData)))
	require.NoError(t, err)
	want := fmt.Sprintf("documents/%d/%s/photo.png", n.ID, n.CurrentVersionID)
	found, foundManifest := false, false
	for _, file := range archive.File {
		reader, err := file.Open()
		require.NoError(t, err)
		contents, err := io.ReadAll(reader)
		require.NoError(t, err)
		require.NoError(t, reader.Close())
		if file.Name == want {
			found = true
		}
		if file.Name == "bundle.json" {
			foundManifest = true
			var manifest struct {
				Documents []bundle.Document `json:"documents"`
			}
			require.NoError(t, json.Unmarshal(contents, &manifest))
			require.Len(t, manifest.Documents, 1)
			require.Len(t, manifest.Documents[0].Roles, 1)
			role := manifest.Documents[0].Roles[0]
			require.Equal(t, "photo_rendered", role.Role)
			require.Equal(t, want, role.Path)
			var receipt bundle.PhotoRenderReceipt
			require.NoError(t, json.Unmarshal(role.Recipe, &receipt))
			require.Equal(t, r.PhotoRender.Canonical(), receipt.Profile)
			require.Equal(t, n.ID, receipt.Source.NodeID)
		}
	}
	require.True(t, found)
	require.True(t, foundManifest)
}

func TestPhotoExportUnavailableMemberNamesPhoto(t *testing.T) {
	t.Parallel()
	for _, state := range []string{"missing-blob", "replaced", "unsupported", "oversized-dimensions"} {
		t.Run(state, func(t *testing.T) {
			t.Parallel()
			ts, s := newTestServer(t, nil)
			mediaType := "image/jpeg"
			if state == "unsupported" {
				mediaType = "image/heic"
			}
			content := []byte("synthetic member")
			if state == "missing-blob" {
				content = mediatest.JPEG(2, 2, color.Transparent)
			}
			if state == "oversized-dimensions" {
				mediaType = "image/png"
				content = oversizedPNGHeader(10001, 10001)
			}

			hash, size, err := s.Blobs.Write(bytes.NewReader(content))
			require.NoError(t, err)
			var members []bundle.Member
			if state == "unsupported" {
				earlier, err := s.CreateFile(t.Context(), s.RootID(), "earlier-corrupt.jpg", hash, size, "image/jpeg")
				require.NoError(t, err)
				members = append(members, bundle.Member{NodeID: earlier.ID, VersionID: earlier.CurrentVersionID, SHA256: hash, Size: size})
			}
			n, err := s.CreateFile(t.Context(), s.RootID(), "unavailable-member", hash, size, mediaType)
			require.NoError(t, err)
			members = append(members, bundle.Member{NodeID: n.ID, VersionID: n.CurrentVersionID, SHA256: hash, Size: size})
			source, err := s.CreateExportSource(t.Context(), "master", bundle.SourceRequest{OperationID: uuid.New().String(), Kind: "explicit", Members: members}, nil)
			require.NoError(t, err)
			switch state {
			case "missing-blob":
				err = s.Blobs.Remove(hash)
			case "replaced":
				hash, size, err = s.Blobs.Write(bytes.NewReader([]byte("replacement member")))
				require.NoError(t, err)
				_, _, err = s.ReplaceContent(t.Context(), n.ID, n.Revision, hash, size, mediaType)
			}
			require.NoError(t, err)
			r := bundle.PlanRequest{OperationID: uuid.New().String(), SourceID: source.ID, MemberHash: source.MemberHash, Roles: []bundle.RolePolicy{{Role: "photo_rendered"}}, PhotoRender: &bundle.PhotoRenderProfile{Format: "jpeg", Quality: 90}}
			response, body := do(t, ts, http.MethodPost, "/api/v1/exports/plans", nil, r)
			_, err = s.ExportPlan(t.Context(), "master", r.OperationID)
			require.ErrorIs(t, err, store.ErrNotFound)
			if state == "missing-blob" {
				require.Equal(t, http.StatusUnprocessableEntity, response.StatusCode, body)
				require.Contains(t, body, "export_role_unavailable")
				require.Contains(t, body, fmt.Sprintf("photo %d (unavailable-member): source is missing: required export role is unavailable", n.ID))
				require.NotContains(t, body, hash)
				require.NotContains(t, body, s.BlobsDir)
				return
			}
			switch state {
			case "oversized-dimensions":
				require.Equal(t, http.StatusRequestEntityTooLarge, response.StatusCode, body)
				require.Contains(t, body, fmt.Sprintf("photo %d (unavailable-member)", n.ID))
			case "unsupported":
				require.Contains(t, body, fmt.Sprintf("photo %d", n.ID))
				require.Equal(t, http.StatusUnprocessableEntity, response.StatusCode, body)
				require.Contains(t, body, "unsupported photo media type image/heic")
			default:
				require.Contains(t, body, fmt.Sprintf("photo %d (unavailable-member)", n.ID))
				require.Equal(t, http.StatusConflict, response.StatusCode, body)
				require.Contains(t, body, "no longer exportable")
			}
		})
	}
}

func TestPhotoExportMalformedQuery(t *testing.T) {
	t.Parallel()
	ts, _ := newTestServer(t, nil)
	request := bundle.SourceRequest{OperationID: uuid.New().String(), Kind: "photos", Photos: &bundle.PhotoExportSelection{Query: query.Query{V: 1, Syntax: "advanced", Text: ")", Mode: "lexical", Sort: query.Sort{Field: "name", Direction: "asc"}}}}
	response, body := do(t, ts, http.MethodPost, "/api/v1/exports/sources", nil, request)
	require.Equal(t, http.StatusUnprocessableEntity, response.StatusCode, body)
	require.Contains(t, body, "invalid_query")
}

func TestPhotoExportClassifiesSourceReadFailures(t *testing.T) {
	t.Parallel()
	for _, state := range []string{"corrupt", "unreadable"} {
		t.Run(state, func(t *testing.T) {
			t.Parallel()
			if state == "unreadable" && runtime.GOOS == "windows" {
				t.Skip("chmod cannot deny reads on Windows")
			}
			if state == "unreadable" && os.Geteuid() == 0 {
				t.Skip("root ignores file permissions")
			}
			ts, s := newTestServer(t, nil)
			content := mediatest.JPEG(2, 2, color.White)
			hash, size, err := s.Blobs.Write(bytes.NewReader(content))
			require.NoError(t, err)
			n, err := s.CreateFile(t.Context(), s.RootID(), "damaged-source.jpg", hash, size, "image/jpeg")
			require.NoError(t, err)
			member := bundle.Member{NodeID: n.ID, VersionID: n.CurrentVersionID, SHA256: hash, Size: size}
			source, err := s.CreateExportSource(t.Context(), "master", bundle.SourceRequest{OperationID: uuid.New().String(), Kind: "explicit", Members: []bundle.Member{member}}, nil)
			require.NoError(t, err)
			paths, err := filepath.Glob(filepath.Join(s.BlobsDir, hash[:2], hash+"*"))
			require.NoError(t, err)
			require.Len(t, paths, 1)
			if state == "corrupt" {
				require.NoError(t, os.WriteFile(paths[0], bytes.Repeat([]byte{'x'}, len(content)), 0o600))
			} else {
				require.NoError(t, os.Chmod(paths[0], 0))
				t.Cleanup(func() { _ = os.Chmod(paths[0], 0o600) })
			}
			r := bundle.PlanRequest{OperationID: uuid.New().String(), SourceID: source.ID, MemberHash: source.MemberHash, Roles: []bundle.RolePolicy{{Role: "photo_rendered"}}, PhotoRender: &bundle.PhotoRenderProfile{Format: "jpeg", Quality: 90}}
			response, body := do(t, ts, http.MethodPost, "/api/v1/exports/plans", nil, r)
			if state == "corrupt" {
				require.Equal(t, http.StatusUnprocessableEntity, response.StatusCode, body)
				require.Contains(t, body, fmt.Sprintf("photo %d (damaged-source.jpg): source failed integrity verification; run docbank verify", n.ID))
			} else {
				require.Equal(t, http.StatusInternalServerError, response.StatusCode, body)
				require.Contains(t, body, "export_failed")
			}
			require.NotContains(t, body, hash)
			require.NotContains(t, body, s.BlobsDir)
		})
	}
}

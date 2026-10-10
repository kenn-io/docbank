package api_test

import (
	"archive/zip"
	"bytes"
	"encoding/json/jsontext"
	"encoding/json/v2"
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
	connection := daemonconn.New(ts.URL, testAPIKey)
	client := connection.API()
	require.NoError(t, s.SetupPhotoHidden(t.Context(), "synthetic-passcode"))
	asset, err = s.PhotoAssetForNode(t.Context(), n.ID)
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
	abandoned := filepath.Join(vault, "export-archives", ".photo-export-"+strings.Repeat("a", 32))
	require.NoError(t, os.MkdirAll(abandoned, 0700))
	require.NoError(t, os.WriteFile(filepath.Join(abandoned, "payload.tmp"), []byte("abandoned"), 0600))
	old := time.Now().Add(-25 * time.Hour)
	require.NoError(t, os.Chtimes(abandoned, old, old))
	response, body = do(t, ts, http.MethodPost, "/api/v1/exports/plans", nil, r)
	require.Equal(t, http.StatusForbidden, response.StatusCode, body)
	response, body = do(t, ts, http.MethodPost, "/api/v1/exports/plans", headers, r)
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	var plan bundle.Plan
	require.NoError(t, json.Unmarshal([]byte(body), &plan))
	require.Zero(t, plan.PhotoRender.Quality)
	r.PhotoRender.Quality = 20
	_, err = os.Stat(abandoned)
	require.ErrorIs(t, err, os.ErrNotExist)
	response, body = do(t, ts, http.MethodPost, "/api/v1/exports/plans", headers, r)
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	var retry bundle.Plan
	require.NoError(t, json.Unmarshal([]byte(body), &retry))
	require.Equal(t, plan, retry)
	_, _, err = connection.PhotoHidden(t.Context(), "lock", "", cookie)
	require.NoError(t, err)
	locked := r
	locked.OperationID = uuid.New().String()
	response, body = do(t, ts, http.MethodPost, "/api/v1/exports/plans", headers, locked)
	require.Equal(t, http.StatusForbidden, response.StatusCode, body)
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
			require.NotContains(t, string(contents), "Catalog keyword")
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
	for _, state := range []string{"non-photo", "trashed", "selection-trashed", "replaced", "mislabeled", "missing-blob", "damaged-blob", "unsupported", "oversized-metadata"} {
		t.Run(state, func(t *testing.T) {
			t.Parallel()
			ts, s := newTestServer(t, nil)
			mediaType := "image/jpeg"
			switch state {
			case "non-photo":
				mediaType = "text/plain"
			case "unsupported":
				mediaType = "image/heic"
			}
			content := []byte("synthetic member")
			if state == "mislabeled" || state == "missing-blob" || state == "damaged-blob" || state == "oversized-metadata" {
				if state == "mislabeled" {
					mediaType = "image/png"
				}
				var encoded bytes.Buffer
				require.NoError(t, jpeg.Encode(&encoded, image.NewRGBA(image.Rect(0, 0, 2, 2)), nil))
				content = encoded.Bytes()
				if state == "oversized-metadata" {
					packet := `http://ns.adobe.com/xap/1.0/` + "\x00" + `<x:xmpmeta xmlns:x="adobe:ns:meta/"><rdf:RDF xmlns:rdf="http://www.w3.org/1999/02/22-rdf-syntax-ns#"><rdf:Description xmlns:keep="https://example.org/photo/" keep:Note="` + strings.Repeat("z", 60000) + `"/></rdf:RDF></x:xmpmeta>`
					content = append([]byte{0xff, 0xd8, 0xff, 0xe1, byte((len(packet) + 2) >> 8), byte(len(packet) + 2)}, []byte(packet)...)
					content = append(content, encoded.Bytes()[2:]...)
				}
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
			if state == "selection-trashed" {
				asset, err := s.PhotoAssetForNode(t.Context(), n.ID)
				require.NoError(t, err)
				_, _, err = s.Trash(t.Context(), n.ID, n.Revision)
				require.NoError(t, err)
				request := bundle.SourceRequest{OperationID: uuid.New().String(), Kind: "photos", Photos: &bundle.PhotoExportSelection{AssetIDs: []string{asset.ID}, Query: query.Query{V: 1, Syntax: "advanced", Mode: "lexical", Sort: query.Sort{Field: "name", Direction: "asc"}}}}
				response, body := do(t, ts, http.MethodPost, "/api/v1/exports/sources", nil, request)
				require.Equal(t, http.StatusConflict, response.StatusCode, body)
				require.Contains(t, body, fmt.Sprintf("photo %d (unavailable-member)", n.ID))
				require.Contains(t, body, "no longer exportable")
				return
			}
			if state == "oversized-metadata" {
				asset, err := s.PhotoAssetForNode(t.Context(), n.ID)
				require.NoError(t, err)
				_, err = s.EditPhotoAuthored(t.Context(), []store.PhotoAuthoredTarget{{FileID: asset.Files[0].ID, Revision: 1, Patch: store.PhotoAuthoredPatch{Caption: new(strings.Repeat("c", store.MaxPhotoAuthoredTextBytes))}}})
				require.NoError(t, err)
			}
			source, err := s.CreateExportSource(t.Context(), "master", bundle.SourceRequest{OperationID: uuid.New().String(), Kind: "explicit", Members: members}, nil)
			require.NoError(t, err)
			switch state {
			case "missing-blob":
				err = s.Blobs.Remove(hash)
			case "damaged-blob":
				damaged := bytes.Clone(content)
				damaged[len(damaged)/2] ^= 1
				err = os.WriteFile(filepath.Join(s.BlobsDir, hash[:2], hash), damaged, 0600)
			case "trashed":
				_, _, err = s.Trash(t.Context(), n.ID, n.Revision)
			case "replaced":
				hash, size, err = s.Blobs.Write(bytes.NewReader([]byte("replacement member")))
				require.NoError(t, err)
				_, _, err = s.ReplaceContent(t.Context(), n.ID, n.Revision, hash, size, mediaType)
			}
			require.NoError(t, err)
			r := bundle.PlanRequest{OperationID: uuid.New().String(), SourceID: source.ID, MemberHash: source.MemberHash, Roles: []bundle.RolePolicy{{Role: "photo_rendered"}}, PhotoRender: &bundle.PhotoRenderProfile{Format: "jpeg", Quality: 90}}
			r.PhotoRender.IncludeMetadata = state == "oversized-metadata"
			response, body := do(t, ts, http.MethodPost, "/api/v1/exports/plans", nil, r)
			if state == "missing-blob" || state == "damaged-blob" {
				require.Equal(t, http.StatusUnprocessableEntity, response.StatusCode, body)
				require.Contains(t, body, "export_role_unavailable")
				require.Contains(t, body, fmt.Sprintf("photo %d (unavailable-member): source is missing or unreadable", n.ID))
				require.NotContains(t, body, hash)
				require.NotContains(t, body, s.BlobsDir)
				return
			}
			require.Contains(t, body, fmt.Sprintf("photo %d", n.ID))
			switch state {
			case "unsupported":
				require.Equal(t, http.StatusUnprocessableEntity, response.StatusCode, body)
				require.Contains(t, body, "unsupported photo media type image/heic")
			case "oversized-metadata":
				require.Equal(t, http.StatusUnprocessableEntity, response.StatusCode, body)
				_, err = s.ExportPlan(t.Context(), "master", r.OperationID)
				require.ErrorIs(t, err, store.ErrNotFound)
			case "mislabeled":
				require.Equal(t, http.StatusUnprocessableEntity, response.StatusCode, body)
				require.Contains(t, body, "unexpected image format")
			default:
				require.Equal(t, http.StatusConflict, response.StatusCode, body)
				require.Contains(t, body, "no longer exportable")
			}
		})
	}
}

func TestPhotoExportPNGWithoutQualityAndInvalidPlanSettings(t *testing.T) {
	t.Parallel()
	ts, s := newTestServer(t, nil)
	var encoded bytes.Buffer
	require.NoError(t, jpeg.Encode(&encoded, image.NewRGBA(image.Rect(0, 0, 2, 2)), nil))
	hash, size, err := s.Blobs.Write(bytes.NewReader(encoded.Bytes()))
	require.NoError(t, err)
	n, err := s.CreateFile(t.Context(), s.RootID(), "synthetic-settings.jpg", hash, size, "image/jpeg")
	require.NoError(t, err)
	source, err := s.CreateExportSource(t.Context(), "master", bundle.SourceRequest{OperationID: uuid.New().String(), Kind: "explicit", Members: []bundle.Member{{NodeID: n.ID, VersionID: n.CurrentVersionID, SHA256: hash, Size: size}}}, nil)
	require.NoError(t, err)
	for _, settings := range []struct {
		name   string
		extra  string
		status int
	}{
		{"png without quality", "", http.StatusOK},
		{"default duplicates", `,"duplicate_policy":"preserve"`, http.StatusOK},
		{"null quality", `,"quality":null`, http.StatusBadRequest},
		{"duplicate collapse", `,"duplicate_policy":"collapse_exact_content"`, http.StatusConflict},
		{"volume limits", `,"volume_limits":{"roles":1,"role_bytes":1024}`, http.StatusConflict},
	} {
		t.Run(settings.name, func(t *testing.T) {
			profileExtra, planExtra := "", settings.extra
			if settings.name == "null quality" {
				profileExtra, planExtra = settings.extra, ""
			}
			raw := fmt.Sprintf(`{"operation_id":%q,"source_id":%q,"member_hash":%q,"roles":[{"role":"photo_rendered"}],"photo_render":{"format":"png","long_edge":0,"include_metadata":false,"remove_gps":false%s}%s}`, uuid.New().String(), source.ID, source.MemberHash, profileExtra, planExtra)
			response, body := do(t, ts, http.MethodPost, "/api/v1/exports/plans", nil, jsontext.Value(raw))
			require.Equal(t, settings.status, response.StatusCode, body)
			switch settings.status {
			case http.StatusOK:
				var plan bundle.Plan
				require.NoError(t, json.Unmarshal([]byte(body), &plan))
				require.Zero(t, plan.PhotoRender.Quality)
			case http.StatusConflict:
				require.Contains(t, body, "photo exports take no duplicate_policy or volume_limits")
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

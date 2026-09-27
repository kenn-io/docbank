package store_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"image/color"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank"
	"go.kenn.io/docbank/document/redaction"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/blob"
	"go.kenn.io/docbank/internal/config"
	"go.kenn.io/docbank/internal/daemonconn"
	"go.kenn.io/docbank/internal/store"
)

func TestProductionPreviewRouteIssuesVerifiedImageAndTextTickets(t *testing.T) {
	fixture := newProductionPreviewHTTPFixture(t, true)
	body, err := json.Marshal(map[string]any{
		"operation_id": "89000000-0000-4000-8000-000000000045", "member_id": fixture.memberID, "page": 1,
	})
	require.NoError(t, err)
	request, err := http.NewRequest(http.MethodPost, fixture.ts.URL+fixture.path(), bytes.NewReader(body))
	require.NoError(t, err)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("If-Match", strconv.FormatInt(fixture.etag, 10))
	response, err := fixture.ts.Client().Do(request)
	require.NoError(t, err)
	defer func() { require.NoError(t, response.Body.Close()) }()
	require.Equal(t, http.StatusOK, response.StatusCode)
	var preview previewRouteResult
	require.NoError(t, json.UnmarshalRead(response.Body, &preview))
	require.Equal(t, "89000000-0000-4000-8000-000000000045", preview.OperationID)
	require.NotEmpty(t, preview.PreviewInputSHA256)
	for _, artifact := range []struct {
		URL, SHA256 string
		Size        int64
		Text        bool
	}{{preview.Image.URL, preview.Image.SHA256, preview.Image.Size, false},
		{preview.Text.URL, preview.Text.SHA256, preview.Text.Size, true}} {
		require.NotEmpty(t, artifact.URL)
		download, getErr := fixture.ts.Client().Get(fixture.ts.URL + artifact.URL)
		require.NoError(t, getErr)
		data, readErr := io.ReadAll(download.Body)
		require.NoError(t, readErr)
		require.NoError(t, download.Body.Close())
		require.Equal(t, http.StatusOK, download.StatusCode)
		require.Equal(t, artifact.Size, int64(len(data)))
		sum := sha256.Sum256(data)
		require.Equal(t, artifact.SHA256, hex.EncodeToString(sum[:]))
		if artifact.Text {
			require.Equal(t, "[REDACTED]", string(data))
		} else {
			image, decodeErr := png.Decode(bytes.NewReader(data))
			require.NoError(t, decodeErr)
			require.Equal(t, color.NRGBA{A: 255}, color.NRGBAModel.Convert(image.At(150, 150)))
		}
		again, getErr := fixture.ts.Client().Get(fixture.ts.URL + artifact.URL)
		require.NoError(t, getErr)
		require.Equal(t, http.StatusNotFound, again.StatusCode)
		require.NoError(t, again.Body.Close())
	}
}

func TestProductionPreviewRouteRequiresAuthentication(t *testing.T) {
	fixture := newProductionPreviewHTTPFixture(t, true)
	request, err := http.NewRequest(http.MethodPost, fixture.ts.URL+fixture.path(),
		bytes.NewReader([]byte(`{"operation_id":"89000000-0000-4000-8000-000000000054","member_id":"`+
			fixture.memberID+`","page":1}`)))
	require.NoError(t, err)
	request.Header["X-Api-Key"] = []string{""}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("If-Match", strconv.FormatInt(fixture.etag, 10))
	response, err := fixture.ts.Client().Do(request)
	require.NoError(t, err)
	require.Equal(t, http.StatusUnauthorized, response.StatusCode)
	require.NoError(t, response.Body.Close())
}

func TestProductionPreviewRouteUsesSelected600DPIRecipe(t *testing.T) {
	fixture := newProductionPreviewHTTPFixture(t, true)
	receipt, err := fixture.vault.ApplyProductionChanges(t.Context(), "synthetic-operator",
		fixture.setID, fixture.revision, redaction.ApplyRequest{
			OperationID: "89000000-0000-4000-8000-000000000055", ETag: fixture.etag,
			Changes: []redaction.Change{{Kind: "recipe", RecipeID: redaction.RecipeID600DPI}},
		})
	require.NoError(t, err)
	fixture.etag = receipt.ETag
	preview := fixture.preview(t, "89000000-0000-4000-8000-000000000056", &fixture.etag, "")
	response, err := fixture.ts.Client().Get(fixture.ts.URL + preview.Image.URL)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, response.StatusCode)
	image, err := png.Decode(response.Body)
	require.NoError(t, err)
	require.NoError(t, response.Body.Close())
	require.Equal(t, 600, image.Bounds().Dx())
	require.Equal(t, 600, image.Bounds().Dy())
}

type previewHTTPFixture struct {
	ts             *httptest.Server
	vault          *store.Store
	root, setID    string
	memberID       string
	revision, etag int64
}

func (fixture previewHTTPFixture) post(t *testing.T, operationID string, page int,
	etag *int64, session string) *http.Response {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"operation_id": operationID, "member_id": fixture.memberID, "page": page,
	})
	require.NoError(t, err)
	request, err := http.NewRequest(http.MethodPost, fixture.ts.URL+fixture.path(), bytes.NewReader(body))
	require.NoError(t, err)
	request.Header.Set("Content-Type", "application/json")
	if etag != nil {
		request.Header.Set("If-Match", strconv.FormatInt(*etag, 10))
	}
	if session != "" {
		request.Header["X-Api-Key"] = []string{""}
		request.Header.Set(api.WebSessionHeader, session)
	}
	response, err := fixture.ts.Client().Do(request)
	require.NoError(t, err)
	return response
}

func (fixture previewHTTPFixture) preview(t *testing.T, operationID string,
	etag *int64, session string) previewRouteResult {
	t.Helper()
	response := fixture.post(t, operationID, 1, etag, session)
	defer func() { require.NoError(t, response.Body.Close()) }()
	require.Equal(t, http.StatusOK, response.StatusCode)
	var result previewRouteResult
	require.NoError(t, json.UnmarshalRead(response.Body, &result))
	return result
}

func TestProductionPreviewRouteReplaysWithFreshTicketsAndRequiresIfMatch(t *testing.T) {
	fixture := newProductionPreviewHTTPFixture(t, true)
	const operationID = "89000000-0000-4000-8000-000000000046"
	first := fixture.preview(t, operationID, &fixture.etag, "")
	replayed := fixture.preview(t, operationID, &fixture.etag, "")
	require.Equal(t, first.PreviewInputSHA256, replayed.PreviewInputSHA256)
	require.Equal(t, first.Image.SHA256, replayed.Image.SHA256)
	require.Equal(t, first.Text.SHA256, replayed.Text.SHA256)
	require.NotEqual(t, first.Image.URL, replayed.Image.URL)
	require.NotEqual(t, first.Text.URL, replayed.Text.URL)
	changed := fixture.post(t, operationID, 2, &fixture.etag, "")
	require.Equal(t, http.StatusConflict, changed.StatusCode)
	require.NoError(t, changed.Body.Close())
	missing := fixture.post(t, "89000000-0000-4000-8000-000000000047", 1, nil, "")
	require.Equal(t, http.StatusPreconditionRequired, missing.StatusCode)
	require.NoError(t, missing.Body.Close())
	staleETag := fixture.etag + 1
	stale := fixture.post(t, "89000000-0000-4000-8000-000000000048", 1, &staleETag, "")
	require.Equal(t, http.StatusConflict, stale.StatusCode)
	require.NoError(t, stale.Body.Close())
}

func TestProductionPreviewRouteRevokesBothBrowserTickets(t *testing.T) {
	fixture := newProductionPreviewHTTPFixture(t, true)
	sessionResponse, err := fixture.ts.Client().Post(fixture.ts.URL+"/api/daemon/web-session", "", nil)
	require.NoError(t, err)
	require.Equal(t, http.StatusCreated, sessionResponse.StatusCode)
	var session struct {
		Token string `json:"token"`
	}
	require.NoError(t, json.UnmarshalRead(sessionResponse.Body, &session))
	require.NoError(t, sessionResponse.Body.Close())
	preview := fixture.preview(t, "89000000-0000-4000-8000-000000000049",
		&fixture.etag, session.Token)
	revoke, err := http.NewRequest(http.MethodDelete, fixture.ts.URL+"/api/daemon/web-session", nil)
	require.NoError(t, err)
	revoke.Header["X-Api-Key"] = []string{""}
	revoke.Header.Set(api.WebSessionHeader, session.Token)
	revoked, err := fixture.ts.Client().Do(revoke)
	require.NoError(t, err)
	require.Equal(t, http.StatusNoContent, revoked.StatusCode)
	require.NoError(t, revoked.Body.Close())
	for _, url := range []string{preview.Image.URL, preview.Text.URL} {
		response, getErr := fixture.ts.Client().Get(fixture.ts.URL + url)
		require.NoError(t, getErr)
		require.Equal(t, http.StatusNotFound, response.StatusCode)
		require.NoError(t, response.Body.Close())
	}
	entries, err := os.ReadDir(filepath.Join(fixture.root, "web-downloads"))
	require.NoError(t, err)
	require.Empty(t, entries)
}

func TestProductionPreviewRouteMissingPDFPublishesNoTicket(t *testing.T) {
	fixture := newProductionPreviewHTTPFixture(t, false)
	response := fixture.post(t, "89000000-0000-4000-8000-000000000050", 1, &fixture.etag, "")
	require.Equal(t, http.StatusConflict, response.StatusCode)
	var problem struct {
		Code string `json:"code"`
	}
	require.NoError(t, json.UnmarshalRead(response.Body, &problem))
	require.NoError(t, response.Body.Close())
	require.Equal(t, "production_preview_unavailable", problem.Code)
	entries, err := os.ReadDir(filepath.Join(fixture.root, "web-downloads"))
	require.NoError(t, err)
	require.Empty(t, entries)
}

func TestProductionPreviewGeneratedClientReceivesBoundedTicket(t *testing.T) {
	fixture := newProductionPreviewHTTPFixture(t, true)
	const operationID = "89000000-0000-4000-8000-000000000051"
	result, err := daemonconn.New(fixture.ts.URL, "synthetic-api-key").ProductionPreview(
		t.Context(), fixture.setID, fixture.revision, fixture.etag,
		api.ProductionPreviewRequest{OperationID: operationID, MemberID: fixture.memberID, Page: 1})
	require.NoError(t, err)
	require.Equal(t, operationID, result.OperationID)
	require.NotEmpty(t, result.Image.URL)
	require.NotEmpty(t, result.Text.URL)
	require.NotEqual(t, result.Image.URL, result.Text.URL)
}

func TestProductionPreviewEmbeddedVaultUsesItsOwnRoot(t *testing.T) {
	seed := newClosedPreviewVaultFixture(t)
	first, err := docbank.New(t.Context(), docbank.Config{Root: seed.root})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, first.Close()) })
	second, err := docbank.New(t.Context(), docbank.Config{Root: t.TempDir()})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, second.Close()) })
	request := docbank.ProductionPreviewRequest{OperationID: "89000000-0000-4000-8000-000000000052",
		MemberID: seed.memberID, Page: 1}
	_, err = second.ProductionPreview(t.Context(), "synthetic-operator",
		seed.setID, seed.revision, seed.etag, request)
	require.ErrorIs(t, err, store.ErrNotFound)
	preview, err := first.ProductionPreview(t.Context(), "synthetic-operator",
		seed.setID, seed.revision, seed.etag, request)
	require.NoError(t, err)
	require.Equal(t, "[REDACTED]", string(preview.Text))
	require.NotEmpty(t, preview.Image)
	sum := sha256.Sum256(preview.Image)
	require.Equal(t, hex.EncodeToString(sum[:]), preview.ImageSHA256)
	sum = sha256.Sum256(preview.Text)
	require.Equal(t, hex.EncodeToString(sum[:]), preview.TextSHA256)
	image, err := png.Decode(bytes.NewReader(preview.Image))
	require.NoError(t, err)
	require.Equal(t, color.NRGBA{A: 255}, color.NRGBAModel.Convert(image.At(150, 150)))
	entries, err := os.ReadDir(seed.root)
	require.NoError(t, err)
	for _, entry := range entries {
		require.NotContains(t, entry.Name(), ".production-preview-")
	}
}

func TestProductionPreviewEmbeddedVaultSweepsAbandonedStageOnOpen(t *testing.T) {
	root := t.TempDir()
	abandoned := filepath.Join(root, ".production-preview-abandoned")
	require.NoError(t, os.Mkdir(abandoned, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(abandoned, "synthetic-preview.png"),
		[]byte("abandoned synthetic bytes"), 0o600))
	vault, err := docbank.New(t.Context(), docbank.Config{Root: root})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, vault.Close()) })
	_, err = os.Stat(abandoned)
	require.ErrorIs(t, err, os.ErrNotExist)
}

type closedPreviewVaultFixture struct {
	root, setID, memberID string
	revision, etag        int64
}

func newClosedPreviewVaultFixture(t *testing.T) closedPreviewVaultFixture {
	t.Helper()
	seed, root, set, draft, member, sourcePDF := store.ProductionPreviewStageFixture(t)
	blobs, err := blob.New(store.NewPackCatalog(seed), filepath.Join(root, "blobs"))
	require.NoError(t, err)
	written, err := blobs.WriteDetailedContext(t.Context(), bytes.NewReader(sourcePDF))
	require.NoError(t, err)
	require.Equal(t, member.PDFSHA256, written.Hash)
	require.NoError(t, seed.RecordBlob(t.Context(), written.Hash, written.Size, store.BlobPhysical{
		Encoding: "raw", StoredBytes: written.StoredSize, Created: written.Created,
	}))
	require.NoError(t, blobs.Close())
	require.NoError(t, seed.Close())
	return closedPreviewVaultFixture{root: root, setID: set.ID, memberID: member.ID,
		revision: draft.Revision, etag: draft.ETag}
}

// This proof builds and runs the actual CLI daemon against a synthetic vault.
// It is opt-in because every invocation compiles and spawns a separate process.
func TestProductionPreviewRealDaemonRestartKeepsAdmissionButNotTickets(t *testing.T) {
	if os.Getenv("DOCBANK_PREVIEW_REAL_DAEMON") != "1" {
		t.Skip("opt-in real daemon preview proof")
	}
	seed := newClosedPreviewVaultFixture(t)
	name := "docbank"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	binary := filepath.Join(t.TempDir(), name)
	build := exec.CommandContext(t.Context(), "go", "build", "-tags", "fts5", "-o", binary,
		"go.kenn.io/docbank/cmd/docbank")
	output, err := build.CombinedOutput()
	require.NoError(t, err, string(output))
	stop := func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_, err := daemonconn.Stop(ctx, seed.root)
		require.NoError(t, err)
	}
	t.Cleanup(stop)
	start := func() (*daemonconn.Connection, string, string) {
		t.Helper()
		cmd := exec.CommandContext(t.Context(), binary, "daemon", "start")
		cmd.Env = append(os.Environ(), "DOCBANK_HOME="+seed.root)
		output, err := cmd.CombinedOutput()
		require.NoError(t, err, string(output))
		record, _, found, err := daemonconn.Find(t.Context(), seed.root)
		require.NoError(t, err)
		require.True(t, found)
		base := "http://" + record.Address
		key := record.Metadata["api_key"]
		require.NotEmpty(t, key)
		return daemonconn.New(base, key), base, key
	}
	get := func(base, key, path string) *http.Response {
		t.Helper()
		request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, base+path, nil)
		require.NoError(t, err)
		request.Header.Set("X-Api-Key", key)
		response, err := http.DefaultClient.Do(request)
		require.NoError(t, err)
		return response
	}
	firstClient, firstBase, firstKey := start()
	request := api.ProductionPreviewRequest{OperationID: "89000000-0000-4000-8000-000000000053",
		MemberID: seed.memberID, Page: 1}
	first, err := firstClient.ProductionPreview(t.Context(), seed.setID, seed.revision, seed.etag, request)
	require.NoError(t, err)
	for _, ticket := range []api.ProductionPreviewArtifactTicket{first.Image, first.Text} {
		response := get(firstBase, firstKey, ticket.URL)
		data, err := io.ReadAll(response.Body)
		require.NoError(t, err)
		require.NoError(t, response.Body.Close())
		require.Equal(t, http.StatusOK, response.StatusCode)
		sum := sha256.Sum256(data)
		require.Equal(t, ticket.SHA256, hex.EncodeToString(sum[:]))
	}
	// Keep one unconsumed ticket across restart to prove ephemeral publication.
	oldTicket, err := firstClient.ProductionPreview(t.Context(), seed.setID,
		seed.revision, seed.etag, request)
	require.NoError(t, err)
	stop()
	secondClient, secondBase, secondKey := start()
	stale := get(secondBase, secondKey, oldTicket.Image.URL)
	require.Equal(t, http.StatusNotFound, stale.StatusCode)
	require.NoError(t, stale.Body.Close())
	replayed, err := secondClient.ProductionPreview(t.Context(), seed.setID,
		seed.revision, seed.etag, request)
	require.NoError(t, err)
	require.Equal(t, first.PreviewInputSHA256, replayed.PreviewInputSHA256)
	require.Equal(t, first.Image.SHA256, replayed.Image.SHA256)
	require.Equal(t, first.Text.SHA256, replayed.Text.SHA256)
	require.NotEqual(t, oldTicket.Image.URL, replayed.Image.URL)
	entries, err := os.ReadDir(filepath.Join(seed.root, "web-downloads"))
	require.NoError(t, err)
	require.Len(t, entries, 2, "only the replay's two staged artifacts remain")
}

func (fixture previewHTTPFixture) path() string {
	return "/api/v1/productions/sets/" + fixture.setID + "/revisions/" +
		strconv.FormatInt(fixture.revision, 10) + "/previews"
}

func newProductionPreviewHTTPFixture(t *testing.T, withPDF bool) previewHTTPFixture {
	t.Helper()
	const apiKey = "synthetic-api-key"
	vault, root, set, draft, member, sourcePDF := store.ProductionPreviewStageFixture(t)
	blobs, err := blob.New(store.NewPackCatalog(vault), filepath.Join(root, "blobs"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, blobs.Close()) })
	if withPDF {
		written, writeErr := blobs.WriteDetailedContext(t.Context(), bytes.NewReader(sourcePDF))
		require.NoError(t, writeErr)
		require.Equal(t, member.PDFSHA256, written.Hash)
		require.NoError(t, vault.RecordBlob(t.Context(), written.Hash, written.Size, store.BlobPhysical{
			Encoding: "raw", StoredBytes: written.StoredSize, Created: written.Created,
		}))
	}
	cfg := config.Default()
	cfg.Server.APIKey = apiKey
	d := api.Deps{Store: vault, Blobs: blobs, VaultRoot: root, Cfg: cfg,
		WebURL: "http://docbank-0123456789abcdef0123456789abcdef.localhost:43210/"}
	server := api.NewServer(d)
	t.Cleanup(server.Close)
	ts := httptest.NewServer(server.Handler())
	t.Cleanup(ts.Close)
	ts.Client().Transport = previewAPIKeyTransport{key: apiKey, next: ts.Client().Transport}
	return previewHTTPFixture{ts: ts, vault: vault, root: root, setID: set.ID,
		memberID: member.ID, revision: draft.Revision, etag: draft.ETag}
}

type previewRouteResult struct {
	OperationID        string `json:"operation_id"`
	PreviewInputSHA256 string `json:"preview_input_sha256"`
	Image              struct {
		URL    string `json:"url"`
		SHA256 string `json:"sha256"`
		Size   int64  `json:"size"`
	} `json:"image"`
	Text struct {
		URL    string `json:"url"`
		SHA256 string `json:"sha256"`
		Size   int64  `json:"size"`
	} `json:"text"`
}

type previewAPIKeyTransport struct {
	key  string
	next http.RoundTripper
}

func (transport previewAPIKeyTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	request = request.Clone(request.Context())
	if _, explicit := request.Header["X-Api-Key"]; !explicit {
		request.Header.Set("X-Api-Key", transport.key)
	}
	return transport.next.RoundTrip(request)
}

package api_test

import (
	"encoding/json/v2"
	"maps"
	"net/http"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/store"
)

func revisionHeaders(headers map[string]string, revision int64) map[string]string {
	out := map[string]string{"If-Match": strconv.Quote(strconv.FormatInt(revision, 10))}
	maps.Copy(out, headers)
	return out
}

func trashPageNames(t *testing.T, f photoRouteFixture, headers map[string]string) ([]string, int) {
	t.Helper()
	response, body := get(t, f.ts, "/api/v1/trash", headers)
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	var page api.TrashPage
	require.NoError(t, json.Unmarshal([]byte(body), &page))
	names := make([]string, 0, len(page.Items))
	for _, item := range page.Items {
		names = append(names, item.Name)
	}
	return names, page.Total
}

// TestPhotoOwnerFolderTrash keeps owner folders out of an owner's trash
// reach and keeps trashed contents with their owner.
func TestPhotoOwnerFolderTrash(t *testing.T) {
	f := newPhotoRouteFixture(t, nil)
	ctx := t.Context()
	bob := issuePhotoOwnerSession(t, f.ts, ownerHeader(f.bob))
	alice := issuePhotoOwnerSession(t, f.ts, ownerHeader(f.alice))
	photos, err := f.s.NodeByPath(ctx, "/photos")
	require.NoError(t, err)
	bobFolder, err := f.s.NodeByPath(ctx, "/photos/"+f.bob.ID)
	require.NoError(t, err)

	t.Run("owner_cannot_trash_structure", func(t *testing.T) {
		for _, node := range []store.Node{photos, bobFolder} {
			response, body := do(t, f.ts, http.MethodPost, "/api/v1/nodes/"+strconv.FormatInt(node.ID, 10)+"/trash",
				revisionHeaders(bob, node.Revision), nil)
			require.Equal(t, http.StatusUnprocessableEntity, response.StatusCode, body)
			assert.Contains(t, body, `"code":"is_root"`)
			t.Logf("%s: %d is_root", node.Name, response.StatusCode)
		}
	})

	// Alice trashes her photo, then the master key trashes her whole folder.
	response, body := do(t, f.ts, http.MethodPost, "/api/v1/nodes/"+strconv.FormatInt(f.aliceNode.ID, 10)+"/trash",
		revisionHeaders(alice, f.aliceNode.Revision), nil)
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	aliceFolder, err := f.s.NodeByPath(ctx, "/photos/"+f.alice.ID)
	require.NoError(t, err)
	t.Run("master_trashes_owner_folder", func(t *testing.T) {
		response, body := do(t, f.ts, http.MethodPost, "/api/v1/nodes/"+strconv.FormatInt(aliceFolder.ID, 10)+"/trash",
			revisionHeaders(nil, aliceFolder.Revision), nil)
		require.Equal(t, http.StatusOK, response.StatusCode, body)
		t.Logf("master: %d", response.StatusCode)
	})

	t.Run("trash_listing", func(t *testing.T) {
		// Browser sessions cannot list trash, so the master key selects each owner.
		names, total := trashPageNames(t, f, ownerHeader(f.bob))
		assert.NotContains(t, names, "alice.jpg")
		assert.NotContains(t, names, f.alice.ID)
		assert.Zero(t, total)
		t.Logf("bob roots=%v total=%d", names, total)
		names, total = trashPageNames(t, f, ownerHeader(f.alice))
		assert.ElementsMatch(t, []string{"alice.jpg", f.alice.ID}, names)
		assert.Equal(t, 2, total)
		t.Logf("alice roots=%v total=%d", names, total)
	})

	t.Run("restore_answers_missing", func(t *testing.T) {
		trashed, err := f.s.NodeByID(ctx, f.aliceNode.ID)
		require.NoError(t, err)
		key := strconv.FormatInt(trashed.ID, 10)
		missing := strconv.FormatInt(trashed.ID+100000, 10)
		response, body := do(t, f.ts, http.MethodPost, "/api/v1/nodes/"+key+"/restore", revisionHeaders(bob, trashed.Revision), nil)
		hidden := photoAnswerOf(t, response, body, key)
		response, body = do(t, f.ts, http.MethodPost, "/api/v1/nodes/"+missing+"/restore", revisionHeaders(bob, trashed.Revision), nil)
		require.Equal(t, http.StatusNotFound, hidden.status)
		require.Equal(t, photoAnswerOf(t, response, body, missing), hidden)
		t.Logf("hidden answer: %+v", hidden)
	})
}

// TestPhotoOwnerFolderWrites keeps an owner-bound request from creating
// anything inside another owner's folder.
func TestPhotoOwnerFolderWrites(t *testing.T) {
	f := newPhotoRouteFixture(t, nil)
	intruder := "/photos/" + f.alice.ID + "/Intruder"
	response, body := do(t, f.ts, http.MethodPost, "/api/v1/path/mkdir", ownerHeader(f.bob), map[string]string{"path": intruder})
	require.Equal(t, http.StatusNotFound, response.StatusCode, body)
	_, err := f.s.NodeByPath(t.Context(), intruder)
	require.ErrorIs(t, err, store.ErrNotFound, "nothing is created")
	t.Logf("mkdir as bob: %d", response.StatusCode)

	response, body = do(t, f.ts, http.MethodPost, "/api/v1/path/mkdir", ownerHeader(f.bob), map[string]string{"path": "/photos/" + f.bob.ID + "/Trip"})
	assert.Equal(t, http.StatusCreated, response.StatusCode, body)
}

// TestPhotoOwnerTotals clamps node-based totals to what the request sees.
func TestPhotoOwnerTotals(t *testing.T) {
	f := newPhotoRouteFixture(t, configureProcessingTestService(t))
	bob := issuePhotoOwnerSession(t, f.ts, ownerHeader(f.bob))

	t.Run("collection_quality", func(t *testing.T) {
		for _, test := range []struct {
			name    string
			headers map[string]string
			files   int64
		}{{"bob", bob, 3}, {"master", nil, 4}} {
			response, body := get(t, f.ts, "/api/v1/collections/"+f.collectionID+"/quality", test.headers)
			require.Equal(t, http.StatusOK, response.StatusCode, body)
			var quality api.CollectionQuality
			require.NoError(t, json.Unmarshal([]byte(body), &quality))
			assert.Equal(t, test.files, quality.Collection.FileCount)
			assert.Equal(t, test.files, quality.DuplicateDocuments, "duplicates count only visible copies")
			t.Logf("%s file_count=%d duplicate_documents=%d", test.name, quality.Collection.FileCount, quality.DuplicateDocuments)
		}
	})

	t.Run("processing_coverage", func(t *testing.T) {
		read := func(headers map[string]string, versionID string) api.CoverageReport {
			t.Helper()
			response, body := get(t, f.ts, "/api/v1/coverage?profile=private&vault_uid="+
				f.s.VaultID()+"&content_version_id="+versionID, headers)
			require.Equal(t, http.StatusOK, response.StatusCode, body)
			var report api.CoverageReport
			require.NoError(t, json.Unmarshal([]byte(body), &report))
			return report
		}
		hidden := read(bob, f.aliceNode.CurrentVersionID)
		unknown := read(bob, missingPhotoUUID)
		assert.Equal(t, unknown.Renditions, hidden.Renditions)
		assert.Equal(t, 1, hidden.Renditions.Stale)
		master := read(nil, f.aliceNode.CurrentVersionID)
		assert.Zero(t, master.Renditions.Stale)
		t.Logf("bob renditions=%+v unknown=%+v master=%+v", hidden.Renditions, unknown.Renditions, master.Renditions)
	})
}

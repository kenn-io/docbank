package api_test

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/store"
)

type scopedTagGrantAuthority map[string]api.Principal

type scopedTagFixture struct {
	server    *httptest.Server
	catalog   *testStore
	first     store.Node
	second    store.Node
	emptyDir  store.Node
	taggedDir store.Node
	tag       store.Tag
}

func (a scopedTagGrantAuthority) CurrentGrant(_ context.Context, subject string) (api.Principal, error) {
	grant, ok := a[subject]
	if !ok {
		return api.Principal{}, api.ErrOperationGrantRevoked
	}
	return grant, nil
}

func newScopedTagPolicyServer(t *testing.T) scopedTagFixture {
	t.Helper()
	now := time.Date(2026, time.September, 23, 12, 0, 0, 0, time.UTC)
	var first, second, emptyDir, taggedDir store.Node
	var tag store.Tag
	ts, catalog := newTestServer(t, func(deps *api.Deps) {
		first = createPolicyFile(t, deps, "synthetic-first.txt", "first")
		second = createPolicyFile(t, deps, "synthetic-second.txt", "second")
		var err error
		emptyDir, err = deps.Store.Mkdir(t.Context(), deps.Store.RootID(), "synthetic-empty")
		require.NoError(t, err)
		taggedDir, err = deps.Store.Mkdir(t.Context(), deps.Store.RootID(), "synthetic-tagged")
		require.NoError(t, err)
		tag, err = deps.Store.CreateTag(t.Context(), "shared-synthetic")
		require.NoError(t, err)
		for _, node := range []store.Node{first, second, taggedDir} {
			_, err = deps.Store.AssignTag(t.Context(), tag.ID, node.ID, node.Revision)
			require.NoError(t, err)
		}
		tag, err = deps.Store.TagByID(t.Context(), tag.ID)
		require.NoError(t, err)
		first, err = deps.Store.NodeByID(t.Context(), first.ID)
		require.NoError(t, err)
		second, err = deps.Store.NodeByID(t.Context(), second.ID)
		require.NoError(t, err)
		taggedDir, err = deps.Store.NodeByID(t.Context(), taggedDir.ID)
		require.NoError(t, err)
		firstGrant := routePrincipal(now, first.CurrentVersionID)
		firstGrant.SubjectID = "synthetic:first"
		firstGrant.Operations = append(firstGrant.Operations, api.OperationMetadataMutation)
		secondGrant := routePrincipal(now, second.CurrentVersionID)
		secondGrant.SubjectID = "synthetic:second"
		secondGrant.Operations = append(secondGrant.Operations, api.OperationMetadataMutation)
		deps.OperationPolicy = api.NewOperationPolicy(api.OperationPolicyOptions{
			Authority: scopedTagGrantAuthority{firstGrant.SubjectID: firstGrant, secondGrant.SubjectID: secondGrant},
			Now:       func() time.Time { return now },
		})
		deps.AuthenticatePrincipal = func(request *http.Request) (api.Principal, bool) {
			switch request.Header.Get("Authorization") {
			case "Bearer synthetic-first":
				return firstGrant, true
			case "Bearer synthetic-second":
				return secondGrant, true
			default:
				return api.Principal{}, false
			}
		}
	})
	require.Empty(t, emptyDir.CurrentVersionID)
	require.Empty(t, taggedDir.CurrentVersionID)
	return scopedTagFixture{ts, catalog, first, second, emptyDir, taggedDir, tag}
}

func scopedTagHeaders(token string) map[string]string {
	return map[string]string{"X-Api-Key": "", "Authorization": "Bearer synthetic-" + token}
}

func TestScopedPrincipalsCannotMutateGlobalTagDefinitions(t *testing.T) {
	fixture := newScopedTagPolicyServer(t)
	ts, catalog, tag := fixture.server, fixture.catalog, fixture.tag
	for _, token := range []string{"first", "second"} {
		t.Run(token, func(t *testing.T) {
			headers := scopedTagHeaders(token)
			response, body := do(t, ts, http.MethodPost, "/api/v1/tags", headers,
				map[string]any{"name": "remote-" + token})
			require.Equal(t, http.StatusForbidden, response.StatusCode, body)
			_, err := catalog.TagByName(t.Context(), "remote-"+token)
			require.ErrorIs(t, err, store.ErrNotFound, "global tag was created: %v", err)

			headers["If-Match"] = strconv.Quote(strconv.FormatInt(tag.Revision, 10))
			response, body = do(t, ts, http.MethodPatch, "/api/v1/tags/"+tag.ID, headers,
				map[string]any{"name": "remote-renamed-" + token})
			require.Equal(t, http.StatusForbidden, response.StatusCode, body)
			response, body = do(t, ts, http.MethodDelete, "/api/v1/tags/"+tag.ID, headers, nil)
			require.Equal(t, http.StatusForbidden, response.StatusCode, body)
		})
	}
	unchanged, err := catalog.TagByID(t.Context(), tag.ID)
	require.NoError(t, err)
	require.Equal(t, tag, unchanged)

	response, body := do(t, ts, http.MethodPost, "/api/v1/tags", nil,
		map[string]any{"name": "local-created"})
	require.Equal(t, http.StatusCreated, response.StatusCode, body)
}

func TestScopedPrincipalsCannotAssignTagsToDirectories(t *testing.T) {
	fixture := newScopedTagPolicyServer(t)
	ts, catalog, first, emptyDir, taggedDir, tag := fixture.server, fixture.catalog,
		fixture.first, fixture.emptyDir, fixture.taggedDir, fixture.tag
	for _, token := range []string{"first", "second"} {
		t.Run(token, func(t *testing.T) {
			headers := scopedTagHeaders(token)
			headers["If-Match"] = strconv.Quote(strconv.FormatInt(emptyDir.Revision, 10))
			response, body := do(t, ts, http.MethodPut,
				fmt.Sprintf("/api/v1/nodes/%d/tags/%s", emptyDir.ID, tag.ID), headers, nil)
			require.Equal(t, http.StatusNotFound, response.StatusCode, body)
			headers["If-Match"] = strconv.Quote(strconv.FormatInt(taggedDir.Revision, 10))
			response, body = do(t, ts, http.MethodDelete,
				fmt.Sprintf("/api/v1/nodes/%d/tags/%s", taggedDir.ID, tag.ID), headers, nil)
			require.Equal(t, http.StatusNotFound, response.StatusCode, body)
		})
	}
	unchanged, err := catalog.TagByID(t.Context(), tag.ID)
	require.NoError(t, err)
	require.Equal(t, tag, unchanged)
	for _, original := range []store.Node{emptyDir, taggedDir} {
		current, err := catalog.NodeByID(t.Context(), original.ID)
		require.NoError(t, err)
		require.Equal(t, original.Revision, current.Revision)
	}

	response, body := do(t, ts, http.MethodPut,
		fmt.Sprintf("/api/v1/nodes/%d/tags/%s", emptyDir.ID, tag.ID),
		map[string]string{"If-Match": strconv.Quote(strconv.FormatInt(emptyDir.Revision, 10))}, nil)
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	response, body = do(t, ts, http.MethodDelete,
		fmt.Sprintf("/api/v1/nodes/%d/tags/%s", taggedDir.ID, tag.ID),
		map[string]string{"If-Match": strconv.Quote(strconv.FormatInt(taggedDir.Revision, 10))}, nil)
	require.Equal(t, http.StatusOK, response.StatusCode, body)

	fileTag, err := catalog.CreateTag(t.Context(), "file-only")
	require.NoError(t, err)
	path := fmt.Sprintf("/api/v1/nodes/%d/tags/%s", first.ID, fileTag.ID)
	response, body = do(t, ts, http.MethodPut, path,
		map[string]string{"X-Api-Key": "", "Authorization": "Bearer synthetic-second",
			"If-Match": strconv.Quote(strconv.FormatInt(first.Revision, 10))}, nil)
	require.Equal(t, http.StatusNotFound, response.StatusCode, body)
	response, body = do(t, ts, http.MethodPut, path,
		map[string]string{"X-Api-Key": "", "Authorization": "Bearer synthetic-first",
			"If-Match": strconv.Quote(strconv.FormatInt(first.Revision, 10))}, nil)
	require.Equal(t, http.StatusNotFound, response.StatusCode, body)
	response, body = do(t, ts, http.MethodPut, path,
		map[string]string{"If-Match": strconv.Quote(strconv.FormatInt(first.Revision, 10))}, nil)
	require.Equal(t, http.StatusOK, response.StatusCode, body)
}

func TestScopedTagAssignmentRejectsHiddenTagSelector(t *testing.T) {
	for _, method := range []string{http.MethodPut, http.MethodDelete} {
		t.Run(method, func(t *testing.T) {
			fixture := newScopedTagPolicyServer(t)
			hidden, err := fixture.catalog.CreateTag(t.Context(), "hidden-synthetic")
			require.NoError(t, err)
			_, err = fixture.catalog.AssignTag(t.Context(), hidden.ID, fixture.second.ID, fixture.second.Revision)
			require.NoError(t, err)

			headers := scopedTagHeaders("first")
			headers["If-Match"] = strconv.Quote(strconv.FormatInt(fixture.first.Revision, 10))
			path := fmt.Sprintf("/api/v1/nodes/%d/tags/%s", fixture.first.ID, hidden.ID)
			response, body := do(t, fixture.server, method, path, headers, nil)
			require.Equal(t, http.StatusNotFound, response.StatusCode, body)
			require.NotContains(t, body, hidden.Name)

			missing := fmt.Sprintf("/api/v1/nodes/%d/tags/%s", fixture.first.ID,
				"11111111-1111-4111-8111-111111111111")
			missingResponse, _ := do(t, fixture.server, method, missing, headers, nil)
			require.Equal(t, missingResponse.StatusCode, response.StatusCode)
		})
	}
}

func TestScopedTagListIgnoresSupersededGrantVersion(t *testing.T) {
	fixture := newScopedTagPolicyServer(t)
	oldVersionID := fixture.first.CurrentVersionID
	_, replacement, err := fixture.catalog.ReplaceContent(t.Context(), fixture.first.ID,
		fixture.first.Revision, testHash("synthetic-replacement"), 21, "text/plain")
	require.NoError(t, err)
	require.NotEqual(t, oldVersionID, replacement.ID)

	response, body := get(t, fixture.server, "/api/v1/tags?limit=100&offset=0", scopedTagHeaders("first"))
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	var page api.TagPage
	require.NoError(t, json.Unmarshal([]byte(body), &page))
	require.Zero(t, page.Total)
	require.Empty(t, page.Items)
}

func TestScopedTagAssignmentReceiptCountsOnlyVisibleSources(t *testing.T) {
	fixture := newScopedTagPolicyServer(t)
	ts, first, second, tag := fixture.server, fixture.first, fixture.second, fixture.tag
	for _, actor := range []struct {
		token string
		node  store.Node
	}{{"first", first}, {"second", second}} {
		t.Run(actor.token, func(t *testing.T) {
			headers := scopedTagHeaders(actor.token)
			headers["If-Match"] = strconv.Quote(strconv.FormatInt(actor.node.Revision, 10))
			response, body := do(t, ts, http.MethodPut,
				fmt.Sprintf("/api/v1/nodes/%d/tags/%s", actor.node.ID, tag.ID), headers, nil)
			require.Equal(t, http.StatusOK, response.StatusCode, body)
			var receipt api.TagAssignmentReceipt
			require.NoError(t, json.Unmarshal([]byte(body), &receipt))
			require.False(t, receipt.Changed)
			require.Equal(t, 1, receipt.Tag.AssignmentCount)
		})
	}
	response, body := do(t, ts, http.MethodPut,
		fmt.Sprintf("/api/v1/nodes/%d/tags/%s", first.ID, tag.ID),
		map[string]string{"If-Match": strconv.Quote(strconv.FormatInt(first.Revision, 10))}, nil)
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	var local api.TagAssignmentReceipt
	require.NoError(t, json.Unmarshal([]byte(body), &local))
	require.Equal(t, 3, local.Tag.AssignmentCount)
}

func TestScopedTagRevisionAndETagIgnoreHiddenAssignments(t *testing.T) {
	fixture := newScopedTagPolicyServer(t)
	firstHeaders := scopedTagHeaders("first")
	firstPath := fmt.Sprintf("/api/v1/nodes/%d/tags/%s", fixture.first.ID, fixture.tag.ID)
	firstHeaders["If-Match"] = strconv.Quote(strconv.FormatInt(fixture.first.Revision, 10))
	readFirst := func() (api.Tag, api.TagAssignmentReceipt, string) {
		t.Helper()
		response, body := get(t, fixture.server, "/api/v1/tags?limit=100&offset=0", firstHeaders)
		require.Equal(t, http.StatusOK, response.StatusCode, body)
		var page api.TagPage
		require.NoError(t, json.Unmarshal([]byte(body), &page))
		require.Len(t, page.Items, 1)
		response, body = do(t, fixture.server, http.MethodPut, firstPath, firstHeaders, nil)
		require.Equal(t, http.StatusOK, response.StatusCode, body)
		var receipt api.TagAssignmentReceipt
		require.NoError(t, json.Unmarshal([]byte(body), &receipt))
		require.False(t, receipt.Changed)
		return page.Items[0], receipt, response.Header.Get("ETag")
	}
	beforeList, beforeReceipt, beforeETag := readFirst()
	require.Equal(t, int64(1), beforeList.Revision)
	require.Equal(t, int64(1), beforeReceipt.Tag.Revision)

	secondHeaders := scopedTagHeaders("second")
	secondHeaders["If-Match"] = strconv.Quote(strconv.FormatInt(fixture.second.Revision, 10))
	response, body := do(t, fixture.server, http.MethodDelete,
		fmt.Sprintf("/api/v1/nodes/%d/tags/%s", fixture.second.ID, fixture.tag.ID), secondHeaders, nil)
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	var secondReceipt api.TagAssignmentReceipt
	require.NoError(t, json.Unmarshal([]byte(body), &secondReceipt))
	require.True(t, secondReceipt.Changed)

	afterList, afterReceipt, afterETag := readFirst()
	require.Equal(t, beforeList.Revision, afterList.Revision)
	require.Equal(t, beforeList.AssignmentCount, afterList.AssignmentCount)
	require.Equal(t, beforeReceipt.Tag.Revision, afterReceipt.Tag.Revision)
	require.Equal(t, beforeReceipt.Tag.AssignmentCount, afterReceipt.Tag.AssignmentCount)
	require.Equal(t, beforeETag, afterETag)
	require.Equal(t, beforeReceipt.Node.Revision, afterReceipt.Node.Revision)

	ownerTag, err := fixture.catalog.TagByID(t.Context(), fixture.tag.ID)
	require.NoError(t, err)
	require.Equal(t, fixture.tag.Revision+1, ownerTag.Revision)
	require.Equal(t, fixture.tag.AssignmentCount-1, ownerTag.AssignmentCount)
	renamed, err := fixture.catalog.RenameTag(t.Context(), fixture.tag.ID, ownerTag.Revision, "renamed-synthetic")
	require.NoError(t, err)
	require.Equal(t, ownerTag.Revision+1, renamed.Revision)
	renamedNode, err := fixture.catalog.NodeByID(t.Context(), fixture.first.ID)
	require.NoError(t, err)
	response, body = do(t, fixture.server, http.MethodPut, firstPath, firstHeaders, nil)
	require.Equal(t, http.StatusPreconditionFailed, response.StatusCode, body)
	firstHeaders["If-Match"] = strconv.Quote(strconv.FormatInt(renamedNode.Revision, 10))
	renamedList, renamedReceipt, renamedETag := readFirst()
	require.Equal(t, "renamed-synthetic", renamedList.Name)
	require.Equal(t, "renamed-synthetic", renamedReceipt.Tag.Name)
	require.Equal(t, beforeList.Revision, renamedList.Revision)
	require.Equal(t, beforeReceipt.Tag.Revision, renamedReceipt.Tag.Revision)
	require.Equal(t, firstHeaders["If-Match"], renamedETag)
	require.NotEqual(t, beforeETag, renamedETag)
}

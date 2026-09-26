package store_test

import (
	"bytes"
	"encoding/json/v2"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document/redaction"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/blob"
	"go.kenn.io/docbank/internal/config"
	"go.kenn.io/docbank/internal/store"
)

func TestProductionDecisionCheckHTTPIsExactAndReadOnly(t *testing.T) {
	vault, root, set, draft, member, _ := store.ProductionPreviewStageFixture(t)
	blobs, err := blob.New(store.NewPackCatalog(vault), filepath.Join(root, "blobs"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, blobs.Close()) })
	cfg := config.Default()
	cfg.Server.APIKey = "synthetic-api-key"
	server := api.NewServer(api.Deps{Store: vault, Blobs: blobs, VaultRoot: root, Cfg: cfg})
	t.Cleanup(server.Close)
	httpServer := httptest.NewServer(server.Handler())
	t.Cleanup(httpServer.Close)
	path := "/api/v1/productions/sets/" + set.ID + "/revisions/1/decisions/check"
	decision := redaction.Decision{ID: "89000000-0000-4000-8000-000000000055", MemberID: member.ID,
		Action: "redact", Reason: "synthetic private reason", Selector: redaction.Selector{
			Kind: "page", MapSHA256: member.MapSHA256, Pages: []int{1}}}
	call := func(targetPath string, proposed redaction.Decision, etag int64, authorized bool) (int, []byte) {
		t.Helper()
		payload, err := json.Marshal(struct {
			Decision redaction.Decision `json:"decision"`
		}{proposed})
		require.NoError(t, err)
		request, err := http.NewRequestWithContext(t.Context(), http.MethodPost,
			httpServer.URL+targetPath, bytes.NewReader(payload))
		require.NoError(t, err)
		request.Header.Set("Content-Type", "application/json")
		if authorized {
			request.Header.Set("X-Api-Key", cfg.Server.APIKey)
		}
		if etag > 0 {
			request.Header.Set("If-Match", strconv.FormatInt(etag, 10))
		}
		response, err := httpServer.Client().Do(request)
		require.NoError(t, err)
		data, err := io.ReadAll(response.Body)
		require.NoError(t, err)
		require.NoError(t, response.Body.Close())
		return response.StatusCode, data
	}
	status, _ := call(path, decision, draft.ETag, false)
	require.Equal(t, http.StatusUnauthorized, status)
	status, _ = call(path, decision, 0, true)
	require.Equal(t, http.StatusPreconditionRequired, status)
	status, _ = call(path, decision, draft.ETag+1, true)
	require.Equal(t, http.StatusConflict, status)
	status, body := call(path, decision, draft.ETag, true)
	require.Equal(t, http.StatusOK, status, string(body))
	require.NotContains(t, string(body), decision.Reason)
	var result struct {
		SetID      string `json:"set_id"`
		Revision   int64  `json:"revision"`
		ETag       int64  `json:"etag"`
		MemberID   string `json:"member_id"`
		DecisionID string `json:"decision_id"`
		Outcome    string `json:"outcome"`
	}
	require.NoError(t, json.Unmarshal(body, &result))
	require.Equal(t, set.ID, result.SetID)
	require.Equal(t, draft.Revision, result.Revision)
	require.Equal(t, draft.ETag, result.ETag)
	require.Equal(t, member.ID, result.MemberID)
	require.Equal(t, decision.ID, result.DecisionID)
	require.Equal(t, "ready", result.Outcome)
	fresh, err := vault.ProductionDraft(t.Context(), set.ID, draft.Revision)
	require.NoError(t, err)
	require.Equal(t, draft.ETag, fresh.ETag)

	undecidedSet, undecidedDraft, err := vault.CreateProductionSet(t.Context(), "synthetic-operator",
		redaction.CreateRequest{OperationID: "89000000-0000-4000-8000-000000000056", Name: "Synthetic expansion check"})
	require.NoError(t, err)
	_, err = vault.ApplyProductionChanges(t.Context(), "synthetic-operator", undecidedSet.ID, 1,
		redaction.ApplyRequest{OperationID: "89000000-0000-4000-8000-000000000057", ETag: undecidedDraft.ETag,
			Changes: []redaction.Change{{Kind: "member", Member: &member}}})
	require.NoError(t, err)
	undecidedDraft, err = vault.ProductionDraft(t.Context(), undecidedSet.ID, 1)
	require.NoError(t, err)
	frame, err := vault.ProductionResolvedMaskPage(t.Context(), undecidedSet.ID, 1,
		member.ID, undecidedDraft.ETag, 1, "", 1)
	require.NoError(t, err)
	rectangle := redaction.Decision{ID: "89000000-0000-4000-8000-000000000058", MemberID: member.ID,
		Action: "redact", Reason: "synthetic private reason", Selector: redaction.Selector{
			Kind: "rectangle", MapSHA256: member.MapSHA256,
			Boxes: []redaction.Box{{Page: 1, FrameSHA256: frame.Page.FrameSHA256,
				X0: 1000, Y0: 2000, X1: 5000, Y1: 6000}},
		}}
	expansionPath := "/api/v1/productions/sets/" + undecidedSet.ID + "/revisions/1/decisions/check"
	status, body = call(expansionPath, rectangle, undecidedDraft.ETag, true)
	require.Equal(t, http.StatusOK, status, string(body))
	require.NotContains(t, string(body), rectangle.Reason)
	var expansion struct {
		Outcome          string              `json:"outcome"`
		Expanded         *redaction.Selector `json:"expanded"`
		ExpandedBoxCount int                 `json:"expanded_box_count"`
	}
	require.NoError(t, json.Unmarshal(body, &expansion))
	require.Equal(t, "selection_expansion_required", expansion.Outcome)
	require.NotNil(t, expansion.Expanded)
	require.Equal(t, member.MapSHA256, expansion.Expanded.MapSHA256)
	require.NotEmpty(t, expansion.Expanded.Boxes)
	require.Positive(t, expansion.ExpandedBoxCount)
	fresh, err = vault.ProductionDraft(t.Context(), undecidedSet.ID, 1)
	require.NoError(t, err)
	require.Equal(t, undecidedDraft.ETag, fresh.ETag)
}

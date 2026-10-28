package api_test

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"uuid"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/redaction"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/daemonconn"
	"go.kenn.io/docbank/internal/processing"
	"go.kenn.io/docbank/internal/redactiontest"
	"go.kenn.io/docbank/internal/store"
)

//nolint:paralleltest // Qualified PDFium accounts for process-wide RSS.
func TestRedactionHTTP(t *testing.T) {
	runtime := redactiontest.PageRuntime(t)
	ts, s := newTestServer(t, func(d *api.Deps) {
		service, err := processing.NewService(processing.ServiceConfig{Catalog: d.Store, Blobs: d.Blobs, Gate: api.NewOperationGate(), SpoolDirectory: filepath.Join(d.VaultRoot, "blobs", "tmp")})
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, service.Shutdown(context.Background())) })
		d.Processing, d.PageRuntime = service, runtime
	})
	data := redactiontest.PDF(t, []string{"SECRET KEEP SECRET"}, "")
	hash, size, err := s.Blobs.Write(bytes.NewReader(data))
	require.NoError(t, err)
	node, err := s.CreateFile(t.Context(), s.RootID(), "native.pdf", hash, size, "application/pdf")
	require.NoError(t, err)
	selection := store.PageBinding{NodeID: node.ID, Revision: node.Revision, Source: document.PageSource{VersionID: node.CurrentVersionID, SHA256: hash, Size: size}}
	response, body := do(t, ts, http.MethodPost, "/api/v1/redaction/prepare", nil, processing.RedactionPreparationRequest{Selection: selection})
	require.Equal(t, 200, response.StatusCode, body)
	rawPreparation := jsontext.Value(body)
	require.Contains(t, body, `"$schema"`)
	var prep processing.RedactionPreparation
	require.NoError(t, json.Unmarshal([]byte(body), &prep))
	masks := []processing.RedactionMask{{ID: uuid.New(), Selector: redaction.Selector{Kind: "text", MapSHA256: prep.Map.SHA256, Span: &redaction.Span{End: 6}}}}
	request := map[string]any{"preparation": rawPreparation, "masks": masks}
	response, body = do(t, ts, http.MethodPost, "/api/v1/redaction/resolve", nil, request)
	require.Equal(t, 200, response.StatusCode, body)
	var review processing.RedactionReview
	require.NoError(t, json.Unmarshal([]byte(body), &review))
	require.NoError(t, processing.ValidateRedactionReview(prep, masks, review))
	ns, err := s.EnsureBatesNamespace(t.Context(), "HTTP", "", 6)
	require.NoError(t, err)
	reserve := map[string]any{"preparation": rawPreparation, "masks": masks, "review_sha256": review.SHA256,
		"namespace_id": ns.NamespaceID, "start_at": 1}
	for _, consent := range []any{nil, false} {
		reserve["retain_original"] = consent
		response, body = do(t, ts, http.MethodPost, "/api/v1/redaction/reservations", nil, reserve)
		require.Contains(t, []int{400, 422}, response.StatusCode, body)
	}
	delete(reserve, "retain_original")
	response, body = do(t, ts, http.MethodPost, "/api/v1/redaction/reservations", nil, reserve)
	require.Contains(t, []int{400, 422}, response.StatusCode, body)
	reserve["retain_original"] = true
	committed := loseNumberedReservationReply(t, ts, processing.NumberedReservationRequest{
		Preparation: prep, Masks: masks, ReviewSHA256: review.SHA256,
		NamespaceID: uuid.MustParse(ns.NamespaceID), StartAt: 1, RetainOriginal: true,
	})
	response, body = do(t, ts, http.MethodPost, "/api/v1/redaction/reservations", nil, reserve)
	require.Equal(t, 200, response.StatusCode, body)
	var reservation processing.NumberedReservation
	require.NoError(t, json.Unmarshal([]byte(body), &reservation))
	require.NoError(t, processing.ValidateNumberedReservation(reservation, &review))
	require.Equal(t, committed, reservation, "a lost reply must reuse the committed reservation")

	wrongMasks := []processing.RedactionMask{{ID: uuid.New(), Selector: redaction.Selector{Kind: "text", MapSHA256: prep.Map.SHA256, Span: &redaction.Span{End: 11}}}}
	wrongReview, err := processing.BuildRedactionReview(prep, wrongMasks, 1)
	require.NoError(t, err)
	response, body = do(t, ts, http.MethodPost, "/api/v1/redaction/render", nil, processing.RedactionRenderRequest{
		Preparation: prep, Masks: wrongMasks, ReviewSHA256: wrongReview.SHA256, AllocationID: reservation.AllocationID})
	require.Equal(t, 409, response.StatusCode, body)
	require.Contains(t, body, "redaction_review_changed")
	ordinary := batesTestRecipe(api.BatesNamespace{NamespaceID: ns.NamespaceID, Prefix: ns.Prefix, Padding: ns.Padding}, 2)
	ordinarySHA, err := ordinary.SHA256()
	require.NoError(t, err)
	ordinaryRequest, err := reservation.Recipe.PlanRequest(reservation.SnapshotID)
	require.NoError(t, err)
	ordinaryRequest.OperationID, ordinaryRequest.RecipeSHA256, ordinaryRequest.StartAt = uuid.New().String(), ordinarySHA, 2
	ordinaryAllocation, err := s.ReserveBatesRange(t.Context(), ordinaryRequest)
	require.NoError(t, err)
	response, body = do(t, ts, http.MethodPost, "/api/v1/redaction/render", nil, processing.RedactionRenderRequest{
		Preparation: prep, Masks: masks, ReviewSHA256: review.SHA256, AllocationID: uuid.MustParse(ordinaryAllocation.AllocationID)})
	require.Equal(t, 409, response.StatusCode, body)
	require.Contains(t, body, "redaction_review_changed")
	response, body = do(t, ts, http.MethodPost, "/api/v1/redaction/render", nil, processing.RedactionRenderRequest{
		Preparation: prep, Masks: masks, ReviewSHA256: review.SHA256, AllocationID: uuid.New()})
	require.Equal(t, 404, response.StatusCode, body)
	require.Contains(t, body, "allocation not found")
	response, body = do(t, ts, http.MethodPost, "/api/v1/redaction/reservations", nil, processing.NumberedReservationRequest{
		Preparation: prep, Masks: masks, ReviewSHA256: review.SHA256, NamespaceID: uuid.New(), StartAt: 1, RetainOriginal: true})
	require.Equal(t, 404, response.StatusCode, body)
	require.Contains(t, body, "namespace not found")
	response, body = do(t, ts, http.MethodPost, "/api/v1/redaction/render", nil, map[string]any{
		"preparation": rawPreparation, "masks": masks, "review_sha256": review.SHA256, "allocation_id": reservation.AllocationID})
	require.Equal(t, 200, response.StatusCode, body)
	render := processing.RedactionRenderRequest{Preparation: prep, Masks: masks, ReviewSHA256: review.SHA256}
	response, body = do(t, ts, http.MethodPost, "/api/v1/redaction/render", nil, map[string]any{
		"preparation": rawPreparation, "masks": masks, "review_sha256": review.SHA256,
	})
	require.Equal(t, 200, response.StatusCode, body)
	require.Equal(t, "application/zip", response.Header.Get("Content-Type"))
	require.Equal(t, review.SHA256, response.Header.Get("X-Redaction-Review-Sha256"))
	archive, err := zip.NewReader(strings.NewReader(body), int64(len(body)))
	require.NoError(t, err)
	require.Len(t, archive.File, 3)
	stream, err := archive.File[1].Open()
	require.NoError(t, err)
	text, err := io.ReadAll(stream)
	require.NoError(t, err)
	require.NoError(t, stream.Close())
	require.Equal(t, "[REDACTED] KEEP SECRET\f", string(text))
	box := prep.Map.Atoms[0].Boxes[0]
	box.X1 = box.X0 + (box.X1-box.X0)/2
	partial := processing.RedactionResolveRequest{Preparation: prep, Masks: []processing.RedactionMask{{ID: uuid.New(), Selector: redaction.Selector{Kind: "rectangle", MapSHA256: prep.Map.SHA256, Boxes: []redaction.Box{box}}}}}
	response, body = do(t, ts, http.MethodPost, "/api/v1/redaction/resolve", nil, partial)
	require.Equal(t, 422, response.StatusCode, body)
	var problem api.Error
	require.NoError(t, json.Unmarshal([]byte(body), &problem))
	require.Equal(t, "selection_expansion_required", problem.Code)
	require.NotNil(t, problem.Redaction)
	require.NotEmpty(t, problem.Redaction.ExpandedBoxes)
	require.NotNil(t, problem.Redaction.Expanded)
	partial.Masks[0].Selector = *problem.Redaction.Expanded
	response, body = do(t, ts, http.MethodPost, "/api/v1/redaction/resolve", nil, partial)
	require.Equal(t, 200, response.StatusCode, body)
	for _, path := range []string{"/api/v1/redaction/resolve", "/api/v1/redaction/render", "/api/v1/redaction/reservations"} {
		response, body = do(t, ts, http.MethodPost, path, map[string]string{"X-Api-Key": ""}, render)
		require.Equal(t, 401, response.StatusCode, body)
		response, body = do(t, ts, http.MethodPost, path, map[string]string{"X-Api-Key": "", api.WebSessionHeader: issueWebSession(t, ts)}, render)
		require.Equal(t, 403, response.StatusCode, body)
	}
	render.ReviewSHA256 = strings.Repeat("0", 64)
	response, body = do(t, ts, http.MethodPost, "/api/v1/redaction/render", nil, render)
	require.Equal(t, 409, response.StatusCode, body)
	require.Contains(t, body, "redaction_review_changed")
	for _, path := range []string{"/api/v1/redaction/resolve", "/api/v1/redaction/render"} {
		malformed := map[string]any{"preparation": prep, "masks": []any{map[string]any{"id": "12345678-1234-4234-8234-123456789ABC", "selector": masks[0].Selector}}}
		if strings.HasSuffix(path, "render") {
			malformed["review_sha256"] = review.SHA256
		}
		response, body = do(t, ts, http.MethodPost, path, nil, malformed)
		require.Contains(t, []int{400, 422}, response.StatusCode, body)
		malformed["masks"] = masks
		var unknownPreparation map[string]any
		require.NoError(t, json.Unmarshal(rawPreparation, &unknownPreparation))
		unknownPreparation["extra"] = true
		malformed["preparation"] = unknownPreparation
		response, body = do(t, ts, http.MethodPost, path, nil, malformed)
		require.Contains(t, []int{400, 422}, response.StatusCode, body)
	}
}

// Drop the downstream connection only after the real API has committed. This
// exercises the client's single attempt and the store-backed replay together.
func loseNumberedReservationReply(t *testing.T, daemon *httptest.Server,
	request processing.NumberedReservationRequest,
) processing.NumberedReservation {
	t.Helper()
	type committedReply struct {
		status int
		data   []byte
		err    error
	}
	replies := make(chan committedReply, 1)
	var calls atomic.Int32
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		response, err := daemon.Client().Post(daemon.URL+r.URL.Path, "application/json", r.Body)
		if err != nil {
			replies <- committedReply{err: err}
			return
		}
		data, err := io.ReadAll(response.Body)
		_ = response.Body.Close()
		select {
		case replies <- committedReply{status: response.StatusCode, data: data, err: err}:
		default:
		}
		connection, _, err := http.NewResponseController(w).Hijack()
		if err == nil {
			_ = connection.Close()
		}
	}))
	defer proxy.Close()
	client := daemonconn.New(proxy.URL, testAPIKey)
	defer func() { require.NoError(t, client.Close()) }()
	_, err := client.ReserveNumberedPDF(t.Context(), request)
	require.ErrorIs(t, err, daemonconn.ErrNumberedReservationUnknown)
	require.EqualValues(t, 1, calls.Load(), "reservation must not retry automatically")
	reply := <-replies
	require.NoError(t, reply.err)
	require.Equal(t, http.StatusOK, reply.status, string(reply.data))
	var reserved processing.NumberedReservation
	require.NoError(t, json.Unmarshal(reply.data, &reserved))
	return reserved
}

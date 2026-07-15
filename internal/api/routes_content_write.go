package api

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strconv"

	"github.com/danielgtaylor/huma/v2"

	"go.kenn.io/docbank/internal/ingest"
)

func registerContentWriteRoute(mux *http.ServeMux, api huma.API, d Deps, g *gate) {
	registerContentWriteOpenAPI(api)
	mux.HandleFunc("PUT /api/v1/nodes/{id}/content", func(w http.ResponseWriter, r *http.Request) {
		handleContentWrite(w, r, d, g)
	})
}

func registerContentWriteOpenAPI(api huma.API) {
	registry := api.OpenAPI().Components.Schemas
	receiptSchema := huma.SchemaFromType(registry, reflect.TypeFor[ContentReplacementReceipt]())
	errorSchema := huma.SchemaFromType(registry, reflect.TypeFor[Error]())
	api.OpenAPI().AddOperation(&huma.Operation{
		OperationID: "replaceNodeContent", Method: http.MethodPut,
		Path: "/api/v1/nodes/{id}/content", Summary: "Replace a file's content with a new immutable head",
		Description: "Streams raw content under an If-Match node revision. The daemon grants " +
			"metadata authority only after independently computing the declared SHA-256 and size.",
		Parameters: []*huma.Param{
			{Name: "id", In: "path", Required: true, Description: "Stable file node ID",
				Schema: &huma.Schema{Type: "integer", Format: "int64"}},
			{Name: "If-Match", In: "header", Required: true, Description: "Quoted or bare current node revision",
				Schema: &huma.Schema{Type: openAPIStringType}},
			{Name: BlobHashHeader, In: "header", Required: true,
				Description: "Expected lowercase hexadecimal SHA-256 of the request body",
				Schema:      &huma.Schema{Type: openAPIStringType, Pattern: "^[0-9a-f]{64}$"}},
			{Name: BlobSizeHeader, In: "header", Required: true,
				Description: "Expected raw request-body byte length",
				Schema:      &huma.Schema{Type: "integer", Format: "int64", Minimum: new(float64(0))}},
		},
		RequestBody: &huma.RequestBody{Required: true, Content: map[string]*huma.MediaType{
			"*/*": {Schema: &huma.Schema{Type: openAPIStringType, Format: "binary"}},
		}},
		Responses: map[string]*huma.Response{
			"200": {Description: "Replacement committed", Headers: map[string]*huma.Param{
				"ETag": {Description: "Quoted resulting node revision", Schema: &huma.Schema{Type: openAPIStringType}},
			}, Content: map[string]*huma.MediaType{"application/json": {Schema: receiptSchema}}},
			"default": {Description: "Error", Content: map[string]*huma.MediaType{
				"application/problem+json": {Schema: errorSchema},
			}},
		},
	})
}

func handleContentWrite(w http.ResponseWriter, r *http.Request, d Deps, g *gate) {
	nodeID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || nodeID <= 0 {
		writeError(w, NewError(http.StatusUnprocessableEntity, "validation",
			"node id must be a positive integer"))
		return
	}
	ifRev, matchErr := parseIfMatch(r.Header.Get("If-Match"))
	if matchErr != nil {
		apiErr := &Error{}
		ok := errors.As(matchErr, &apiErr)
		if !ok {
			apiErr = NewError(http.StatusInternalServerError, "internal", matchErr.Error())
		}
		writeError(w, apiErr)
		return
	}
	expectedHash, expectedSize, identityErr := parseExpectedBlobIdentity(r)
	if identityErr != nil {
		writeError(w, identityErr)
		return
	}
	if r.ContentLength >= 0 && r.ContentLength != expectedSize {
		writeError(w, NewError(http.StatusUnprocessableEntity, "size_mismatch", fmt.Sprintf(
			"declared %d bytes, HTTP body reports %d", expectedSize, r.ContentLength)))
		return
	}
	mimeType, mimeErr := uploadMediaType(r.Header.Get("Content-Type"))
	if mimeErr != nil {
		writeError(w, NewError(http.StatusUnprocessableEntity, "validation", mimeErr.Error()))
		return
	}

	var result ingest.ReplacementResult
	opErr := g.mutate(func() error {
		return d.Blobs.WithMutation(r.Context(), func() error {
			limited := &io.LimitedReader{R: r.Body, N: expectedSize + 1}
			ing := &ingest.Ingester{Store: d.Store, Blobs: d.Blobs}
			var replaceErr error
			result, replaceErr = ing.ReplaceContent(r.Context(), nodeID, ifRev, mimeType,
				limited, expectedHash, expectedSize)
			return replaceErr
		})
	})
	if opErr != nil {
		writeError(w, uploadError(opErr))
		return
	}
	w.Header().Set("ETag", fmt.Sprintf("%q", strconv.FormatInt(result.Node.Revision, 10)))
	writeJSON(w, http.StatusOK, ContentReplacementReceipt{
		Node: fromStoreNode(result.Node), Version: fromStoreContentVersion(result.Version),
		ComputedHash: result.ComputedHash, ComputedSize: result.ComputedSize,
	})
}

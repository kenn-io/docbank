package api

import (
	"errors"
	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/processing"
	"io"
	"mime"
	"net/http"
	"reflect"
	"strconv"
)

const emailDocumentJSONMediaType = "application/json"

func readEmailDocumentJSON(w http.ResponseWriter, r *http.Request, out any) bool {
	media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || media != emailDocumentJSONMediaType {
		writeError(w, NewError(http.StatusUnsupportedMediaType, "validation", "requires application/json"))
		return false
	}
	r.Body = http.MaxBytesReader(w, r.Body, document.EmailDocumentMaxJSONBytes)
	raw, readErr := io.ReadAll(r.Body)
	if readErr == nil {
		readErr = document.UnmarshalEmailDocumentJSON(raw, out)
	}
	if err = readErr; err != nil {
		status := http.StatusUnprocessableEntity
		code := "validation"
		if _, ok := errors.AsType[*http.MaxBytesError](err); ok {
			status = http.StatusRequestEntityTooLarge
			code = "too_large"
		}
		writeError(w, NewError(status, code, err.Error()))
		return false
	}
	return true
}

func registerEmailDocumentRoutes(mux *http.ServeMux, api huma.API, d Deps, g *gate) {
	registerProcessingConsentRoutes(mux, api, d, g)
	registerEmailDocumentRoute(mux, api, huma.Operation{
		OperationID: "publishEmailDocuments", Method: http.MethodPost,
		Path: "/api/v1/email-document-publications", Summary: "Publish exact email attachments as ordinary documents",
	}, reflect.TypeFor[document.EmailDocumentPublicationRequest](), reflect.TypeFor[document.EmailDocumentPublicationReceipt](), func(w http.ResponseWriter, r *http.Request) {
		var request document.EmailDocumentPublicationRequest
		if !readEmailDocumentJSON(w, r, &request) {
			return
		}
		var receipt document.EmailDocumentPublicationReceipt
		err := g.mutate(func() error {
			if d.PublishEmailDocuments == nil {
				return errors.New("email document publication is not configured")
			}
			var err error
			receipt, err = d.PublishEmailDocuments(r.Context(), d.Store, d.Blobs, request)
			return err
		})
		w.Header().Set("Cache-Control", "no-store")
		if err != nil {
			writeEmailStoreError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, receipt)
	})
	registerEmailDocumentRoute(mux, api, huma.Operation{
		OperationID: "getEmailDocumentPublication", Method: http.MethodGet,
		Path: "/api/v1/email-document-publications/{operation_id}", Summary: "Read an immutable publication receipt",
		Parameters: []*huma.Param{{Name: "operation_id", In: "path", Required: true, Schema: &huma.Schema{Type: "string"}}},
	}, nil, reflect.TypeFor[document.EmailDocumentPublicationReceipt](), func(w http.ResponseWriter, r *http.Request) {
		receipt, err := d.Store.EmailDocumentPublication(r.Context(), r.PathValue("operation_id"))
		w.Header().Set("Cache-Control", "no-store")
		if err != nil {
			writeEmailStoreError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, receipt)
	})
	type removeRequest struct {
		RequestDigest string `json:"request_digest"`
	}
	registerEmailDocumentRoute(mux, api, huma.Operation{
		OperationID: "removeEmailDocumentPublication", Method: http.MethodDelete,
		Path: "/api/v1/email-document-publications/{operation_id}", Summary: "Release one receipt without deleting children",
		Parameters: []*huma.Param{{Name: "operation_id", In: "path", Required: true, Schema: &huma.Schema{Type: "string"}}},
	}, reflect.TypeFor[removeRequest](), nil, func(w http.ResponseWriter, r *http.Request) {
		var request removeRequest
		if !readEmailDocumentJSON(w, r, &request) {
			return
		}
		err := g.mutate(func() error {
			return d.Store.RemoveEmailDocumentPublication(r.Context(), r.PathValue("operation_id"), request.RequestDigest)
		})
		if err != nil {
			writeEmailStoreError(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	registerEmailDocumentRoute(mux, api, huma.Operation{
		OperationID: "listEmailDocumentRelations", Method: http.MethodGet,
		Path: "/api/v1/email-document-relations", Summary: "Read a bounded page of exact parent or child occurrences",
		Parameters: []*huma.Param{
			{Name: "parent_version_id", In: "query", Schema: &huma.Schema{Type: "string"}},
			{Name: "child_version_id", In: "query", Schema: &huma.Schema{Type: "string"}},
			{Name: "after_operation_id", In: "query", Schema: &huma.Schema{Type: "string"}},
			{Name: "after_order", In: "query", Schema: &huma.Schema{Type: "integer"}},
			{Name: "limit", In: "query", Schema: &huma.Schema{Type: "integer"}},
		},
	}, nil, reflect.TypeFor[document.EmailDocumentRelationPage](), func(w http.ResponseWriter, r *http.Request) {
		values := r.URL.Query()
		q := document.EmailDocumentRelationQuery{ParentVersionID: values.Get("parent_version_id"), ChildVersionID: values.Get("child_version_id"), AfterOperationID: values.Get("after_operation_id")}
		for key, vals := range values {
			if len(vals) != 1 {
				writeError(w, NewError(422, "validation", "repeated relation query field"))
				return
			}
			switch key {
			case "parent_version_id", "child_version_id", "after_operation_id":
			case "limit", "after_order":
				n, err := strconv.Atoi(vals[0])
				if err != nil {
					writeError(w, NewError(422, "validation", "invalid relation page number"))
					return
				}
				if key == "limit" {
					q.Limit = n
				} else {
					q.AfterOrder = n
				}
			default:
				writeError(w, NewError(422, "validation", "unknown relation query field"))
				return
			}
		}
		page, err := d.Store.EmailDocumentRelations(r.Context(), q)
		w.Header().Set("Cache-Control", "no-store")
		if err != nil {
			writeEmailStoreError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, page)
	})
	registerEmailDocumentRoute(mux, api, huma.Operation{
		OperationID: "requestEmailDocumentProcessing", Method: http.MethodPost,
		Path: "/api/v1/email-document-processing", Summary: "Request ordinary consent-aware attachment processing",
	}, reflect.TypeFor[document.EmailDocumentProcessingRequest](), reflect.TypeFor[document.EmailDocumentProcessingReceipt](), func(w http.ResponseWriter, r *http.Request) {
		var request document.EmailDocumentProcessingRequest
		if !readEmailDocumentJSON(w, r, &request) {
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		if d.Processing == nil {
			writeEmailStoreError(w, processingUnavailable())
			return
		}
		receipt, err := d.Processing.EnqueueEmailDocumentProcessing(r.Context(), request)
		if err != nil {
			if errors.Is(err, processing.ErrProfileNotConfigured) {
				err = fromProcessingError(err)
			}
			writeEmailStoreError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, receipt)
	})
}

// Register the raw handler and its schema together: these operations enforce
// the email JSON token budgets before allocating typed request collections.
func registerEmailDocumentRoute(mux *http.ServeMux, api huma.API, op huma.Operation, input, output reflect.Type, handler http.HandlerFunc) {
	registry := api.OpenAPI().Components.Schemas
	if input != nil {
		op.RequestBody = &huma.RequestBody{Required: true, Content: map[string]*huma.MediaType{
			emailDocumentJSONMediaType: {Schema: huma.SchemaFromType(registry, input)},
		}}
	}
	op.Responses = map[string]*huma.Response{
		"default": {Description: "Request failed", Content: map[string]*huma.MediaType{
			emailDocumentJSONMediaType: {Schema: huma.SchemaFromType(registry, reflect.TypeFor[Error]())},
		}},
	}
	if output != nil {
		op.Responses["200"] = &huma.Response{Description: "Exact retained authority", Content: map[string]*huma.MediaType{
			emailDocumentJSONMediaType: {Schema: huma.SchemaFromType(registry, output)},
		}}
	} else {
		op.Responses["204"] = &huma.Response{Description: "Receipt removed"}
	}
	mux.HandleFunc(op.Method+" "+op.Path, handler)
	api.OpenAPI().AddOperation(&op)
}

package api

import (
	"errors"
	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/docbank/document"
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
	registerProcessingConsentRoutes(mux, d, g)
	mux.HandleFunc("POST /api/v1/email-document-publications", func(w http.ResponseWriter, r *http.Request) {
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
	mux.HandleFunc("GET /api/v1/email-document-publications/{operation_id}", func(w http.ResponseWriter, r *http.Request) {
		receipt, err := d.Store.EmailDocumentPublication(r.Context(), r.PathValue("operation_id"))
		w.Header().Set("Cache-Control", "no-store")
		if err != nil {
			writeEmailStoreError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, receipt)
	})
	mux.HandleFunc("DELETE /api/v1/email-document-publications/{operation_id}", func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			RequestDigest string `json:"request_digest"`
		}
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
	mux.HandleFunc("GET /api/v1/email-document-relations", func(w http.ResponseWriter, r *http.Request) {
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
	mux.HandleFunc("POST /api/v1/email-document-processing", func(w http.ResponseWriter, r *http.Request) {
		var request document.EmailDocumentProcessingRequest
		if !readEmailDocumentJSON(w, r, &request) {
			return
		}
		var receipt document.EmailDocumentProcessingReceipt
		err := g.mutate(func() error {
			var err error
			receipt, err = d.Store.RequestEmailDocumentProcessing(r.Context(), request)
			return err
		})
		w.Header().Set("Cache-Control", "no-store")
		if err != nil {
			writeEmailStoreError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, receipt)
	})
	registerEmailDocumentOpenAPI(api)
}
func registerEmailDocumentOpenAPI(api huma.API) {
	registry := api.OpenAPI().Components.Schemas
	operations := []struct {
		id, method, path, summary string
		input, output             reflect.Type
	}{
		{"grantProcessingConsent", "POST", "/api/v1/processing/consents", "Explicitly grant existing processing consent", reflect.TypeFor[document.ProcessingConsentRequest](), reflect.TypeFor[document.ProcessingConsentReceipt]()},
		{"revokeProcessingConsent", "POST", "/api/v1/processing/consents/revoke", "Revoke a principal and scope before further provider access", reflect.TypeFor[document.ProcessingConsentRevocationRequest](), reflect.TypeFor[document.ProcessingConsentRevocationReceipt]()},
		{"publishEmailDocuments", "POST", "/api/v1/email-document-publications", "Publish exact email attachments as ordinary documents", reflect.TypeFor[document.EmailDocumentPublicationRequest](), reflect.TypeFor[document.EmailDocumentPublicationReceipt]()},
		{"getEmailDocumentPublication", "GET", "/api/v1/email-document-publications/{operation_id}", "Read an immutable publication receipt", nil, reflect.TypeFor[document.EmailDocumentPublicationReceipt]()},
		{"removeEmailDocumentPublication", "DELETE", "/api/v1/email-document-publications/{operation_id}", "Release one receipt without deleting children", reflect.TypeFor[struct {
			RequestDigest string `json:"request_digest"`
		}](), nil},
		{"listEmailDocumentRelations", "GET", "/api/v1/email-document-relations", "Read a bounded page of exact parent or child occurrences", nil, reflect.TypeFor[document.EmailDocumentRelationPage]()},
		{"requestEmailDocumentProcessing", "POST", "/api/v1/email-document-processing", "Request ordinary consent-aware attachment processing", reflect.TypeFor[document.EmailDocumentProcessingRequest](), reflect.TypeFor[document.EmailDocumentProcessingReceipt]()},
	}
	for _, v := range operations {
		op := &huma.Operation{OperationID: v.id, Method: v.method, Path: v.path, Summary: v.summary, Responses: map[string]*huma.Response{}}
		if v.input != nil {
			op.RequestBody = &huma.RequestBody{Required: true, Content: map[string]*huma.MediaType{emailDocumentJSONMediaType: {Schema: huma.SchemaFromType(registry, v.input)}}}
		}
		if v.output != nil {
			op.Responses["200"] = &huma.Response{Description: "Exact retained authority", Content: map[string]*huma.MediaType{emailDocumentJSONMediaType: {Schema: huma.SchemaFromType(registry, v.output)}}}
		} else {
			op.Responses["204"] = &huma.Response{Description: "Receipt removed"}
		}
		op.Responses["default"] = &huma.Response{Description: "Request failed", Content: map[string]*huma.MediaType{emailDocumentJSONMediaType: {Schema: huma.SchemaFromType(registry, reflect.TypeFor[Error]())}}}
		if v.id == "getEmailDocumentPublication" || v.id == "removeEmailDocumentPublication" {
			op.Parameters = []*huma.Param{{Name: "operation_id", In: "path", Required: true, Schema: &huma.Schema{Type: "string"}}}
		}
		if v.id == "listEmailDocumentRelations" {
			for _, key := range []string{"parent_version_id", "child_version_id", "after_operation_id", "after_order", "limit"} {
				kind := "string"
				if key == "limit" || key == "after_order" {
					kind = "integer"
				}
				op.Parameters = append(op.Parameters, &huma.Param{Name: key, In: "query", Schema: &huma.Schema{Type: kind}})
			}
		}
		api.OpenAPI().AddOperation(op)
	}
}

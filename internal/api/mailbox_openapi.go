package api

import (
	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/docbank/internal/mailbox"
	"go.kenn.io/docbank/internal/store"
	"net/http"
	"reflect"
	"strings"
)

const mailboxHeaderLocation = "header"

func registerMailboxOpenAPI(api huma.API) {
	registry := api.OpenAPI().Components.Schemas
	type previewRequest struct {
		Dialect string `json:"dialect"`
	}
	type resumeRequest struct {
		Request      store.MailboxJobRequest `json:"request"`
		Continuation bool                    `json:"continuation"`
	}
	for _, r := range []struct {
		method, path, id, summary, status string
		input, output                     reflect.Type
	}{
		{http.MethodPost, "/containers", "beginMailboxContainer", "Declare an immutable mailbox container", "201", reflect.TypeFor[store.MailboxContainerRequest](), reflect.TypeFor[store.MailboxContainer]()},
		{http.MethodGet, "/containers/{id}", "getMailboxContainer", "Read owned source and verified chunks", "200", nil, reflect.TypeFor[store.MailboxContainer]()},
		{http.MethodPut, "/containers/{id}/chunks/{index}", "uploadMailboxChunk", "Verify and retain one declared chunk", "200", nil, reflect.TypeFor[store.MailboxChunk]()},
		{http.MethodPost, "/containers/{id}/seal", "sealMailboxContainer", "Verify full source and seal ordered chunk authority", "200", nil, reflect.TypeFor[store.MailboxContainer]()},
		{http.MethodDelete, "/containers/{id}", "abortMailboxContainer", "Abort an incomplete upload", "204", nil, nil},
		{http.MethodPost, "/containers/{id}/preview", "previewMailboxContainer", "Preflight archive and preview explicit dialect", "200", reflect.TypeFor[previewRequest](), reflect.TypeFor[mailbox.Preview]()},
		{http.MethodPost, "/jobs", "beginMailboxJob", "Start an immutable resumable mailbox import", "201", reflect.TypeFor[store.MailboxJobRequest](), reflect.TypeFor[store.MailboxJob]()},
		{http.MethodGet, "/jobs", "listMailboxJobs", "Read a bounded page of owned imports", "200", nil, reflect.TypeFor[[]store.MailboxJob]()},
		{http.MethodGet, "/jobs/{id}", "getMailboxJob", "Read durable progress and remaining tail", "200", nil, reflect.TypeFor[store.MailboxJob]()},
		{http.MethodGet, "/jobs/{id}/occurrences", "mailboxOccurrences", "Read a bounded occurrence receipt page", "200", nil, reflect.TypeFor[[]store.MailboxOccurrence]()},
		{http.MethodGet, "/jobs/{id}/events", "mailboxEvents", "Observe durable import progress as NDJSON", "200", nil, reflect.TypeFor[MailboxEvent]()},
		{http.MethodPost, "/jobs/{id}/cancel", "cancelMailboxJob", "Fence publication and cancel pending work", "204", nil, nil},
		{http.MethodPost, "/jobs/{id}/resume", "resumeMailboxJob", "Resume or explicitly continue the exact source and settings", "200", reflect.TypeFor[resumeRequest](), reflect.TypeFor[store.MailboxJob]()},
		{http.MethodPost, "/archives", "registerMailboxArchive", "Register an application-independent EML archive", "201", reflect.TypeFor[store.MailboxArchive](), reflect.TypeFor[store.MailboxArchive]()},
		{http.MethodPost, "/transfers", "transferMailboxEML", "Atomically publish an explicitly identified EML and retry receipt", "200", nil, reflect.TypeFor[store.MailboxTransferReceipt]()},
	} {
		op := &huma.Operation{OperationID: r.id, Method: r.method, Path: "/api/v1/mailbox" + r.path, Summary: r.summary, Responses: map[string]*huma.Response{r.status: {Description: r.summary}, "default": {Description: "Mailbox request failed", Content: map[string]*huma.MediaType{emailDocumentJSONMediaType: {Schema: huma.SchemaFromType(registry, reflect.TypeFor[Error]())}}}}}
		if r.output != nil {
			media := emailDocumentJSONMediaType
			if r.id == "mailboxEvents" {
				media = "application/x-ndjson"
			}
			op.Responses[r.status].Content = map[string]*huma.MediaType{media: {Schema: huma.SchemaFromType(registry, r.output)}}
		}
		if r.input != nil {
			op.RequestBody = &huma.RequestBody{Required: true, Description: "JSON body, at most 1 MiB. Owner is derived from authenticated identity.", Content: map[string]*huma.MediaType{emailDocumentJSONMediaType: {Schema: huma.SchemaFromType(registry, r.input)}}}
		}
		if strings.Contains(r.path, "{id}") {
			op.Parameters = append(op.Parameters, &huma.Param{Name: "id", In: "path", Required: true, Schema: &huma.Schema{Type: "string"}})
		}
		if r.id == "uploadMailboxChunk" {
			op.Parameters = append(op.Parameters, &huma.Param{Name: "index", In: "path", Required: true, Schema: &huma.Schema{Type: "integer"}}, &huma.Param{Name: BlobHashHeader, In: mailboxHeaderLocation, Required: true, Schema: &huma.Schema{Type: "string", Pattern: "^[0-9a-f]{64}$"}}, &huma.Param{Name: BlobSizeHeader, In: mailboxHeaderLocation, Required: true, Schema: &huma.Schema{Type: "integer"}})
			op.RequestBody = &huma.RequestBody{Required: true, Description: "One complete 64 MiB chunk, except the exact shorter final chunk.", Content: map[string]*huma.MediaType{"application/octet-stream": {Schema: &huma.Schema{Type: "string", Format: "binary"}}}}
		}
		if r.id == "transferMailboxEML" {
			op.Parameters = append(op.Parameters, &huma.Param{Name: "X-Docbank-Transfer", In: mailboxHeaderLocation, Required: true, Description: "Base64url JSON MailboxTransferRequest (at most 32768 encoded bytes)", Schema: &huma.Schema{Type: "string"}})
			op.RequestBody = &huma.RequestBody{Required: true, Description: "Exact declared EML bytes, at most 128 MiB.", Content: map[string]*huma.MediaType{"message/rfc822": {Schema: &huma.Schema{Type: "string", Format: "binary"}}}}
			huma.SchemaFromType(registry, reflect.TypeFor[store.MailboxTransferRequest]())
		}
		if r.id == "listMailboxJobs" || r.id == "mailboxOccurrences" {
			kind := "string"
			if r.id == "mailboxOccurrences" {
				kind = "integer"
			}
			op.Parameters = append(op.Parameters, &huma.Param{Name: "after", In: "query", Schema: &huma.Schema{Type: kind}}, &huma.Param{Name: "limit", In: "query", Description: "Page size, 1 through 100", Schema: &huma.Schema{Type: "integer"}})
		}
		api.OpenAPI().AddOperation(op)
	}
}

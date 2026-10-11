package api

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"reflect"

	"github.com/danielgtaylor/huma/v2"

	"go.kenn.io/docbank/internal/ingest"
	"go.kenn.io/docbank/internal/store"
)

const pushUploadPath = "/api/v1/push/uploads"

// PushSourceState is the portable source cursor, which may differ from Node's
// current head after an independent edit. An unknown source has Known=false.
type PushSourceState struct {
	Known bool   `json:"known"`
	Node  *Node  `json:"node,omitzero"`
	Hash  string `json:"hash,omitzero" pattern:"^[0-9a-f]{64}$"`
	Size  int64  `json:"size" minimum:"0"`
}

// PushUploadReceipt proves which bytes the daemon computed for one push
// source and which node now carries that source.
type PushUploadReceipt struct {
	Status       string `json:"status" enum:"added,updated,linked,skipped,duplicate_skipped"`
	Node         Node   `json:"node"`
	ComputedHash string `json:"computed_hash" pattern:"^[0-9a-f]{64}$"`
	ComputedSize int64  `json:"computed_size"`
}

func registerPushRoutes(mux *http.ServeMux, api huma.API, d Deps, g *gate) {
	type input struct {
		Name string `query:"push_name" required:"true" maxLength:"64"`
		Ref  string `query:"source_ref" required:"true" maxLength:"4096"`
	}
	type output struct{ Body PushSourceState }
	huma.Register(api, huma.Operation{
		OperationID: "getPushSource", Method: http.MethodGet, Path: "/api/v1/push/source",
		Summary: "Read the last accepted bytes for one push source",
		Description: "Uses push name plus relative source path. Unknown identities return known=false; mapped trash is an error. " +
			"The last accepted source hash is independent of the node's current version. " +
			"A push name that matches a configured daemon watch is refused with push_name_in_use.",
	}, func(ctx context.Context, in *input) (*output, error) {
		source := store.PushSource{Name: in.Name, Ref: in.Ref, Duplicates: "link"}
		if apiErr := checkPushSource(d, source); apiErr != nil {
			return nil, apiErr
		}
		state, err := d.Store.PushSourceState(ctx, source)
		if errors.Is(err, store.ErrNotFound) {
			return &output{}, nil
		}
		if err != nil {
			return nil, FromStoreError(err)
		}
		node := fromStoreNode(state.Node)
		return &output{Body: PushSourceState{Known: true, Node: &node, Hash: state.Hash, Size: state.Size}}, nil
	})
	registerPushUploadOpenAPI(api)
	mux.HandleFunc("POST "+pushUploadPath, func(w http.ResponseWriter, r *http.Request) {
		handlePushUpload(w, r, d, g)
	})
}

// checkPushSource refuses a push name while a daemon watch of that name is
// configured: both producers would version the same source identities.
func checkPushSource(d Deps, source store.PushSource) *Error {
	if err := store.ValidatePushSource(source); err != nil {
		return NewError(http.StatusUnprocessableEntity, "validation", err.Error())
	}
	for _, watch := range d.Cfg.Watches {
		if watch.Name == source.Name {
			return NewError(http.StatusConflict, "push_name_in_use", fmt.Sprintf(
				"daemon watch %q is still configured; remove it from the daemon configuration "+
					"and restart the daemon before pushing with its name", source.Name))
		}
	}
	return nil
}

func handlePushUpload(w http.ResponseWriter, r *http.Request, d Deps, g *gate) {
	query := r.URL.Query()
	name, err := store.NormalizeName(query.Get("name"))
	if err != nil {
		writeError(w, NewError(http.StatusUnprocessableEntity, "invalid_name", err.Error()))
		return
	}
	parentPath := query.Get("parent_path")
	if err := store.ValidatePushDestination(parentPath); err != nil {
		writeError(w, NewError(http.StatusUnprocessableEntity, "validation", err.Error()))
		return
	}
	expectedHash, expectedSize, identityErr := parseExpectedBlobIdentity(r)
	if identityErr != nil {
		writeError(w, identityErr)
		return
	}
	source := store.PushSource{
		Name: query.Get("push_name"), Ref: query.Get("source_ref"),
		ModifiedAt: query.Get("modified_at"), Duplicates: query.Get("duplicates"),
	}
	if apiErr := checkPushSource(d, source); apiErr != nil {
		writeError(w, apiErr)
		return
	}
	content := store.PushContent{ParentPath: parentPath, Name: name, Hash: expectedHash, Size: expectedSize}
	var result ingest.UploadResult
	var outcome string
	received := receiveUpload(w, r, d, g, uploadPayload{
		name: name, expectedSize: expectedSize,
		prepare: func(ing *ingest.Ingester, mimeType string, body io.Reader) (*ingest.PreparedUpload, error) {
			content.MIMEType = mimeType
			return ing.PreparePushUpload(r.Context(), content, body)
		},
		commit: func(prepared *ingest.PreparedUpload) error {
			var commitErr error
			result, outcome, commitErr = prepared.CommitPush(r.Context(), source)
			return commitErr
		},
	})
	if !received {
		return
	}
	httpStatus := http.StatusOK
	if outcome == "added" {
		httpStatus = http.StatusCreated
	}
	writeJSON(w, httpStatus, PushUploadReceipt{
		Status: outcome, Node: fromStoreNode(result.Node),
		ComputedHash: result.ComputedHash, ComputedSize: result.ComputedSize,
	})
}

// registerPushUploadOpenAPI documents the raw multipart push route. It shares
// the ordinary upload's payload and digest headers, but places new nodes by
// virtual path because an existing source keeps its node wherever it now is.
func registerPushUploadOpenAPI(api huma.API) {
	upload := api.OpenAPI().Paths["/api/v1/uploads"].Post
	registry := api.OpenAPI().Components.Schemas
	receiptSchema := registry.Schema(reflect.TypeFor[PushUploadReceipt](), true, "")
	response := func(description string) *huma.Response {
		return &huma.Response{Description: description, Content: map[string]*huma.MediaType{
			jsonMediaType: {Schema: receiptSchema},
		}}
	}
	parameters := []*huma.Param{}
	for _, param := range upload.Parameters {
		if param.Name != "parent_id" {
			parameters = append(parameters, param)
		}
	}
	for _, field := range []struct {
		name, description string
		required          bool
		max               int
	}{
		{"parent_path", "Absolute virtual directory for a new source; missing directories are created with its node", true, 4096},
		{"push_name", "Portable push identity; lowercase letters, digits, -, _, .", true, 64},
		{"source_ref", "Canonical relative slash path", true, 4096},
		{"duplicates", "New-identity duplicate policy: link, skip, or create", true, 6},
		{"modified_at", "Original source modification time in canonical UTC RFC3339Nano", false, 64},
	} {
		parameters = append(parameters, &huma.Param{
			Name: field.name, In: openAPIQueryLocation, Required: field.required, Description: field.description,
			Schema: &huma.Schema{Type: openAPIStringType, MaxLength: new(field.max)},
		})
	}
	responses := maps.Clone(upload.Responses)
	responses["200"] = response("Existing source updated, linked, skipped, or duplicate-skipped")
	responses["201"] = response("New file node created")
	api.OpenAPI().AddOperation(&huma.Operation{
		OperationID: "uploadPushFile", Method: http.MethodPost, Path: pushUploadPath,
		Summary: "Upload one digest-checked push source",
		Description: upload.Description + " Records push provenance atomically. Existing source identities keep their node " +
			"and version changed source bytes; parent_path and name place only new nodes. Duplicate policy applies " +
			"only to new identities and only links to nodes this push name already owns.",
		Parameters:  parameters,
		RequestBody: upload.RequestBody,
		Responses:   responses,
	})
}

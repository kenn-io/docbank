package api

import (
	"context"
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

// PushSourceCursor is the last accepted bytes for one push source. It can
// differ from the node's current head after an independent edit.
type PushSourceCursor struct {
	SourceRef string `json:"source_ref"`
	NodeID    int64  `json:"node_id" minimum:"1"`
	Hash      string `json:"hash" pattern:"^[0-9a-f]{64}$"`
	Size      int64  `json:"size" minimum:"0"`
	Trashed   bool   `json:"trashed"`
}

// PushSourcePage is one page of a push name's cursors in source_ref byte
// order. Next is the after value for the following page; it is empty on the
// last page.
type PushSourcePage struct {
	Items []PushSourceCursor `json:"items"`
	Next  string             `json:"next,omitzero"`
}

// PushUploadReceipt proves which bytes the daemon computed for one push
// source and which node now carries that source.
type PushUploadReceipt struct {
	Status       string `json:"status" enum:"added,updated,skipped"`
	Node         Node   `json:"node"`
	ComputedHash string `json:"computed_hash" pattern:"^[0-9a-f]{64}$"`
	ComputedSize int64  `json:"computed_size"`
}

func registerPushRoutes(mux *http.ServeMux, api huma.API, d Deps, g *gate) {
	type input struct {
		Name  string `query:"push_name" required:"true" maxLength:"64"`
		After string `query:"after" maxLength:"4096"`
		Limit int    `query:"limit" minimum:"1" maximum:"1000" default:"1000"`
	}
	type output struct{ Body PushSourcePage }
	huma.Register(api, huma.Operation{
		OperationID: "listPushSources", Method: http.MethodGet, Path: "/api/v1/push/sources",
		Summary: "List the last accepted bytes of a push name's sources",
		Description: "Pages through one push name's cursors in source_ref byte order. Before a push accepts a source, " +
			"the cursor of a daemon watch with the same name and source_ref stands in for it. A cursor can differ from " +
			"its node's current version. A push name that matches a configured daemon watch is refused with push_name_in_use.",
	}, func(ctx context.Context, in *input) (*output, error) {
		if apiErr := checkPushName(d, in.Name); apiErr != nil {
			return nil, apiErr
		}
		cursors, err := d.Store.PushCursors(ctx, in.Name, in.After, in.Limit)
		if err != nil {
			return nil, FromStoreError(err)
		}
		page := PushSourcePage{Items: make([]PushSourceCursor, 0, len(cursors))}
		for _, cursor := range cursors {
			page.Items = append(page.Items, PushSourceCursor{
				SourceRef: cursor.Ref, NodeID: cursor.NodeID, Hash: cursor.Hash, Size: cursor.Size, Trashed: cursor.Trashed,
			})
		}
		if len(cursors) == in.Limit {
			page.Next = cursors[len(cursors)-1].Ref
		}
		return &output{Body: page}, nil
	})
	registerPushUploadOpenAPI(api)
	mux.HandleFunc("POST "+pushUploadPath, func(w http.ResponseWriter, r *http.Request) {
		handlePushUpload(w, r, d, g)
	})
}

// checkPushName refuses a push name while a daemon watch of that name is
// configured: both producers would version the same source identities.
func checkPushName(d Deps, name string) *Error {
	if err := store.ValidatePushName(name); err != nil {
		return NewError(http.StatusUnprocessableEntity, "validation", err.Error())
	}
	for _, watch := range d.Cfg.Watches {
		if watch.Name == name {
			return NewError(http.StatusConflict, "push_name_in_use", fmt.Sprintf(
				"daemon watch %q is still configured; remove it from the daemon configuration "+
					"and restart the daemon before pushing with its name", name))
		}
	}
	return nil
}

func checkPushSource(d Deps, source store.PushSource) *Error {
	if err := store.ValidatePushSource(source); err != nil {
		return NewError(http.StatusUnprocessableEntity, "validation", err.Error())
	}
	return checkPushName(d, source.Name)
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
		Name: query.Get("push_name"), Ref: query.Get("source_ref"), ModifiedAt: query.Get("modified_at"),
	}
	if apiErr := checkPushSource(d, source); apiErr != nil {
		writeError(w, apiErr)
		return
	}
	runID := query.Get("run_id")
	if err := store.ValidatePushRunID(runID); err != nil {
		writeError(w, NewError(http.StatusUnprocessableEntity, "validation", err.Error()))
		return
	}
	content := store.PushContent{
		RunID: runID, ParentPath: parentPath, Name: name, Hash: expectedHash, Size: expectedSize,
	}
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
		// One interactive push run reports its first addition; updates are not additions.
		if surface := cliImportSurface(r.Context(), r.Header.Get(ActionSurfaceHeader)); surface != "" {
			d.TelemetryCapture.TrackImport("push:"+runID, surface)
			d.TelemetryCapture.ImportAdded("push:" + runID)
		}
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
		{"modified_at", "Original source modification time in canonical UTC RFC3339Nano", false, 64},
	} {
		parameters = append(parameters, &huma.Param{
			Name: field.name, In: openAPIQueryLocation, Required: field.required, Description: field.description,
			Schema: &huma.Schema{Type: openAPIStringType, MaxLength: new(field.max)},
		})
	}
	responses := maps.Clone(upload.Responses)
	responses["200"] = response("Existing source updated or skipped")
	responses["201"] = response("New file node created")
	parameters = append(parameters, &huma.Param{
		Name: "run_id", In: openAPIQueryLocation, Required: true,
		Description: "UUIDv4 naming the client's scan; every file one run accepts joins one ingest run",
		Schema:      &huma.Schema{Type: openAPIStringType, Format: "uuid"},
	})
	api.OpenAPI().AddOperation(&huma.Operation{
		OperationID: "uploadPushFile", Method: http.MethodPost, Path: pushUploadPath,
		Summary: "Upload one digest-checked push source",
		Description: upload.Description + " Records push provenance atomically. Existing source identities keep their node " +
			"and version changed source bytes; parent_path and name place only new nodes. A new identity always gets " +
			"its own node.",
		Parameters:  parameters,
		RequestBody: upload.RequestBody,
		Responses:   responses,
	})
}

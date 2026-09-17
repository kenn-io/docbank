package api

import (
	"net/http"
	"reflect"

	"github.com/danielgtaylor/huma/v2"
)

const jsonMediaType = "application/json"

// These handlers use net/http directly. Document them alongside the Huma
// operations so browser and daemon lifecycle requests can also be generated.
func registerDaemonOpenAPI(api huma.API) {
	registry := api.OpenAPI().Components.Schemas
	jsonResponse := func(description string, typ reflect.Type) *huma.Response {
		return &huma.Response{Description: description, Content: map[string]*huma.MediaType{
			jsonMediaType: {Schema: huma.SchemaFromType(registry, typ)},
		}}
	}
	api.OpenAPI().AddOperation(&huma.Operation{
		OperationID: "createWebSession", Method: http.MethodPost, Path: webSessionPath,
		Summary: "Issue a scoped browser session",
		Responses: map[string]*huma.Response{"201": jsonResponse("Browser session", reflect.TypeFor[struct {
			Token        string `json:"token"`
			UploadSecret string `json:"upload_secret"`
			URL          string `json:"url"`
		}]())},
	})
	api.OpenAPI().AddOperation(&huma.Operation{
		OperationID: "revokeWebSession", Method: http.MethodDelete, Path: webSessionPath,
		Summary:   "Revoke the current browser session",
		Responses: map[string]*huma.Response{"204": {Description: "Session revoked"}},
	})
	api.OpenAPI().AddOperation(&huma.Operation{
		BodyReadTimeout: -1,
		OperationID:     "prepareWebDownload", Method: http.MethodPost, Path: webDownloadPreparePath,
		Summary: "Verify a document and prepare a browser download",
		RequestBody: &huma.RequestBody{Required: true, Content: map[string]*huma.MediaType{
			jsonMediaType: {Schema: huma.SchemaFromType(registry, reflect.TypeFor[webDownloadRequest]())},
		}},
		Responses: map[string]*huma.Response{"200": {
			Description: "Download progress and ticket", Content: map[string]*huma.MediaType{
				"application/x-ndjson": {Schema: huma.SchemaFromType(registry, reflect.TypeFor[webDownloadEvent]())},
			},
		}},
	})
	api.OpenAPI().AddOperation(&huma.Operation{
		OperationID: "cancelWebDownload", Method: http.MethodDelete, Path: webDownloadPreparePath,
		Summary: "Discard a prepared browser download",
		Parameters: []*huma.Param{{Name: "ticket", In: "query", Required: true,
			Schema: &huma.Schema{Type: openAPIStringType}}},
		Responses: map[string]*huma.Response{"204": {Description: "Download discarded"}},
	})
}

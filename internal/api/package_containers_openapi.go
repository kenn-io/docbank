package api

import (
	"net/http"
	"reflect"
	"strings"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/docbank/internal/store"
)

func registerPackageContainerOpenAPI(api huma.API) {
	const defaultResponse = "default"
	registry := api.OpenAPI().Components.Schemas
	for _, spec := range []struct {
		method, path, id, summary, status string
		input, output                     reflect.Type
	}{
		{http.MethodPost, "/containers", "beginPackageContainer", "Declare an immutable ZIP package", "201", reflect.TypeFor[packageContainerInput](), reflect.TypeFor[PackageContainer]()},
		{http.MethodGet, "/containers/{id}", "getPackageContainer", "Read an owned ZIP package", "200", nil, reflect.TypeFor[PackageContainer]()},
		{http.MethodPut, "/containers/{id}/chunks/{index}", "uploadPackageChunk", "Verify and retain one declared ZIP chunk", "200", nil, reflect.TypeFor[store.MailboxChunk]()},
		{http.MethodPost, "/containers/{id}/seal", "sealPackageContainer", "Verify the full ZIP digest and seal", "200", nil, reflect.TypeFor[PackageContainer]()},
		{http.MethodDelete, "/containers/{id}", "abortPackageContainer", "Abort an incomplete ZIP upload", "204", nil, nil},
	} {
		op := &huma.Operation{OperationID: spec.id, Method: spec.method, Path: "/api/v1/packages" + spec.path,
			Summary: spec.summary, Responses: map[string]*huma.Response{
				spec.status: {Description: spec.summary},
				defaultResponse: {Description: "Package container request failed", Content: map[string]*huma.MediaType{
					"application/problem+json": {Schema: registry.Schema(reflect.TypeFor[Error](), true, "")},
				}},
			}}
		if spec.output != nil {
			op.Responses[spec.status].Content = map[string]*huma.MediaType{jsonMediaType: {Schema: registry.Schema(spec.output, true, "")}}
		}
		if spec.input != nil {
			op.RequestBody = &huma.RequestBody{Required: true, Description: "ZIP digest, exact size, and caller-supplied container ID; at most 1 MiB of JSON", Content: map[string]*huma.MediaType{
				jsonMediaType: {Schema: registry.Schema(spec.input, true, "")},
			}}
		}
		if strings.Contains(spec.path, "{id}") {
			op.Parameters = append(op.Parameters, &huma.Param{Name: "id", In: openAPIPathLocation, Required: true, Schema: &huma.Schema{Type: openAPIStringType}})
		}
		if spec.id == "uploadPackageChunk" {
			op.Parameters = append(op.Parameters,
				&huma.Param{Name: "index", In: openAPIPathLocation, Required: true, Schema: &huma.Schema{Type: mailboxIntegerType}},
				&huma.Param{Name: BlobHashHeader, In: mailboxHeaderLocation, Required: true, Schema: &huma.Schema{Type: openAPIStringType, Pattern: "^[0-9a-f]{64}$"}},
				&huma.Param{Name: BlobSizeHeader, In: mailboxHeaderLocation, Required: true, Schema: &huma.Schema{Type: mailboxIntegerType, Format: "int64"}},
			)
			op.RequestBody = &huma.RequestBody{Required: true, Description: "One complete 64 MiB chunk, except the exact shorter final chunk; API-key clients only", Content: map[string]*huma.MediaType{
				"application/octet-stream": {Schema: &huma.Schema{Type: openAPIStringType, Format: "binary"}},
			}}
		}
		api.OpenAPI().AddOperation(op)
	}
	api.OpenAPI().AddOperation(&huma.Operation{
		OperationID: "preflightPackageContainer", Method: http.MethodPost,
		Path: "/api/v1/packages/containers/{id}/preflight", Summary: "Preview a sealed ZIP package",
		MaxBodyBytes: maxPackageRequestBytes,
		Parameters: []*huma.Param{{Name: "id", In: openAPIPathLocation, Required: true,
			Schema: &huma.Schema{Type: openAPIStringType}}},
		RequestBody: &huma.RequestBody{Required: true, Description: "Profiles and a source reference matching this sealed container", Content: map[string]*huma.MediaType{
			jsonMediaType: {Schema: registry.Schema(reflect.TypeFor[PackagePreflightRequest](), true, "")},
		}},
		Responses: map[string]*huma.Response{
			"200": {Description: "Expiring package preview", Content: map[string]*huma.MediaType{
				jsonMediaType: {Schema: registry.Schema(reflect.TypeFor[PackagePreflight](), true, "")},
			}},
			defaultResponse: {Description: "Package preflight failed", Content: map[string]*huma.MediaType{
				"application/problem+json": {Schema: registry.Schema(reflect.TypeFor[Error](), true, "")},
			}},
		},
	})
}

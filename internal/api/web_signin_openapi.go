package api

import (
	"net/http"
	"reflect"

	"github.com/danielgtaylor/huma/v2"
)

func registerWebSignInOpenAPI(api huma.API) {
	doc := api.OpenAPI()
	jsonBody := func(value any) map[string]*huma.MediaType {
		schema := huma.SchemaFromType(doc.Components.Schemas, reflect.TypeOf(value))
		return map[string]*huma.MediaType{jsonMediaType: {Schema: schema}}
	}
	status := map[string]*huma.Response{"200": {
		Description: "Browser sign-in status and optional process-local tab credentials",
		Content:     jsonBody(webSignInStatus{}),
	}}
	doc.AddOperation(&huma.Operation{
		OperationID: "getWebSignIn", Method: http.MethodGet, Path: webAuthPath,
		Summary:   "Check whether browser key login is enabled",
		Responses: status,
	})
	doc.AddOperation(&huma.Operation{
		OperationID: "loginWebSignIn", Method: http.MethodPost, Path: webLoginPath,
		Summary:     "Sign in using the configured API key",
		RequestBody: &huma.RequestBody{Required: true, Content: jsonBody(webSignInRequest{})},
		Responses:   status,
	})
	doc.AddOperation(&huma.Operation{
		OperationID: "listWebSignIns", Method: http.MethodGet, Path: webAuthSessionsPath,
		Summary: "List browser key logins (master API only)",
		Responses: map[string]*huma.Response{"200": {
			Description: "Live browser logins",
			Content:     jsonBody(webAuthSessionList{}),
		}},
	})
	doc.AddOperation(&huma.Operation{
		OperationID: "revokeWebSignIn", Method: http.MethodDelete, Path: webAuthSessionsPath + "/{id}",
		Summary: "Revoke a browser key login (master API only)",
		Parameters: []*huma.Param{{
			Name: "id", In: "path", Required: true,
			Schema: &huma.Schema{Type: openAPIStringType},
		}},
		Responses: map[string]*huma.Response{"204": {Description: "Browser login revoked"}},
	})
}

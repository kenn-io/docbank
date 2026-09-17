package api

import (
	"fmt"
	"go/token"
	"net/http"
	"reflect"
	"strings"

	"github.com/danielgtaylor/huma/v2"

	"go.kenn.io/docbank/internal/config"
	"go.kenn.io/docbank/internal/daemonauth"
)

// JSON v2 writes nil slices as empty arrays. Keep the generated contract aligned.
func init() { huma.DefaultArrayNullable = false }

// NewOfflineServer builds a Server for offline document generation only: it
// never binds a socket or serves a request, so its API key is a fixed
// placeholder rather than anything meaningful. NewServer refuses an empty
// key, so every offline caller (OpenAPIYAML, `docbank openapi`) must
// go through here instead of reintroducing a keyless Deps of its own.
func NewOfflineServer() *Server {
	cfg := config.Default()
	cfg.Server.APIKey = "openapi-render-only"
	return NewServer(Deps{Cfg: cfg})
}

// OpenAPIYAML renders the API contract without binding a socket or opening
// a vault: handlers are registered but never invoked. `docbank openapi`
// (and doc tooling) call this offline.
func OpenAPIYAML() ([]byte, error) {
	doc := NewOfflineServer().API().OpenAPI()
	// Lifecycle operations belong in the generated daemon client, but stay out
	// of the running server's public documentation.
	doc.AddOperation(&huma.Operation{OperationID: "health", Method: http.MethodGet, Path: "/health",
		Responses: map[string]*huma.Response{"200": {Description: "Daemon health", Content: map[string]*huma.MediaType{jsonMediaType: {Schema: huma.SchemaFromType(doc.Components.Schemas, reflect.TypeFor[struct {
			Status        string `json:"status"`
			Version       string `json:"version"`
			UptimeSeconds int64  `json:"uptime_seconds"`
		}]())}}}}})
	doc.AddOperation(&huma.Operation{OperationID: "shutdownDaemon", Method: http.MethodPost, Path: "/api/daemon/shutdown",
		Parameters: []*huma.Param{{Name: "X-Docbank-Daemon-Token", In: "header", Required: true, Schema: &huma.Schema{Type: "string"}}},
		Responses:  map[string]*huma.Response{"202": {Description: "Shutdown accepted"}}})
	doc.AddOperation(&huma.Operation{OperationID: "challengeDaemon", Method: http.MethodGet, Path: daemonauth.ChallengePath,
		Parameters: []*huma.Param{{Name: "nonce", In: openAPIQueryLocation, Required: true, Schema: &huma.Schema{Type: "string"}}},
		Responses: map[string]*huma.Response{"200": {Description: "Daemon ownership proof", Content: map[string]*huma.MediaType{jsonMediaType: {Schema: huma.SchemaFromType(doc.Components.Schemas, reflect.TypeFor[struct {
			Proof string `json:"proof"`
		}]())}}}}})
	// The in-module Go client shares the API's wire types. Do not generate a
	// second copy with different UUID, time, or optional-field representations.
	registry := doc.Components.Schemas
	for name, schema := range registry.Map() {
		typ := registry.TypeFromRef("#/components/schemas/" + name)
		if typ == nil {
			continue
		}
		for typ.Kind() == reflect.Pointer {
			typ = typ.Elem()
		}
		if !strings.HasPrefix(typ.PkgPath(), "go.kenn.io/docbank/") || !token.IsExported(typ.Name()) ||
			typ.Implements(reflect.TypeFor[huma.SchemaProvider]()) || reflect.PointerTo(typ).Implements(reflect.TypeFor[huma.SchemaProvider]()) {
			continue
		}
		if schema.Extensions == nil {
			schema.Extensions = make(map[string]any)
		}
		name, _, _ := strings.Cut(typ.String(), ".")
		schema.Extensions["x-go-type"] = name + "." + typ.Name()
		schema.Extensions["x-go-type-import"] = struct {
			Path string `json:"path"`
			Name string `json:"name"`
		}{typ.PkgPath(), name}
	}
	// The patch decoder tracks presence separately, while clients send the
	// public optional-field request type with the same wire representation.
	patch := registry.Map()["UpdateSavedQueryRequest"]
	patch.Extensions = map[string]any{
		"x-go-type": "api.SavedQueryPatch",
		"x-go-type-import": struct {
			Path string `json:"path"`
			Name string `json:"name"`
		}{"go.kenn.io/docbank/internal/api", "api"},
	}
	out, err := doc.YAML()
	if err != nil {
		return nil, fmt.Errorf("rendering OpenAPI document: %w", err)
	}
	return out, nil
}

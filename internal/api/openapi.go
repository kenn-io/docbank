package api

import (
	"fmt"

	"github.com/danielgtaylor/huma/v2"

	"go.kenn.io/docbank/internal/config"
)

// JSON v2 writes nil slices as empty arrays. Keep the generated contract aligned.
func init() { huma.DefaultArrayNullable = false }

// NewOfflineServer builds a Server for offline document generation only: it
// never binds a socket or serves a request, so its API key is a fixed
// placeholder rather than anything meaningful. NewServer refuses an empty
// key, so every offline caller (OpenAPIYAML, `docbank openapi --json`) must
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
	out, err := NewOfflineServer().API().OpenAPI().YAML()
	if err != nil {
		return nil, fmt.Errorf("rendering OpenAPI document: %w", err)
	}
	return out, nil
}

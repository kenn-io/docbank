package api

import (
	"fmt"

	"go.kenn.io/docbank/internal/config"
)

// OpenAPIYAML renders the API contract without binding a socket or opening
// a vault: handlers are registered but never invoked. `docbank openapi`
// (and doc tooling) call this offline.
func OpenAPIYAML() ([]byte, error) {
	s := NewServer(Deps{Cfg: config.Default()})
	out, err := s.API().OpenAPI().YAML()
	if err != nil {
		return nil, fmt.Errorf("rendering OpenAPI document: %w", err)
	}
	return out, nil
}

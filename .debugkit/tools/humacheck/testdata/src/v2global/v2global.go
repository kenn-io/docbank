package v2global

import (
	"net/http"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humago"

	_ "v2global/override"
)

func apiConfig() huma.Config {
	config := huma.DefaultConfig("global", "1")
	config.DocsPath = "/docs"
	return config
}

func New() huma.API {
	return humago.NewWithPrefix(http.NewServeMux(), "/api/v1", apiConfig())
}

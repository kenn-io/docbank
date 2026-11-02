package v2inline

import (
	"encoding/json/v2"
	"io"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humago"
)

func New() huma.API {
	mux := http.NewServeMux()
	cfg := huma.DefaultConfig("inline", "1")
	jsonFormat := huma.Format{
		Marshal: func(w io.Writer, value any) error {
			return json.MarshalWrite(w, value)
		},
		Unmarshal: func(data []byte, value any) error {
			return json.Unmarshal(data, value)
		},
	}
	cfg.Formats = map[string]huma.Format{
		"application/json": jsonFormat,
		"json":             jsonFormat,
	}
	return humago.New(mux, cfg)
}

// NewLiteral builds the config as a composite literal.
func NewLiteral() huma.API {
	mux := http.NewServeMux()
	return huma.NewAPI(huma.Config{
		Formats: map[string]huma.Format{
			"application/json": {
				Marshal:   func(w io.Writer, v any) error { return json.MarshalWrite(w, v) },
				Unmarshal: func(data []byte, v any) error { return json.Unmarshal(data, v) },
			},
		},
	}, humago.NewAdapter(mux, "/api"))
}

// NewNamedResult mutates a named result before a bare return.
func NewNamedResult() huma.API {
	return humago.New(http.NewServeMux(), namedConfig())
}

func namedConfig() (cfg huma.Config) {
	cfg = huma.DefaultConfig("named", "1")
	cfg.Formats["application/json"] = huma.Format{
		Marshal:   func(w io.Writer, v any) error { return json.MarshalWrite(w, v) },
		Unmarshal: func(data []byte, v any) error { return json.Unmarshal(data, v) },
	}
	return
}

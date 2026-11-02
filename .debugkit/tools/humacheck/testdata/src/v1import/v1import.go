// Package v1import installs a JSON v2 format but still imports encoding/json
// elsewhere, which is what the rule reports.
package v1import

import (
	"encoding/json" // want "encoding/json \(v1\) is imported"
	"io"
	"net/http"

	jsonv2 "encoding/json/v2"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humago"
)

func New() huma.API {
	config := huma.DefaultConfig("codec", "1")
	config.Formats["application/json"] = huma.Format{
		Marshal:   func(w io.Writer, v any) error { return jsonv2.MarshalWrite(w, v) },
		Unmarshal: func(data []byte, v any) error { return jsonv2.Unmarshal(data, v) },
	}
	return humago.New(http.NewServeMux(), config)
}

// Settings is unrelated to Huma; the import is still reported because any
// v1 use in the module can leak v1 semantics into API payloads.
func Settings(data []byte) (map[string]any, error) {
	var out map[string]any
	return out, json.Unmarshal(data, &out)
}

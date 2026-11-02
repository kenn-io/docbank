// Package unusedmap builds a v2 format map that never reaches a Config, so
// the API still runs on Huma's v1 defaults and the missing-install finding
// is expected.
package unusedmap

import (
	jsonv2 "encoding/json/v2"
	"io"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humago"
)

var spare = map[string]huma.Format{
	"application/json": {
		Marshal:   func(w io.Writer, v any) error { return jsonv2.MarshalWrite(w, v) },
		Unmarshal: func(data []byte, v any) error { return jsonv2.Unmarshal(data, v) },
	},
}

func New() huma.API {
	_ = spare
	return humago.New(http.NewServeMux(), huma.DefaultConfig("unused", "1"))
}

// An index write of a v2 format into a map that never reaches a Config is
// not an install either.
func stash() {
	spare["application/json"] = huma.Format{
		Marshal:   func(w io.Writer, v any) error { return jsonv2.MarshalWrite(w, v) },
		Unmarshal: func(data []byte, v any) error { return jsonv2.Unmarshal(data, v) },
	}
}

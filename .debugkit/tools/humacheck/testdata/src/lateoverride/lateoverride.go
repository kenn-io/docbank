// Package lateoverride copies huma.DefaultJSONFormat into the defaults map
// before replacing the variable, so the map keeps the v1 value and the
// missing-install finding is expected.
package lateoverride

import (
	jsonv2 "encoding/json/v2"
	"io"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humago"
)

func init() {
	huma.DefaultFormats["application/json"] = huma.DefaultJSONFormat
	huma.DefaultJSONFormat = huma.Format{
		Marshal:   func(w io.Writer, v any) error { return jsonv2.MarshalWrite(w, v) },
		Unmarshal: func(data []byte, v any) error { return jsonv2.Unmarshal(data, v) },
	}
}

func New() huma.API {
	return humago.New(http.NewServeMux(), huma.DefaultConfig("late", "1"))
}

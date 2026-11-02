// Package codecs defines the module's shared JSON v2 format.
package codecs

import (
	jsonv2 "encoding/json/v2"
	"io"

	"github.com/danielgtaylor/huma/v2"
)

var JSONFormat = huma.Format{
	Marshal:   func(w io.Writer, v any) error { return jsonv2.MarshalWrite(w, v) },
	Unmarshal: func(data []byte, v any) error { return jsonv2.Unmarshal(data, v) },
}

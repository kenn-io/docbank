package embedclient

import (
	"encoding/base64"
	"encoding/binary"
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"math"

	jsonv2 "encoding/json/v2"

	"go.kenn.io/kit/embedconfig"
)

type wireRequest struct {
	Model          string   `json:"model"`
	Input          []string `json:"input"`
	InputType      string   `json:"input_type,omitempty"`
	Dimensions     int      `json:"dimensions,omitzero"`
	EncodingFormat string   `json:"encoding_format,omitempty"`
}

type wireResponse struct {
	Data []wireItem `json:"data"`
}

type wireItem struct {
	Embedding wireEmbedding `json:"embedding"`
	Index     *int          `json:"index"`
}

// wireEmbedding is one returned vector. Providers send a JSON array of finite
// numbers or a base64 string of little-endian float32 values.
type wireEmbedding struct {
	values []float32
	err    error
}

func (e *wireEmbedding) UnmarshalJSONFrom(dec *jsontext.Decoder) error {
	switch dec.PeekKind() {
	case '[':
		var elements []*float64
		if err := jsonv2.UnmarshalDecode(dec, &elements); err != nil {
			return errors.New("vector is invalid")
		}
		e.values, e.err = finiteFloat32s(elements)
		return nil
	case '"':
		return e.unmarshalBase64(dec)
	default:
		if err := dec.SkipValue(); err != nil {
			return err
		}
		e.err = errors.New("vector is invalid")
		return nil
	}
}

func (e *wireEmbedding) unmarshalBase64(dec *jsontext.Decoder) error {
	tok, err := dec.ReadToken()
	if err != nil {
		return err
	}
	if tok.Kind() != '"' {
		e.err = errors.New("vector is invalid")
		return nil
	}
	decoded, err := base64.StdEncoding.DecodeString(tok.String())
	if err != nil {
		return errors.New("vector is invalid")
	}
	if len(decoded)%4 != 0 {
		return errors.New("vector is invalid")
	}
	out := make([]float32, len(decoded)/4)
	for i := range out {
		value := math.Float32frombits(binary.LittleEndian.Uint32(decoded[i*4 : (i+1)*4]))
		if math.IsNaN(float64(value)) || math.IsInf(float64(value), 0) {
			e.err = fmt.Errorf("component %d is not finite", i)
			return nil
		}
		out[i] = value
	}
	e.values = out
	return nil
}

func finiteFloat32s(elements []*float64) ([]float32, error) {
	out := make([]float32, len(elements))
	for i, element := range elements {
		if element == nil {
			return nil, fmt.Errorf("component %d is null", i)
		}
		value := float32(*element)
		if math.IsNaN(*element) || math.IsInf(*element, 0) || math.IsInf(float64(value), 0) {
			return nil, fmt.Errorf("component %d is not finite", i)
		}
		out[i] = value
	}
	return out, nil
}

func (c *Client) classify(payload []byte, count int) ([][]float32, []error, error) {
	var decoded wireResponse
	if err := jsonv2.Unmarshal(payload, &decoded); err != nil {
		return nil, nil, errors.New("embed response is invalid")
	}
	raw, err := orderItems(decoded.Data, count)
	if err != nil {
		return nil, nil, err
	}
	out := make([][]float32, len(raw))
	var problems []error
	for i, item := range raw {
		vector, err := decodeVector(item, c.model.Dimensions, c.model.Normalization)
		if err != nil {
			problems = append(problems, &VectorError{Index: i, Err: err})
			continue
		}
		out[i] = vector
	}
	return out, problems, nil
}

func orderItems(items []wireItem, count int) ([]wireEmbedding, error) {
	if len(items) != count {
		return nil, fmt.Errorf("embed response contained %d vectors for %d inputs", len(items), count)
	}
	missing := 0
	for _, item := range items {
		if item.Index == nil {
			missing++
		}
	}
	out := make([]wireEmbedding, count)
	if missing == count {
		for i, item := range items {
			out[i] = item.Embedding
		}
		return out, nil
	}
	if missing != 0 {
		return nil, errors.New("embed response mixes indexed and unindexed vectors")
	}
	seen := make([]bool, count)
	for pos, item := range items {
		index := *item.Index
		if index < 0 || index >= count {
			return nil, fmt.Errorf("embed response item %d has index %d", pos, index)
		}
		if seen[index] {
			return nil, fmt.Errorf("embed response contains duplicate index %d", index)
		}
		seen[index] = true
		out[index] = item.Embedding
	}
	return out, nil
}

func decodeVector(raw wireEmbedding, dims int, normalization embedconfig.Normalization) ([]float32, error) {
	if raw.err != nil {
		return nil, raw.err
	}
	if len(raw.values) != dims {
		return nil, fmt.Errorf("has %d dimensions, expected %d", len(raw.values), dims)
	}
	out := make([]float32, len(raw.values))
	var sumSquares float64
	for i, value := range raw.values {
		if math.IsNaN(float64(value)) || math.IsInf(float64(value), 0) {
			return nil, fmt.Errorf("component %d is not finite", i)
		}
		out[i] = value
		sumSquares += float64(value) * float64(value)
	}
	if sumSquares == 0 {
		return nil, errors.New("has zero norm")
	}
	if normalization != embedconfig.NormalizationL2 {
		return out, nil
	}
	norm := math.Sqrt(sumSquares)
	for i, value := range out {
		out[i] = float32(float64(value) / norm)
	}
	return out, nil
}

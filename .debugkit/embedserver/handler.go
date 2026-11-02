package embedserver

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"encoding/json/jsontext"
	jsonv2 "encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
)

// Ollama decodes requests with v1 semantics, replacing invalid UTF-8 and lone
// surrogates with U+FFFD and keeping the last duplicate member.
var decodeOptions = jsonv2.JoinOptions(jsontext.AllowInvalidUTF8(true), jsontext.AllowDuplicateNames(true))

type handler struct {
	engine engine
	gate   chan struct{}
	log    io.Writer
}
type request struct {
	Model      string          `json:"model"`
	Input      json.RawMessage `json:"input"`
	Dimensions int             `json:"dimensions"`
	Encoding   string          `json:"encoding_format"`
}
type item struct {
	Object    string `json:"object"`
	Index     int    `json:"index"`
	Embedding any    `json:"embedding"`
}
type usage struct {
	Prompt int `json:"prompt_tokens"`
	Total  int `json:"total_tokens"`
}
type response struct {
	Object string `json:"object"`
	Data   []item `json:"data"`
	Model  string `json:"model"`
	Usage  usage  `json:"usage"`
}

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 64<<20))
	if err != nil {
		if _, ok := errors.AsType[*http.MaxBytesError](err); ok {
			http.Error(w, "request exceeds 64 MiB", http.StatusRequestEntityTooLarge)
		} else {
			http.Error(w, "could not read request body", http.StatusBadRequest)
		}
		return
	}
	var req request
	if err := jsonv2.Unmarshal(body, &req, decodeOptions); err != nil {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	if req.Model != "nomic-embed-text" && req.Model != "nomic-embed-text:latest" && req.Model != "nomic-embed-text:v1.5" {
		http.Error(w, "model not found", http.StatusNotFound)
		return
	}
	var inputs []string
	var text *string
	if jsonv2.Unmarshal(req.Input, &text, decodeOptions) == nil && text != nil {
		inputs = []string{*text}
	} else {
		var texts []*string
		if err := jsonv2.Unmarshal(req.Input, &texts, decodeOptions); err != nil {
			http.Error(w, "input must be a string or string array", http.StatusBadRequest)
			return
		}
		for _, text := range texts {
			if text == nil {
				http.Error(w, "input must contain strings", http.StatusBadRequest)
				return
			}
			inputs = append(inputs, *text)
		}
	}
	if len(inputs) == 0 {
		http.Error(w, "input must contain at least one string", http.StatusBadRequest)
		return
	}
	if req.Dimensions < 0 || req.Dimensions > 768 {
		http.Error(w, "dimensions must be between 0 and 768", http.StatusBadRequest)
		return
	}
	if req.Encoding != "" && req.Encoding != "float" && req.Encoding != "base64" {
		http.Error(w, "encoding_format must be float or base64", http.StatusBadRequest)
		return
	}
	if req.Dimensions == 0 {
		req.Dimensions = 768
	}
	out, err := h.embeddings(r.Context(), req, inputs)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return
		}
		_, _ = fmt.Fprintf(h.log, "inference failed: %v\n", err)
		http.Error(w, "inference failed", http.StatusInternalServerError)
		return
	}
	data, err := jsonv2.Marshal(out)
	if err != nil {
		http.Error(w, "encode failed", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(data)
}

func (h *handler) embeddings(ctx context.Context, req request, inputs []string) (response, error) {
	out := response{Object: "list", Model: req.Model, Data: make([]item, len(inputs))}
	for i, text := range inputs {
		vector, tokens, err := h.embed(ctx, text)
		if err != nil {
			return out, err
		}
		vector = vector[:req.Dimensions]
		var squared float64
		for _, v := range vector {
			squared += float64(v) * float64(v)
		}
		if norm := math.Sqrt(squared); norm > 0 {
			for j, v := range vector {
				vector[j] = float32(float64(v) / norm)
			}
		}
		var embedding any = vector
		if req.Encoding == "base64" {
			data := make([]byte, len(vector)*4)
			for j, v := range vector {
				binary.LittleEndian.PutUint32(data[j*4:], math.Float32bits(v))
			}
			embedding = base64.StdEncoding.EncodeToString(data)
		}
		out.Data[i] = item{Object: "embedding", Index: i, Embedding: embedding}
		out.Usage.Prompt += tokens
		out.Usage.Total += tokens
	}
	return out, nil
}

// embed holds the gate for one input, so other requests run between batch items.
func (h *handler) embed(ctx context.Context, text string) ([]float32, int, error) {
	select {
	case h.gate <- struct{}{}:
	case <-ctx.Done():
		return nil, 0, ctx.Err()
	}
	defer func() { <-h.gate }()
	return h.engine.embed(ctx, text)
}

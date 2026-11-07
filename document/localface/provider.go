// Package localface adapts Kit's local face model to the embedding worker.
package localface

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"

	"go.kenn.io/docbank/document"
	"go.kenn.io/kit/embedclient"
	"go.kenn.io/kit/embedconfig"
)

const AdapterContract = "docbank-local-face/v1"

type Provider struct {
	descriptor document.EmbeddingDescriptor
	resolve    func(context.Context) (string, error)
}

func New(descriptor document.EmbeddingDescriptor, resolve func(context.Context) (string, error)) (*Provider, error) {
	d, err := document.NewEmbeddingDescriptor(descriptor)
	if err != nil {
		return nil, err
	}
	if d.Model != embedclient.FaceModel || d.ModelRevision != embedclient.FaceRevision || d.Dimension != 512 || d.Metric != document.VectorMetricL2 || d.Normalization != "unit_length" || d.ScalarEncoding != "float32" || d.SupportsTextQuery || !slices.Equal(d.InputKinds, []document.EmbeddingInputKind{document.EmbeddingInputFace}) || d.DocumentFormatter != embedclient.FaceRecipe || resolve == nil {
		return nil, errors.New("invalid local face model contract")
	}
	return &Provider{d, resolve}, nil
}
func (p *Provider) Descriptor() document.EmbeddingDescriptor { return p.descriptor }
func (p *Provider) Embed(ctx context.Context, inputs []document.EmbeddingInput, auth document.EmbeddingAuthorization) (document.EmbeddingResult, error) {
	if err := document.ValidateEmbeddingProviderRequest(p, inputs, auth); err != nil {
		return document.EmbeddingResult{}, err
	}
	origin, err := p.resolve(ctx)
	if err != nil {
		return document.EmbeddingResult{}, err
	}
	client, err := embedclient.New(embedclient.Options{Model: embedconfig.Model{Name: embedclient.FaceModel, Dimensions: 512, Metric: embedconfig.MetricCosine, Normalization: embedconfig.NormalizationNone}, Deployment: embedconfig.Deployment{BaseURL: strings.TrimRight(origin, "/") + "/v1"}})
	if err != nil {
		return document.EmbeddingResult{}, fmt.Errorf("create local face client: %w", err)
	}
	var preview []byte
	boxes := make([]embedclient.FaceBox, len(inputs))
	for i, input := range inputs {
		data, err := io.ReadAll(io.LimitReader(input.Source, auth.MaxInputBytes+1))
		if err != nil {
			return document.EmbeddingResult{}, err
		}
		if int64(len(data)) > auth.MaxInputBytes {
			return document.EmbeddingResult{}, errors.New("face preview exceeds byte limit")
		}
		if i == 0 {
			preview = data
		} else if !bytes.Equal(preview, data) {
			return document.EmbeddingResult{}, errors.New("face batch spans previews")
		}
		boxes[i] = embedclient.FaceBox(*input.Box)
	}
	result := document.EmbeddingResult{Vectors: make([]document.EmbeddingVector, len(inputs))}
	batch, err := client.EmbedFaces(ctx, preview, boxes)
	if err != nil {
		return document.EmbeddingResult{}, fmt.Errorf("embed local faces: %w", err)
	}
	for i, item := range batch.Data {
		result.Vectors[i] = document.EmbeddingVector{Key: inputs[i].Key, Values: item.Embedding}
	}
	return result, nil
}

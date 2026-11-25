package document

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"

	"go.kenn.io/kit/embedclient"
	"go.kenn.io/kit/embedconfig"
)

type LocalFaceProvider struct {
	descriptor EmbeddingDescriptor
	resolve    func(context.Context) (string, error)
}

func NewLocalFaceProvider(descriptor EmbeddingDescriptor, resolve func(context.Context) (string, error)) (*LocalFaceProvider, error) {
	d, err := NewEmbeddingDescriptor(descriptor)
	if err != nil {
		return nil, err
	}
	if d.Model != embedclient.FaceModel || d.ModelRevision != embedclient.FaceRevision || d.Dimension != 512 || d.Metric != VectorMetricL2 || d.Normalization != "unit_length" || d.ScalarEncoding != "float32" || d.SupportsTextQuery || !slices.Equal(d.InputKinds, []EmbeddingInputKind{EmbeddingInputFace}) || d.DocumentFormatter != embedclient.FaceRecipe || resolve == nil {
		return nil, errors.New("invalid local face model contract")
	}
	return &LocalFaceProvider{d, resolve}, nil
}
func (p *LocalFaceProvider) Descriptor() EmbeddingDescriptor { return p.descriptor }
func (p *LocalFaceProvider) Embed(ctx context.Context, inputs []EmbeddingInput, auth EmbeddingAuthorization) (EmbeddingResult, error) {
	if err := ValidateEmbeddingProviderRequest(p, inputs, auth); err != nil {
		return EmbeddingResult{}, err
	}
	origin, err := p.resolve(ctx)
	if err != nil {
		return EmbeddingResult{}, err
	}
	client, err := embedclient.New(embedclient.Options{Model: embedconfig.Model{Name: embedclient.FaceModel, Dimensions: 512, Metric: embedconfig.MetricCosine, Normalization: embedconfig.NormalizationNone}, Deployment: embedconfig.Deployment{BaseURL: strings.TrimRight(origin, "/") + "/v1"}})
	if err != nil {
		return EmbeddingResult{}, fmt.Errorf("create local face client: %w", err)
	}
	var preview []byte
	boxes := make([]embedclient.FaceBox, len(inputs))
	for i, input := range inputs {
		data, err := io.ReadAll(io.LimitReader(input.Source, auth.MaxInputBytes+1))
		if err != nil {
			return EmbeddingResult{}, err
		}
		if int64(len(data)) > auth.MaxInputBytes {
			return EmbeddingResult{}, errors.New("face preview exceeds byte limit")
		}
		if i == 0 {
			preview = data
		} else if !bytes.Equal(preview, data) {
			return EmbeddingResult{}, errors.New("face batch spans previews")
		}
		boxes[i] = embedclient.FaceBox(*input.Box)
	}
	result := EmbeddingResult{Vectors: make([]EmbeddingVector, len(inputs))}
	batch, err := client.EmbedFaces(ctx, preview, boxes)
	if err != nil {
		return EmbeddingResult{}, fmt.Errorf("embed local faces: %w", err)
	}
	for i, item := range batch.Data {
		result.Vectors[i] = EmbeddingVector{Key: inputs[i].Key, Values: item.Embedding}
	}
	return result, nil
}

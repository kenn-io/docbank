package api

import (
	"context"
	"errors"
	"net/http"
	"reflect"

	"github.com/danielgtaylor/huma/v2"

	"go.kenn.io/docbank/internal/vector"
)

func registerEmbeddingRoutes(api huma.API, d Deps) {
	type listOutput struct{ Body EmbeddingGenerationList }
	huma.Register(api, huma.Operation{
		OperationID: "listEmbeddingGenerations", Method: http.MethodGet,
		Path: "/api/v1/embeddings", Summary: "List local embedding generations and coverage",
	}, func(ctx context.Context, _ *struct{}) (*listOutput, error) {
		out := &listOutput{Body: EmbeddingGenerationList{
			Configured: d.Embeddings != nil, Items: []EmbeddingGeneration{},
		}}
		if d.Embeddings == nil {
			return out, nil
		}
		items, err := d.Embeddings.Generations(ctx)
		if err != nil {
			return nil, embeddingProblem(err)
		}
		for _, item := range items {
			out.Body.Items = append(out.Body.Items, embeddingGeneration(item))
		}
		return out, nil
	})

	streamSchema := api.OpenAPI().Components.Schemas.Schema(
		reflect.TypeFor[EmbeddingBuildEvent](), true, "EmbeddingBuildEvent")
	huma.Register(api, huma.Operation{
		OperationID: "buildEmbeddingGeneration", Method: http.MethodPost,
		Path:    "/api/v1/embeddings/build/stream",
		Summary: "Mirror verified text and build the configured embedding generation",
		Description: "Returns newline-delimited JSON. Progress precedes exactly one terminal " +
			"result or error event; an HTTP 200 only means the stream started.",
		Responses: map[string]*huma.Response{
			"200": {Description: "Embedding progress followed by one terminal result or error",
				Content: map[string]*huma.MediaType{
					"application/x-ndjson": {Schema: streamSchema},
				}},
		},
	}, func(_ context.Context, _ *struct{}) (*huma.StreamResponse, error) {
		if d.Embeddings == nil {
			return nil, NewError(http.StatusUnprocessableEntity, "embeddings_unconfigured",
				"embeddings are not configured; add [embeddings] base_url and model to config.toml")
		}
		return &huma.StreamResponse{Body: func(hctx huma.Context) {
			hctx.SetHeader("Content-Type", "application/x-ndjson")
			hctx.SetHeader("Cache-Control", "no-store")
			runCtx, cancel := context.WithCancel(hctx.Context())
			defer cancel()
			stream := newEventStreamWriter[EmbeddingBuildEvent](hctx.BodyWriter(), cancel)
			result, err := d.Embeddings.Build(runCtx, func(progress vector.Progress) {
				stream.send(EmbeddingBuildEvent{Type: "progress", Progress: &EmbeddingBuildProgress{
					Phase: progress.Phase, Done: progress.Done, Total: progress.Total,
				}})
			})
			if stream.err() != nil {
				return
			}
			if err != nil {
				stream.send(EmbeddingBuildEvent{Type: "error", Error: embeddingProblem(err)})
				return
			}
			stream.send(EmbeddingBuildEvent{Type: "result", Result: &EmbeddingBuildResult{
				Fingerprint: result.Fingerprint, Model: result.Model, Dimensions: result.Dimensions,
				Mirrored: result.Mirrored, Removed: result.Removed, Embedded: result.Embedded,
				Chunks: result.Chunks, Skipped: result.Skipped, Stale: result.Stale,
				Activated: result.Activated,
			}})
		}}, nil
	})
}

func embeddingGeneration(item vector.GenerationInfo) EmbeddingGeneration {
	return EmbeddingGeneration{
		Fingerprint: item.Fingerprint, Model: item.Model, Dimensions: item.Dimensions,
		State: item.State, Embedded: item.Embedded, Skipped: item.Skipped, Pending: item.Pending,
	}
}

func embeddingProblem(err error) *Error {
	if errors.Is(err, vector.ErrBuildRunning) {
		return NewError(http.StatusConflict, "embeddings_build_running", err.Error())
	}
	return NewError(http.StatusInternalServerError, "embeddings_failed", err.Error())
}

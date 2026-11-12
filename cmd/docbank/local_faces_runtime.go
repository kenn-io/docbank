package main

import (
	"context"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/localface"
	"go.kenn.io/docbank/internal/config"
	"go.kenn.io/docbank/internal/processing"
	"go.kenn.io/kit/embedlocal"
)

func localFaceProfile() (processing.ProfileConfig, error) {
	profile, descriptor, err := config.LocalFaces()
	if err != nil {
		return processing.ProfileConfig{}, err
	}
	provider, err := localface.New(descriptor, func(ctx context.Context) (string, error) { return localEmbeddingOrigin(ctx, embedlocal.FaceModel, nil) })
	if err != nil {
		return processing.ProfileConfig{}, err
	}
	return processing.ProfileConfig{Profile: profile, EmbeddingProviders: map[string]document.EmbeddingProvider{config.LocalFacesProfile: provider}}, nil
}

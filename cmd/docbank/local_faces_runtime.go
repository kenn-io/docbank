package main

import (
	"context"
	"strings"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/localface"
	"go.kenn.io/docbank/internal/config"
	"go.kenn.io/docbank/internal/processing"
	"go.kenn.io/kit/embedlocal"
)

func localFaceOrigin(ctx context.Context) (string, error) {
	opts := embedlocal.Options{SkipOllama: true, Model: embedlocal.FaceModel}
	origin, err := embedlocal.Endpoint(ctx, opts)
	return strings.TrimSuffix(origin, "/v1"), err
}
func localFaceProfile() (processing.ProfileConfig, error) {
	profile, descriptor, err := config.LocalFaces()
	if err != nil {
		return processing.ProfileConfig{}, err
	}
	provider, err := localface.New(descriptor, localFaceOrigin)
	if err != nil {
		return processing.ProfileConfig{}, err
	}
	return processing.ProfileConfig{Profile: profile, EmbeddingProviders: map[string]document.EmbeddingProvider{config.LocalFacesProfile: provider}}, nil
}

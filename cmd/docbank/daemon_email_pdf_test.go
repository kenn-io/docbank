package main

import (
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/config"
	"go.kenn.io/docbank/internal/processing"
	"testing"
)

func TestConfigureEmailPDFMissingAndInvalidRuntime(t *testing.T) {
	registry := processing.NewRenditionRuntimeRegistry()
	r, err := configureEmailPDF(config.Default(), nil, nil, t.TempDir(), registry)
	require.NoError(t, err)
	require.Nil(t, r)
	cfg := config.Default()
	cfg.EmailPDF = &config.EmailPDFConfig{Chromium: "/missing/chrome"}
	_, err = configureEmailPDF(cfg, nil, nil, t.TempDir(), registry)
	require.Error(t, err)
	require.False(t, registry.Ready())
}

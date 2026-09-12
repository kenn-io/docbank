package main

import (
	"fmt"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/emailpdf"
	"go.kenn.io/docbank/internal/blob"
	"go.kenn.io/docbank/internal/config"
	"go.kenn.io/docbank/internal/processing"
	"go.kenn.io/docbank/internal/store"
	"os"
)

func configureEmailPDF(cfg config.Config, catalog *store.Store, blobs *blob.Store, spool string, registry *processing.RenditionRuntimeRegistry) (*processing.EmailPDFRuntime, error) {
	if cfg.EmailPDF == nil {
		return nil, nil //nolint:nilnil // An omitted opt-in configuration intentionally has no runtime.
	}
	worker, err := os.Executable()
	if err != nil {
		return nil, err
	}
	workerHash, err := emailpdf.FileSHA256(worker)
	if err != nil {
		return nil, err
	}
	c := cfg.EmailPDF
	renderer, err := emailpdf.NewRuntime(emailpdf.RuntimeConfig{Worker: worker, WorkerSHA256: workerHash, Chromium: c.Chromium, Bundle: c.Bundle, BundleSHA256: c.BundleSHA256, Version: c.Version, Fonts: c.Fonts, FontsSHA256: c.FontsSHA256, Spool: spool})
	if err != nil {
		return nil, fmt.Errorf("configuring [email_pdf]: %w", err)
	}
	r := &processing.EmailPDFRuntime{Catalog: catalog, Blobs: blobs, Renderer: renderer, Spool: spool, Recipe: document.EmailPDFRecipeV1{Contract: document.EmailPDFContract, RendererVersion: c.Version, RendererSHA256: c.BundleSHA256, WorkerSHA256: workerHash, FontsSHA256: c.FontsSHA256, Paper: "A4"}}
	for _, paper := range []string{"A4", "Letter"} {
		recipe := r.Recipe
		recipe.Paper = paper
		d, err := document.EmailPDFDescriptor(recipe)
		if err != nil {
			return nil, err
		}
		if err = registry.Register(d.Fingerprint, r); err != nil {
			return nil, err
		}
	}
	return r, nil
}

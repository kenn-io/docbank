package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"runtime"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/emailpdf"
	"go.kenn.io/docbank/internal/blob"
	"go.kenn.io/docbank/internal/config"
	"go.kenn.io/docbank/internal/processing"
	"go.kenn.io/docbank/internal/store"
)

func configureEmailPDF(ctx context.Context, cfg config.Config, catalog *store.Store, blobs *blob.Store, decodeSpool, renderSpool string, registry *processing.RenditionRuntimeRegistry) (*processing.EmailPDFRuntime, error) {
	if cfg.EmailPDF == nil {
		return nil, nil //nolint:nilnil // An omitted opt-in configuration intentionally has no runtime.
	}
	if runtime.GOOS != "linux" {
		return nil, fmt.Errorf("%w: rendering requires a Linux daemon with a systemd user manager and bubblewrap", emailpdf.ErrUnavailable)
	}
	worker, err := os.Executable()
	if err != nil {
		return nil, err
	}
	workerHash, err := emailpdf.FileSHA256(worker)
	if err != nil {
		return nil, err
	}
	bubblewrap, lookupErr := exec.LookPath("bwrap")
	var bubblewrapHash string
	if lookupErr == nil {
		bubblewrapHash, err = emailpdf.FileSHA256(bubblewrap)
		if err != nil {
			return nil, err
		}
	} else {
		bubblewrap = ""
	}
	c := cfg.EmailPDF
	renderer, err := emailpdf.NewRuntime(emailpdf.RuntimeConfig{
		Worker: worker, WorkerSHA256: workerHash,
		Bubblewrap: bubblewrap, BubblewrapSHA256: bubblewrapHash,
		Chromium: c.Chromium, Version: c.Version,
		Bundle: c.Bundle, BundleSHA256: c.BundleSHA256,
		Fonts: c.Fonts, FontsSHA256: c.FontsSHA256, Spool: renderSpool,
	})
	if err != nil {
		return nil, fmt.Errorf("configuring [email_pdf]: %w", err)
	}
	if err := renderer.CheckAvailable(ctx); err != nil {
		return nil, err
	}
	if _, _, err := renderer.Render(ctx, emailpdf.HTML{Bytes: []byte("<!doctype html><html><body>Docbank startup probe</body></html>")}); err != nil {
		return nil, fmt.Errorf("%w: Chromium startup probe failed; verify its version and allow its sandbox's nested user namespaces inside bubblewrap, including host AppArmor policy: %w", emailpdf.ErrUnavailable, err)
	}
	r := &processing.EmailPDFRuntime{
		Catalog: catalog, Blobs: blobs, Renderer: renderer, Spool: decodeSpool,
		Recipe: document.EmailPDFRecipeV1{
			Contract: document.EmailPDFContract, RendererVersion: c.Version,
			RendererSHA256: c.BundleSHA256, WorkerSHA256: workerHash,
			BubblewrapSHA256: bubblewrapHash, FontsSHA256: c.FontsSHA256, Paper: "A4",
		},
	}
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

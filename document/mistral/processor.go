package mistral

import (
	"context"
	"errors"
	"fmt"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/ocr"
)

// ProcessorConfig binds provider-neutral processing to the existing Mistral
// policy, capability evidence, and private staging boundary.
type ProcessorConfig struct {
	Client             *Client
	Policy             Policy
	CapabilityManifest CapabilityManifest
	SpoolDirectory     string
	MaxSpoolBytes      int64
	MinFreeBytes       int64
}

// Processor adapts the consent-gated Mistral client to ocr.Processor.
type Processor struct {
	client            *Client
	policy            Policy
	manifest          CapabilityManifest
	spoolDirectory    string
	maxSpoolBytes     int64
	minFreeBytes      int64
	identity          ocr.Identity
	policyFingerprint string
}

var _ ocr.Processor = (*Processor)(nil)

// NewProcessor validates the exact capability and staging authority before
// accepting source bytes.
func NewProcessor(config ProcessorConfig) (*Processor, error) {
	if config.Client == nil || config.Policy.digest == "" || config.Client.policy.digest != config.Policy.digest {
		return nil, errors.New("mistral OCR processor requires a client and its exact policy")
	}
	if config.SpoolDirectory == "" || config.MaxSpoolBytes < config.Policy.values.MaxDocumentBytes || config.MinFreeBytes <= 0 {
		return nil, errors.New("mistral OCR processor staging bounds are invalid")
	}
	manifest := cloneCapabilityManifest(config.CapabilityManifest)
	policyFingerprint, err := config.Policy.Fingerprint(manifest)
	if err != nil {
		return nil, fmt.Errorf("configure Mistral OCR processor capability policy: %w", err)
	}
	identity, err := ocr.NewIdentity(defaultProvider, config.Policy.values.Model, config.Policy.values.Model)
	if err != nil {
		return nil, fmt.Errorf("configure Mistral OCR identity: %w", err)
	}
	return &Processor{
		client: config.Client, policy: config.Policy, manifest: manifest,
		spoolDirectory: config.SpoolDirectory, maxSpoolBytes: config.MaxSpoolBytes,
		minFreeBytes: config.MinFreeBytes, identity: identity, policyFingerprint: policyFingerprint,
	}, nil
}

func cloneCapabilityManifest(manifest CapabilityManifest) CapabilityManifest {
	clone := manifest
	clone.Results = make([]CapabilityResult, len(manifest.Results))
	copy(clone.Results, manifest.Results)
	for index := range clone.Results {
		if clone.Results[index].ProviderBytes != nil {
			providerBytes := *clone.Results[index].ProviderBytes
			clone.Results[index].ProviderBytes = &providerBytes
		}
	}
	return clone
}

// Identity returns the pinned Mistral provider and model identity.
func (p *Processor) Identity() ocr.Identity {
	if p == nil {
		return ocr.Identity{}
	}
	return p.identity
}

// Process stages, authorizes, sends, normalizes, and removes one source. Raw
// provider JSON and Markdown remain transient.
func (p *Processor) Process(ctx context.Context, source ocr.Source) (result ocr.Result, err error) {
	if p == nil {
		if source.Content != nil {
			_ = source.Content.Close()
		}
		return ocr.Result{}, errors.New("mistral OCR processor is nil")
	}
	if err := source.Validate(); err != nil {
		if source.Content != nil {
			_ = source.Content.Close()
		}
		return ocr.Result{}, &ocr.ProviderError{Kind: ocr.ErrorInvalidInput, Cause: err}
	}
	prepared, err := Prepare(ctx, source.Content, p.policy, PrepareOptions{
		Directory: p.spoolDirectory, DeclaredMediaType: source.MediaType,
		ExpectedSize: source.Size, ExpectedSHA256: source.SHA256,
		MaxSpoolBytes: p.maxSpoolBytes, MinFreeBytes: p.minFreeBytes,
	})
	if err != nil {
		return ocr.Result{}, classifyProcessorError(ctx, err, RequestMetrics{})
	}
	defer func() {
		cleanupErr := prepared.Release()
		if err != nil {
			err = errors.Join(err, cleanupErr)
		} else {
			result.CleanupError = cleanupErr
		}
	}()

	authorization, err := p.policy.Authorize(p.manifest, prepared.Format().ID)
	if err != nil {
		return ocr.Result{}, &ocr.ProviderError{Kind: ocr.ErrorCapabilityChanged, Cause: err}
	}
	providerResult, err := p.client.Process(ctx, prepared, authorization)
	if err != nil {
		return ocr.Result{}, classifyProcessorError(ctx, err, MetricsFromError(err))
	}
	normalized, err := document.NormalizeDocument(providerResult.Document, p.policy.NormalizePolicy())
	if err != nil {
		return ocr.Result{}, &ocr.ProviderError{
			Kind: ocr.ErrorMalformedOutput, Metrics: toOCRMetrics(providerResult.Metrics), Cause: err,
		}
	}
	return ocr.Result{
		Source: providerResult.Document, Document: normalized,
		Identity: p.identity, PolicyFingerprint: p.policyFingerprint,
		UnitsProcessed: providerResult.UnitsProcessed, ProviderBytes: providerResult.ProviderBytes,
		Metrics: toOCRMetrics(providerResult.Metrics),
	}, nil
}

func classifyProcessorError(ctx context.Context, err error, metrics RequestMetrics) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return &ocr.ProviderError{Metrics: toOCRMetrics(metrics), Cause: ctxErr}
	}
	kind := ocr.ErrorMalformedOutput
	switch {
	case errors.Is(err, ErrSpoolCapacity):
		kind = ocr.ErrorCapacity
	case errors.Is(err, ErrSpoolUnavailable):
		kind = ocr.ErrorTransient
	case errors.Is(err, ErrInvalidSource):
		kind = ocr.ErrorInvalidInput
	case errors.Is(err, ErrTransientResponse):
		kind = ocr.ErrorTransient
	case errors.Is(err, ErrPermanentResponse):
		kind = ocr.ErrorRejected
	case errors.Is(err, ErrResponseTooLarge):
		kind = ocr.ErrorResponseTooLarge
	case errors.Is(err, ErrCapabilityContract):
		kind = ocr.ErrorCapabilityChanged
	case metrics.Requests == 0:
		kind = ocr.ErrorTransient
	}
	return &ocr.ProviderError{Kind: kind, Metrics: toOCRMetrics(metrics), Cause: err}
}

func toOCRMetrics(metrics RequestMetrics) ocr.RequestMetrics {
	return ocr.RequestMetrics{Requests: metrics.Requests, Retries: metrics.Retries, Latency: metrics.Latency}
}

package processing

import (
	"context"
	"errors"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/store"
)

// RenditionConsentState reports live consent for only the profile's rendition
// stage. Admission checks the grant again when saving the durable job.
func (service *Service) RenditionConsentState(ctx context.Context, name string) (string, error) {
	profile, err := service.mediaProcessingProfile(name)
	if err != nil {
		return "", err
	}
	_, err = service.catalog.AuthorizeProviderOperation(ctx, service.renditionConsentRequest(profile))
	return processingConsentState(err)
}

// AdmitImportedRendition queues the exact imported version as automatic work.
// Missing consent and unsupported sources leave the committed import alone.
// The watch embedding lane handles text chunks after the rendition publishes.
func (service *Service) AdmitImportedRendition(ctx context.Context, node store.Node, name string) (err error) {
	defer func() {
		state, consentErr := processingConsentState(err)
		if consentErr == nil && state != "active" || errors.Is(err, ErrSourceIneligible) ||
			errors.Is(err, store.ErrRenditionJobStaleAuthority) || errors.Is(err, store.ErrNotFound) {
			err = nil
		}
	}()
	profile, err := service.mediaProcessingProfile(name)
	if err != nil {
		return err
	}
	if _, err := service.catalog.AuthorizeProviderOperation(ctx, service.renditionConsentRequest(profile)); err != nil {
		return err
	}
	version, err := service.catalog.ContentVersionByID(ctx, node.CurrentVersionID)
	if err != nil {
		return err
	}
	_, _, err = service.enqueueRendition(ctx, node, version, profile, service.renditionConsentRequest(profile), "", false)
	return err
}

// WatchEmbeddings embeds only text chunks of versions this profile already rendered.
// Listing and execution retain the profile's existing build-consent checks.
func (service *Service) WatchEmbeddings(name string) *LocalTextSetup {
	profile, ok := service.profiles[name]
	if !ok {
		return nil
	}
	for _, binding := range profile.portable.Embeddings {
		if binding.InputKind == document.EmbeddingInputRenditionChunk {
			return &LocalTextSetup{Service: service, Selected: name, rendered: true, lane: "watch-embeddings:" + name}
		}
	}
	return nil
}

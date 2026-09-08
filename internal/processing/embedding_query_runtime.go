package processing

import (
	"context"
	"errors"
	"reflect"

	"go.kenn.io/docbank/document"
)

// QueryProvider returns the provider only when it exactly reproduces the
// requested persisted descriptor and supports text queries.
func (runtime *ProviderEmbeddingRuntime) QueryProvider(
	descriptor document.EmbeddingDescriptor,
) (document.EmbeddingProvider, error) {
	canonical, ok := snapshotQueryEmbeddingDescriptor(descriptor)
	if !ok || !canonical.SupportsTextQuery || !runtime.Ready() ||
		!reflect.DeepEqual(runtime.provider.Descriptor(), canonical) {
		return nil, ErrEmbeddingRuntimeUnavailable
	}
	return runtime.provider, nil
}

// ResolveQueryEncoder resolves only concrete provider runtimes registered for
// the exact persisted descriptor fingerprint.
func (registry *EmbeddingRuntimeRegistry) ResolveQueryEncoder(
	ctx context.Context, descriptor document.EmbeddingDescriptor,
) (document.EmbeddingProvider, error) {
	if embeddingInterfaceNil(ctx) {
		return nil, errors.New("embedding query encoder context is nil")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if registry == nil {
		return nil, ErrEmbeddingRuntimeUnavailable
	}
	registry.mu.RLock()
	runtime := registry.runtimes[descriptor.Fingerprint]
	registry.mu.RUnlock()
	providerRuntime, ok := runtime.(*ProviderEmbeddingRuntime)
	if !ok {
		return nil, ErrEmbeddingRuntimeUnavailable
	}
	provider, err := providerRuntime.QueryProvider(descriptor)
	if contextErr := ctx.Err(); contextErr != nil {
		return nil, contextErr
	}
	return provider, err
}

func snapshotQueryEmbeddingDescriptor(
	descriptor document.EmbeddingDescriptor,
) (document.EmbeddingDescriptor, bool) {
	if len(descriptor.InputKinds) < 1 || len(descriptor.InputKinds) > 2 ||
		len(descriptor.SupportedRequestModes) < 1 || len(descriptor.SupportedRequestModes) > 3 {
		return document.EmbeddingDescriptor{}, false
	}
	canonical, err := document.NewEmbeddingDescriptor(descriptor)
	if err != nil || !reflect.DeepEqual(canonical, descriptor) {
		return document.EmbeddingDescriptor{}, false
	}
	return canonical, true
}

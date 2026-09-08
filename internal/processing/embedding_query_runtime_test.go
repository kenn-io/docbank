package processing

import (
	"context"
	"errors"
	"io"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/document"
)

func TestEmbeddingRuntimeRegistryResolveQueryEncoderReturnsExactProviderWithoutExecution(t *testing.T) {
	descriptor := embeddingWorkerDescriptor(t)
	provider := &queryRuntimeProvider{descriptor: func() document.EmbeddingDescriptor { return descriptor }}
	blobs := &queryRuntimeBlobs{}
	var classifyCalls atomic.Int32
	runtime := newQueryProviderRuntime(t, provider, blobs, func(error) (EmbeddingProviderFailure, time.Duration) {
		classifyCalls.Add(1)
		return EmbeddingProviderPermanent, 0
	})
	registry := NewEmbeddingRuntimeRegistry()
	require.NoError(t, registry.Register(descriptor.Fingerprint, runtime))

	resolved, err := registry.ResolveQueryEncoder(t.Context(), descriptor)

	require.NoError(t, err)
	require.Same(t, provider, resolved)
	require.Equal(t, int32(1), provider.descriptorCalls.Load())
	require.Zero(t, provider.embedCalls.Load())
	require.Zero(t, blobs.openCalls.Load())
	require.Zero(t, classifyCalls.Load())
}

func TestProviderEmbeddingRuntimeQueryProviderRejectsDescriptorDrift(t *testing.T) {
	descriptor := embeddingWorkerDescriptor(t)
	for _, test := range []struct {
		name   string
		mutate func(*document.EmbeddingDescriptor)
	}{
		{name: "ID", mutate: func(value *document.EmbeddingDescriptor) { value.ID = "different-provider" }},
		{name: "contract version", mutate: func(value *document.EmbeddingDescriptor) { value.ContractVersion++ }},
		{name: "policy fingerprint", mutate: func(value *document.EmbeddingDescriptor) { value.PolicyFingerprint = workerHash("different-policy") }},
		{name: "trust boundary", mutate: func(value *document.EmbeddingDescriptor) { value.TrustBoundary = document.EmbeddingTrustHostedProvider }},
		{name: "model", mutate: func(value *document.EmbeddingDescriptor) { value.Model = "different-model" }},
		{name: "model revision", mutate: func(value *document.EmbeddingDescriptor) { value.ModelRevision = "different-revision" }},
		{name: "dimension", mutate: func(value *document.EmbeddingDescriptor) { value.Dimension++ }},
		{name: "metric", mutate: func(value *document.EmbeddingDescriptor) { value.Metric = document.VectorMetricDotProduct }},
		{name: "normalization", mutate: func(value *document.EmbeddingDescriptor) {
			value.Normalization = document.VectorNormalizationUnitLength
		}},
		{name: "scalar encoding", mutate: func(value *document.EmbeddingDescriptor) { value.ScalarEncoding = "float64" }},
		{name: "document formatter", mutate: func(value *document.EmbeddingDescriptor) { value.DocumentFormatter = "document/v2" }},
		{name: "query formatter", mutate: func(value *document.EmbeddingDescriptor) { value.QueryFormatter = "query/v2" }},
		{name: "input kinds", mutate: func(value *document.EmbeddingDescriptor) { value.InputKinds[0] = document.EmbeddingInputRenditionChunk }},
		{name: "compatibility ID", mutate: func(value *document.EmbeddingDescriptor) { value.CompatibilityID = "different-space" }},
		{name: "text query support", mutate: func(value *document.EmbeddingDescriptor) { value.SupportsTextQuery = false }},
		{name: "model input", mutate: func(value *document.EmbeddingDescriptor) { value.ModelInput.Query.Template = "different: {{content}}" }},
		{name: "request modes", mutate: func(value *document.EmbeddingDescriptor) {
			value.SupportedRequestModes[0] = document.ModelInputModeQuery
		}},
		{name: "fingerprint", mutate: func(value *document.EmbeddingDescriptor) { value.Fingerprint = workerHash("different-descriptor") }},
	} {
		t.Run(test.name, func(t *testing.T) {
			providerDescriptor := cloneQueryRuntimeDescriptor(descriptor)
			test.mutate(&providerDescriptor)
			provider := &queryRuntimeProvider{descriptor: func() document.EmbeddingDescriptor { return providerDescriptor }}
			runtime := newQueryProviderRuntime(t, provider, &queryRuntimeBlobs{}, queryRuntimeClassifier)

			resolved, err := runtime.QueryProvider(descriptor)

			require.ErrorIs(t, err, ErrEmbeddingRuntimeUnavailable)
			require.Nil(t, resolved)
			require.Equal(t, int32(1), provider.descriptorCalls.Load())
			require.Zero(t, provider.embedCalls.Load())
		})
	}
}

func TestProviderEmbeddingRuntimeQueryProviderRejectsInvalidRequestedDescriptorBeforeCallback(t *testing.T) {
	descriptor := embeddingWorkerDescriptor(t)
	threeModes := canonicalQueryRuntimeDescriptor(t, descriptor, func(value *document.EmbeddingDescriptor) {
		value.SupportedRequestModes = []document.ModelInputMode{
			document.ModelInputModeText, document.ModelInputModeDocument, document.ModelInputModeQuery,
		}
	})
	nonQuery := canonicalQueryRuntimeDescriptor(t, descriptor, func(value *document.EmbeddingDescriptor) {
		value.SupportsTextQuery = false
	})
	for _, test := range []struct {
		name       string
		descriptor document.EmbeddingDescriptor
	}{
		{name: "no input kinds", descriptor: changedQueryRuntimeDescriptor(descriptor, func(value *document.EmbeddingDescriptor) { value.InputKinds = nil })},
		{name: "too many input kinds", descriptor: changedQueryRuntimeDescriptor(descriptor, func(value *document.EmbeddingDescriptor) {
			value.InputKinds = []document.EmbeddingInputKind{document.EmbeddingInputOriginalFile, document.EmbeddingInputRenditionChunk, document.EmbeddingInputOriginalFile}
		})},
		{name: "input kinds out of order", descriptor: changedQueryRuntimeDescriptor(descriptor, func(value *document.EmbeddingDescriptor) {
			slices.Reverse(value.InputKinds)
		})},
		{name: "duplicate input kind", descriptor: changedQueryRuntimeDescriptor(descriptor, func(value *document.EmbeddingDescriptor) {
			value.InputKinds[1] = value.InputKinds[0]
		})},
		{name: "no request modes", descriptor: changedQueryRuntimeDescriptor(descriptor, func(value *document.EmbeddingDescriptor) { value.SupportedRequestModes = nil })},
		{name: "too many request modes", descriptor: changedQueryRuntimeDescriptor(descriptor, func(value *document.EmbeddingDescriptor) {
			value.SupportedRequestModes = []document.ModelInputMode{document.ModelInputModeDocument, document.ModelInputModeQuery, document.ModelInputModeText, document.ModelInputModeText}
		})},
		{name: "request modes out of order", descriptor: changedQueryRuntimeDescriptor(threeModes, func(value *document.EmbeddingDescriptor) {
			slices.Reverse(value.SupportedRequestModes)
		})},
		{name: "duplicate request mode", descriptor: changedQueryRuntimeDescriptor(descriptor, func(value *document.EmbeddingDescriptor) {
			value.SupportedRequestModes = []document.ModelInputMode{document.ModelInputModeText, document.ModelInputModeText}
		})},
		{name: "forged matching fingerprint", descriptor: changedQueryRuntimeDescriptor(descriptor, func(value *document.EmbeddingDescriptor) { value.Model = "forged-model" })},
		{name: "text queries unsupported", descriptor: nonQuery},
	} {
		t.Run(test.name, func(t *testing.T) {
			provider := &queryRuntimeProvider{descriptor: func() document.EmbeddingDescriptor { return test.descriptor }}
			runtime := newQueryProviderRuntime(t, provider, &queryRuntimeBlobs{}, queryRuntimeClassifier)

			resolved, err := runtime.QueryProvider(test.descriptor)

			require.ErrorIs(t, err, ErrEmbeddingRuntimeUnavailable)
			require.Nil(t, resolved)
			require.Zero(t, provider.descriptorCalls.Load())
			require.Zero(t, provider.embedCalls.Load())
		})
	}
}

func TestProviderEmbeddingRuntimeQueryProviderUsesDetachedCanonicalSnapshot(t *testing.T) {
	t.Run("matching frozen descriptor survives caller alias mutation", func(t *testing.T) {
		descriptor := embeddingWorkerDescriptor(t)
		frozen := cloneQueryRuntimeDescriptor(descriptor)
		provider := &queryRuntimeProvider{}
		provider.descriptor = func() document.EmbeddingDescriptor {
			descriptor.InputKinds[0] = document.EmbeddingInputKind("mutated")
			descriptor.SupportedRequestModes[0] = document.ModelInputMode("mutated")
			return frozen
		}
		runtime := newQueryProviderRuntime(t, provider, &queryRuntimeBlobs{}, queryRuntimeClassifier)

		resolved, err := runtime.QueryProvider(descriptor)

		require.NoError(t, err)
		require.Same(t, provider, resolved)
		require.Equal(t, document.EmbeddingInputKind("mutated"), descriptor.InputKinds[0])
		require.Equal(t, document.ModelInputMode("mutated"), descriptor.SupportedRequestModes[0])
	})

	t.Run("mutation cannot substitute different runtime", func(t *testing.T) {
		descriptor := canonicalQueryRuntimeDescriptor(t, embeddingWorkerDescriptor(t), func(value *document.EmbeddingDescriptor) {
			value.SupportedRequestModes = []document.ModelInputMode{document.ModelInputModeQuery, document.ModelInputModeText}
		})
		providerDescriptor := canonicalQueryRuntimeDescriptor(t, descriptor, func(value *document.EmbeddingDescriptor) {
			value.SupportedRequestModes = []document.ModelInputMode{document.ModelInputModeDocument, document.ModelInputModeText}
		})
		providerDescriptor.Fingerprint = descriptor.Fingerprint
		provider := &queryRuntimeProvider{}
		provider.descriptor = func() document.EmbeddingDescriptor {
			copy(descriptor.SupportedRequestModes, providerDescriptor.SupportedRequestModes)
			return providerDescriptor
		}
		runtime := newQueryProviderRuntime(t, provider, &queryRuntimeBlobs{}, queryRuntimeClassifier)

		resolved, err := runtime.QueryProvider(descriptor)

		require.ErrorIs(t, err, ErrEmbeddingRuntimeUnavailable)
		require.Nil(t, resolved)
		require.Equal(t, providerDescriptor.SupportedRequestModes, descriptor.SupportedRequestModes)
	})
}

func TestProviderEmbeddingRuntimeQueryProviderRejectsUnavailableRuntime(t *testing.T) {
	descriptor := embeddingWorkerDescriptor(t)
	var nilProvider *queryRuntimeProvider
	var typedNilProvider document.EmbeddingProvider = nilProvider
	for _, test := range []struct {
		name    string
		runtime *ProviderEmbeddingRuntime
	}{
		{name: "nil runtime"},
		{name: "typed nil provider", runtime: &ProviderEmbeddingRuntime{
			provider: typedNilProvider, blobs: &queryRuntimeBlobs{}, spoolDirectory: t.TempDir(), classify: queryRuntimeClassifier,
		}},
		{name: "missing blobs", runtime: &ProviderEmbeddingRuntime{
			provider:       &queryRuntimeProvider{descriptor: func() document.EmbeddingDescriptor { return descriptor }},
			spoolDirectory: t.TempDir(), classify: queryRuntimeClassifier,
		}},
		{name: "missing classifier", runtime: &ProviderEmbeddingRuntime{
			provider: &queryRuntimeProvider{descriptor: func() document.EmbeddingDescriptor { return descriptor }},
			blobs:    &queryRuntimeBlobs{}, spoolDirectory: t.TempDir(),
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			resolved, err := test.runtime.QueryProvider(descriptor)
			require.ErrorIs(t, err, ErrEmbeddingRuntimeUnavailable)
			require.Nil(t, resolved)
		})
	}
}

func TestEmbeddingRuntimeRegistryResolveQueryEncoderRejectsUnavailableRegistrations(t *testing.T) {
	descriptor := embeddingWorkerDescriptor(t)
	t.Run("nil registry", func(t *testing.T) {
		var registry *EmbeddingRuntimeRegistry
		resolved, err := registry.ResolveQueryEncoder(t.Context(), descriptor)
		require.ErrorIs(t, err, ErrEmbeddingRuntimeUnavailable)
		require.Nil(t, resolved)
	})
	t.Run("missing registration", func(t *testing.T) {
		resolved, err := NewEmbeddingRuntimeRegistry().ResolveQueryEncoder(t.Context(), descriptor)
		require.ErrorIs(t, err, ErrEmbeddingRuntimeUnavailable)
		require.Nil(t, resolved)
	})
	t.Run("registered non-provider runtime", func(t *testing.T) {
		registry := NewEmbeddingRuntimeRegistry()
		require.NoError(t, registry.Register(descriptor.Fingerprint, embeddingTransientClassifierRuntime{}))
		resolved, err := registry.ResolveQueryEncoder(t.Context(), descriptor)
		require.ErrorIs(t, err, ErrEmbeddingRuntimeUnavailable)
		require.Nil(t, resolved)
	})
	t.Run("typed nil provider runtime", func(t *testing.T) {
		registry := &EmbeddingRuntimeRegistry{runtimes: map[string]EmbeddingRuntime{
			descriptor.Fingerprint: (*ProviderEmbeddingRuntime)(nil),
		}}
		resolved, err := registry.ResolveQueryEncoder(t.Context(), descriptor)
		require.ErrorIs(t, err, ErrEmbeddingRuntimeUnavailable)
		require.Nil(t, resolved)
	})
}

func TestEmbeddingRuntimeRegistryResolveQueryEncoderContext(t *testing.T) {
	descriptor := embeddingWorkerDescriptor(t)
	t.Run("nil context", func(t *testing.T) {
		provider := &queryRuntimeProvider{descriptor: func() document.EmbeddingDescriptor { return descriptor }}
		registry := queryRuntimeRegistry(t, descriptor, provider)

		resolved, err := registry.ResolveQueryEncoder(nil, descriptor) //nolint:staticcheck // Nil is an explicit API contract case.

		require.EqualError(t, err, "embedding query encoder context is nil")
		require.NotErrorIs(t, err, ErrEmbeddingRuntimeUnavailable)
		require.Nil(t, resolved)
		require.Zero(t, provider.descriptorCalls.Load())
	})

	t.Run("canceled before registry access", func(t *testing.T) {
		provider := &queryRuntimeProvider{descriptor: func() document.EmbeddingDescriptor { return descriptor }}
		registry := queryRuntimeRegistry(t, descriptor, provider)
		ctx, cancel := context.WithCancel(t.Context())
		cancel()

		resolved, err := registry.ResolveQueryEncoder(ctx, descriptor)

		require.ErrorIs(t, err, context.Canceled)
		require.Nil(t, resolved)
		require.Zero(t, provider.descriptorCalls.Load())
	})

	t.Run("canceled before nil registry check", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		var registry *EmbeddingRuntimeRegistry

		resolved, err := registry.ResolveQueryEncoder(ctx, descriptor)

		require.ErrorIs(t, err, context.Canceled)
		require.Nil(t, resolved)
	})

	t.Run("canceled by successful descriptor callback", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		provider := &queryRuntimeProvider{descriptor: func() document.EmbeddingDescriptor {
			cancel()
			return descriptor
		}}
		registry := queryRuntimeRegistry(t, descriptor, provider)

		resolved, err := registry.ResolveQueryEncoder(ctx, descriptor)

		require.ErrorIs(t, err, context.Canceled)
		require.Nil(t, resolved)
		require.Equal(t, int32(1), provider.descriptorCalls.Load())
	})
}

func TestEmbeddingRuntimeRegistryReleasesReadLockBeforeProviderCallback(t *testing.T) {
	descriptor := embeddingWorkerDescriptor(t)
	registry := NewEmbeddingRuntimeRegistry()
	registrationReturned := make(chan struct{})
	var registrationErr error
	var registrationTimedOut atomic.Bool
	provider := &queryRuntimeProvider{}
	provider.descriptor = func() document.EmbeddingDescriptor {
		go func() {
			registrationErr = registry.Register(workerHash("unrelated-query-runtime"), embeddingTransientClassifierRuntime{})
			close(registrationReturned)
		}()
		select {
		case <-registrationReturned:
		case <-time.After(time.Second):
			registrationTimedOut.Store(true)
		}
		return descriptor
	}
	require.NoError(t, registry.Register(descriptor.Fingerprint,
		newQueryProviderRuntime(t, provider, &queryRuntimeBlobs{}, queryRuntimeClassifier)))

	resolved, err := registry.ResolveQueryEncoder(t.Context(), descriptor)
	select {
	case <-registrationReturned:
	case <-time.After(time.Second):
		t.Fatal("unrelated registration did not join after descriptor callback returned")
	}

	require.NoError(t, err)
	require.Same(t, provider, resolved)
	require.False(t, registrationTimedOut.Load(), "registration blocked until the callback returned")
	require.NoError(t, registrationErr)
}

type queryRuntimeProvider struct {
	descriptor      func() document.EmbeddingDescriptor
	descriptorCalls atomic.Int32
	embedCalls      atomic.Int32
}

func (provider *queryRuntimeProvider) Descriptor() document.EmbeddingDescriptor {
	provider.descriptorCalls.Add(1)
	return provider.descriptor()
}

func (provider *queryRuntimeProvider) Embed(
	context.Context, []document.EmbeddingInput, document.EmbeddingAuthorization,
) (document.EmbeddingResult, error) {
	provider.embedCalls.Add(1)
	return document.EmbeddingResult{}, errors.New("query resolution must not execute provider")
}

type queryRuntimeBlobs struct {
	openCalls atomic.Int32
}

func (blobs *queryRuntimeBlobs) OpenContext(context.Context, string) (io.ReadSeekCloser, error) {
	blobs.openCalls.Add(1)
	return nil, errors.New("query resolution must not open blobs")
}

func newQueryProviderRuntime(
	t *testing.T,
	provider document.EmbeddingProvider,
	blobs embeddingRuntimeBlobs,
	classify func(error) (EmbeddingProviderFailure, time.Duration),
) *ProviderEmbeddingRuntime {
	t.Helper()
	runtime, err := NewProviderEmbeddingRuntime(provider, blobs, t.TempDir(), classify)
	require.NoError(t, err)
	return runtime
}

func queryRuntimeClassifier(error) (EmbeddingProviderFailure, time.Duration) {
	return EmbeddingProviderPermanent, 0
}

func queryRuntimeRegistry(
	t *testing.T, descriptor document.EmbeddingDescriptor, provider document.EmbeddingProvider,
) *EmbeddingRuntimeRegistry {
	t.Helper()
	registry := NewEmbeddingRuntimeRegistry()
	require.NoError(t, registry.Register(descriptor.Fingerprint,
		newQueryProviderRuntime(t, provider, &queryRuntimeBlobs{}, queryRuntimeClassifier)))
	return registry
}

func cloneQueryRuntimeDescriptor(value document.EmbeddingDescriptor) document.EmbeddingDescriptor {
	value.InputKinds = slices.Clone(value.InputKinds)
	value.SupportedRequestModes = slices.Clone(value.SupportedRequestModes)
	return value
}

func changedQueryRuntimeDescriptor(
	value document.EmbeddingDescriptor, mutate func(*document.EmbeddingDescriptor),
) document.EmbeddingDescriptor {
	value = cloneQueryRuntimeDescriptor(value)
	mutate(&value)
	return value
}

func canonicalQueryRuntimeDescriptor(
	t *testing.T, value document.EmbeddingDescriptor, mutate func(*document.EmbeddingDescriptor),
) document.EmbeddingDescriptor {
	t.Helper()
	value = changedQueryRuntimeDescriptor(value, mutate)
	canonical, err := document.NewEmbeddingDescriptor(value)
	require.NoError(t, err)
	return canonical
}

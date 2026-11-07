package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/docling"
	"go.kenn.io/docbank/document/workerprotocol"
)

type acceptingResumableRenditionProvider struct {
	descriptor document.RenditionDescriptor
	calls      int
}

func (provider *acceptingResumableRenditionProvider) Descriptor() document.RenditionDescriptor {
	return provider.descriptor
}

func (*acceptingResumableRenditionProvider) Render(
	context.Context, document.AuthorizedUpload, document.RenditionAuthorization,
) (document.RenditionResult, error) {
	return document.RenditionResult{}, nil
}

func (provider *acceptingResumableRenditionProvider) RenderResumable(
	context.Context, document.AuthorizedUpload, document.RenditionAuthorization,
	*document.RenditionResumeHandle, document.RenditionResumeCheckpoint,
) (document.RenditionResult, error) {
	provider.calls++
	return document.RenditionResult{}, nil
}

func TestConfiguredWorkerPreservesResumeAndAllowlist(t *testing.T) {
	cfg, _ := doclingDocumentProcessingConfig(t, "http://127.0.0.1:5002")
	descriptor, err := docling.WorkerDocumentDescriptor()
	require.NoError(t, err)
	profile := cfg.RenditionProfiles["documents"]
	profile.AdapterContract = workerprotocol.DocumentAdapter
	profile.DescriptorID = descriptor.ID
	profile.DescriptorFingerprint = descriptor.Fingerprint
	profile.DisclosureFingerprint = docling.WorkerDisclosureFingerprint(profile.AdapterContract, descriptor, profile.Runtime.Endpoint, profile.DeploymentFingerprint)
	cfg.RenditionProfiles["documents"] = profile
	providers, _, err := configureRenditionProviders(cfg)
	require.NoError(t, err)
	provider, ok := providers["documents"].(document.ResumableRenditionProvider)
	require.True(t, ok, "configured wrapper must preserve the optional interface")
	configured, ok := providers["documents"].(*configuredResumableRenditionProvider)
	require.True(t, ok)
	delegate := &acceptingResumableRenditionProvider{descriptor: descriptor}
	configured.resumable = delegate
	_, err = provider.RenderResumable(t.Context(), nil, document.RenditionAuthorization{RenditionRequestFingerprint: strings.Repeat("0", 64)}, nil, nil)
	require.Error(t, err)
	failure, ok := errors.AsType[*document.RenditionProviderError](err)
	require.True(t, ok)
	require.Equal(t, document.RenditionErrorPolicyRejected, failure.Code())
	require.Zero(t, delegate.calls, "the wrapper must reject unallowlisted requests before delegation")

	resolved, err := cfg.ProcessingProfile("documents")
	require.NoError(t, err)
	_, fingerprints, err := document.CanonicalProfile(resolved.Document)
	require.NoError(t, err)
	_, err = provider.RenderResumable(t.Context(), nil,
		document.RenditionAuthorization{RenditionRequestFingerprint: fingerprints.RenditionRequest}, nil, nil)
	require.NoError(t, err)
	require.Equal(t, 1, delegate.calls, "an allowlisted request must reach the resumable delegate")
	native, _ := doclingDocumentProcessingConfig(t, "http://127.0.0.1:5001")
	providers, _, err = configureRenditionProviders(native)
	require.NoError(t, err)
	_, ok = providers["documents"].(document.ResumableRenditionProvider)
	require.False(t, ok, "native clients must not acquire an unqualified durable protocol")
}

package store

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/docling"
	"go.kenn.io/docbank/document/workerprotocol"
)

func checkWorkerRenditionReplay(t *testing.T, open func(*testing.T) *Store) {
	t.Helper()
	for _, kind := range []string{"worker", "wrong-adapter", "wrong-policy", "wrong-boundary", "native"} {
		t.Run(kind, func(t *testing.T) {
			descriptor, err := docling.WorkerDocumentDescriptor()
			require.NoError(t, err)
			adapter := workerprotocol.DocumentAdapter
			boundary := string(document.RenditionTrustOperatorNetwork)
			if kind == "native" {
				descriptor, err = docling.DocumentDescriptor(document.RenditionTrustOperatorNetwork)
				require.NoError(t, err)
				adapter = docling.DocumentAdapterContract
			}
			if kind == "wrong-adapter" {
				adapter = docling.DocumentAdapterContract
			}
			if kind == "wrong-boundary" {
				boundary = string(document.RenditionTrustHostedProvider)
			}
			profile := catalogProcessingProfileWith(t, false, func(p *document.ProcessingProfileV1) {
				p.Rendition.AdapterContract = adapter
				p.Rendition.Descriptor = document.ProviderDescriptorV1{ID: descriptor.ID, Fingerprint: descriptor.Fingerprint}
				p.Rendition.TrustBoundary = boundary
				p.RetentionDisclosure.TrustBoundary = boundary
				p.Rendition.RequestedArtifacts = []document.EvidenceArtifactRole{document.EvidenceArtifactMarkdown, document.EvidenceArtifactStructured}
			})
			s := open(t)
			versions := seedRenditionCatalogVersions(t, s)
			request := renditionJobTestRequest(versions[0], profile)
			request.ExecutionIdentity.Authorization.ProviderID = descriptor.ID
			request.ExecutionIdentity.Authorization.DescriptorFingerprint = descriptor.Fingerprint
			request.ExecutionIdentity.Authorization.PolicyFingerprint = descriptor.PolicyFingerprint
			request.ExecutionIdentity.Authorization.MaxProviderMarkdownBytes = 1000
			if kind == "wrong-policy" {
				request.ExecutionIdentity.Authorization.PolicyFingerprint = strings.Repeat("a", 64)
			}
			grantRenditionJobConsent(t, s, request)
			job, waiter, err := s.EnqueueRenditionJob(t.Context(), request)
			require.NoError(t, err)
			at := time.Now().UTC()
			claim, err := s.ClaimRenditionJob(t.Context(), job.ID, "worker-before-crash", at, time.Minute)
			require.NoError(t, err)
			_, err = s.BeginRenditionProvider(t.Context(), claim, waiter.ID, at, renditionJobTestSnapshot(request))
			require.NoError(t, err)
			recovered, err := s.ClaimRenditionJob(t.Context(), job.ID, "worker-after-crash", at.Add(2*time.Minute), time.Minute)
			if kind != "worker" {
				require.ErrorIs(t, err, ErrRenditionJobOperatorRequired)
				return
			}
			require.NoError(t, err)
			require.Empty(t, recovered.ResumeHandle)
			require.Greater(t, recovered.Epoch, claim.Epoch)
			work, err := s.RenditionJobWorkByClaim(t.Context(), recovered, at.Add(2*time.Minute))
			require.NoError(t, err)
			require.Nil(t, work.ExecutionSnapshot)
			require.Equal(t, RenditionPhaseQueued, work.Job.Phase)
			require.ErrorIs(t, s.CheckpointRenditionProvider(t.Context(), claim, "stale-handle", at.Add(2*time.Minute)), ErrRenditionJobFenced)
		})
	}
}

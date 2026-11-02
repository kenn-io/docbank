package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
)

func TestEmbeddingReconciliationReportsTransientImagePreparation(t *testing.T) {
	t.Parallel()
	for _, remembered := range []bool{false, true} {
		t.Run(map[bool]string{false: "transient", true: "verified rejection"}[remembered], func(t *testing.T) {
			t.Parallel()
			f := newImageEmbeddingCatalogFixture(t)
			request := EmbeddingReconcileRequest{
				Mutate: embeddingTestMutation, At: time.Now(), Limit: 100, SkipGenerations: true,
				ImageProfiles:          []ProcessingProfileRecord{f.request.Profile},
				DescriptorFingerprints: []string{f.request.Descriptor.Fingerprint},
				ImageRejections:        make(map[ImagePreparationRejectionKey]struct{}),
				PrepareImageGeneration: func(context.Context, ProcessingProfileRecord, document.EmbeddingBindingV1, string) (EmbeddingInputGenerationRecord, document.EmbeddingDescriptor, error) {
					return EmbeddingInputGenerationRecord{}, document.EmbeddingDescriptor{}, &ImagePreparationError{Rejected: remembered, EvidenceFingerprint: f.request.InputGeneration.EvidenceFingerprint, Err: errors.New("synthetic preparation failure")}
				},
			}
			result, err := f.s.ReconcileEmbeddingJobs(t.Context(), request)
			require.NoError(t, err)
			if remembered {
				require.Zero(t, result.Skipped)
			} else {
				require.Equal(t, 2, result.Skipped)
			}
		})
	}
}

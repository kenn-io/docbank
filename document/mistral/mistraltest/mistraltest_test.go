package mistraltest_test

import (
	"bytes"
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/mistral"
	"go.kenn.io/docbank/document/mistral/mistraltest"
)

func TestSyntheticManifestAuthorizesPDF(t *testing.T) {
	normalization, err := document.NewNormalizePolicy(100_000)
	require.NoError(t, err)
	policy, err := mistral.NewPolicy(mistral.PolicyConfig{
		Region: mistral.RegionEU, Model: mistral.DefaultModel,
		Retention: mistral.RetentionZDR, Training: mistral.TrainingOptedOut,
		MaxDocumentBytes: 1024, MaxResponseBytes: 1 << 20, MaxUnits: 10,
		ExtractHeader: true, ExtractFooter: true, NormalizePolicy: normalization,
	})
	require.NoError(t, err)

	manifest, err := mistraltest.SyntheticManifest(policy, true)
	require.NoError(t, err)
	authorization, err := policy.Authorize(manifest, "pdf")
	require.NoError(t, err)
	assert.Equal(t, "pdf", authorization.Format().ID)

	pdf := mistraltest.MinimalPDF("synthetic")
	format, err := mistral.DetectFormat(bytes.NewReader(pdf), int64(len(pdf)), "application/pdf")
	require.NoError(t, err)
	assert.Equal(t, "pdf", format.ID)
}

func TestRetryClassification(t *testing.T) {
	assert.True(t, mistral.IsRetryable(fmt.Errorf("provider: %w", mistral.ErrTransientResponse)))
	assert.True(t, mistral.IsRetryable(fmt.Errorf("spool: %w", mistral.ErrSpoolCapacity)))
	assert.False(t, mistral.IsRetryable(mistral.ErrPermanentResponse))
	assert.False(t, mistral.IsRetryable(errors.New("application failure")))
}

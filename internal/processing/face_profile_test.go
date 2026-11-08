package processing

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/config"
)

func TestFaceProfileRequiresDedicatedSetup(t *testing.T) {
	t.Parallel()
	x := newFaceEmbeddingFixture(t, false)
	selector := x.f.selector(config.LocalFacesProfile)
	plan, err := x.service.Plan(t.Context(), selector)
	require.ErrorIs(t, err, ErrProfileNotConfigured)
	_, err = x.service.Start(t.Context(), StartRequest{Selector: selector, PlanFingerprint: plan.Fingerprint, Consent: true})
	require.ErrorIs(t, err, ErrProfileNotConfigured)
	_, err = x.service.GrantConsent(t.Context(), ConsentGrantRequest{Selector: selector, PlanFingerprint: plan.Fingerprint})
	require.ErrorIs(t, err, ErrProfileNotConfigured)
	for _, profile := range x.service.Profiles() {
		assert.NotEqual(t, config.LocalFacesProfile, profile.Name, "file processing must only list per-file profiles")
	}
}

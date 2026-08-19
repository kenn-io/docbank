//go:build voyage_probe

package voyage_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/document/media"
	"go.kenn.io/docbank/document/voyage"
)

// TestLiveCapabilityProbe runs the authenticated probe against the real
// provider. It requires VOYAGE_API_KEY and VOYAGE_PROBE_SEED_DIR (synthetic
// WebP and MP4 seeds). With a key but no seeds it fails closed rather than
// probing a partial matrix. It stores no media, vectors, or responses; the
// manifest is written to the test's temporary directory only.
func TestLiveCapabilityProbe(t *testing.T) {
	apiKey := os.Getenv("VOYAGE_API_KEY")
	if apiKey == "" {
		t.Skip("VOYAGE_API_KEY is not set")
	}
	seeds := os.Getenv("VOYAGE_PROBE_SEED_DIR")
	require.NotEmpty(t, seeds, "VOYAGE_PROBE_SEED_DIR must name a directory with synthetic webp and mp4 seeds")

	policy, err := voyage.NewPolicy(voyage.PolicyConfig{Media: media.DefaultPolicy()})
	require.NoError(t, err)
	fixtures := filepath.Join(privateTempDir(t), "fixtures")
	require.NoError(t, voyage.WriteProbeFixtures(t.Context(), fixtures, voyage.FixtureOptions{SeedDirectory: seeds}))
	require.NoError(t, voyage.ValidateProbeFixtures(t.Context(), policy, voyage.ProbeFixtureConfig{FixtureDirectory: fixtures}))

	client, err := voyage.NewClient(policy, voyage.ClientConfig{APIKey: apiKey})
	require.NoError(t, err)
	manifest, err := voyage.RunCapabilityProbe(t.Context(), client, voyage.ProbeConfig{
		Fixtures: voyage.ProbeFixtureConfig{FixtureDirectory: fixtures},
	})
	require.NoError(t, err)

	out, err := os.Create(filepath.Join(t.TempDir(), "voyage-capabilities.json"))
	require.NoError(t, err)
	defer func() { _ = out.Close() }()
	require.NoError(t, voyage.EncodeCapabilityManifest(out, manifest))
	for _, result := range manifest.Results {
		t.Logf("%s: %s %s", result.CapabilityID, result.Status, result.ReasonCode)
	}
}

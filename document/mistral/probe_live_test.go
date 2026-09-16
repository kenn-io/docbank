//go:build mistral_probe

package mistral

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
)

// TestLiveCapabilityProbeBoundsTextFormats records sanitized local/provider
// count pairs for the selected text candidates. It uses only temporary,
// synthetic inputs and the same private processing route as the full probe.
func TestLiveCapabilityProbeBoundsTextFormats(t *testing.T) {
	apiKey := os.Getenv("MISTRAL_API_KEY")
	require.NotEmpty(t, apiKey, "MISTRAL_API_KEY must be set for the explicit owner proof")

	normalization, err := document.NewNormalizePolicy(100_000)
	require.NoError(t, err)
	policy, err := NewPolicy(PolicyConfig{
		Region: RegionEU, Model: DefaultModel, Retention: RetentionZDR,
		Training: TrainingOptedOut, MaxDocumentBytes: 1 << 20,
		MaxResponseBytes: 1 << 20, MaxUnits: MaxUnits, ExtractHeader: true,
		ExtractFooter: true, NormalizePolicy: normalization,
	})
	require.NoError(t, err)
	client, err := NewClient(policy, ClientConfig{
		APIKey: apiKey, Timeout: 2 * time.Minute, MaxRetries: 1,
		MaxRetryDelay: time.Second,
	})
	require.NoError(t, err)

	for _, formatID := range []string{"json", "eml"} {
		candidate, ok := CandidateFormatByID(formatID)
		if !ok || localUnitCounters[candidate.ID] == nil {
			continue
		}
		variants, ok := textProbeVariants(candidate.ID)
		if !ok {
			continue
		}
		for _, variant := range variants {
			localUnits, providerUnits := runTextProbeVariant(t, client, policy, candidate, variant)
			t.Logf("format=%s variant=%s local=%d provider=%d", candidate.ID, variant.name, localUnits, providerUnits)
		}
	}
}

type textProbeVariant struct {
	name    string
	content []byte
}

func textProbeVariants(formatID string) ([]textProbeVariant, bool) {
	sentinel, err := ProbeFixtureSentinel(formatID)
	if err != nil {
		return nil, false
	}
	primary, generated, err := generatedFixture(formatID)
	if err != nil || !generated {
		return nil, false
	}
	variants := []textProbeVariant{{name: "fixture", content: primary}}
	switch formatID {
	case "json":
		variants = append(variants, textProbeVariant{name: "pretty", content: []byte("{\n  \"items\": [1, 2, 3],\n  \"sentinel\": \"" + sentinel + "\"\n}\n")})
	case "eml":
		variants = append(variants, textProbeVariant{name: "multipart", content: []byte("From: probe@example.test\r\nTo: archive@example.test\r\nDate: Thu, 13 Aug 2026 00:00:00 +0000\r\nSubject: Synthetic multipart\r\nMIME-Version: 1.0\r\nContent-Type: multipart/mixed; boundary=docbank\r\n\r\n--docbank\r\nContent-Type: text/plain; charset=utf-8\r\n\r\n" + sentinel + "\r\n--docbank\r\nContent-Type: message/rfc822\r\n\r\nFrom: nested@example.test\r\nDate: Thu, 13 Aug 2026 00:00:00 +0000\r\n\r\nnested\r\n--docbank--\r\n")})
	}
	return variants, true
}

func runTextProbeVariant(
	t *testing.T,
	client *Client,
	policy Policy,
	candidate CandidateFormat,
	variant textProbeVariant,
) (int, int) {
	t.Helper()
	digest := sha256.Sum256(variant.content)
	directory := filepath.Join(t.TempDir(), "spool")
	makePrivateDirectory(t, directory)
	prepared, err := Prepare(t.Context(), io.NopCloser(bytes.NewReader(variant.content)), policy, PrepareOptions{
		Directory: directory, DeclaredMediaType: candidate.MediaType,
		ExpectedSize: int64(len(variant.content)), ExpectedSHA256: hex.EncodeToString(digest[:]),
		MaxSpoolBytes: policy.values.MaxDocumentBytes, MinFreeBytes: 1,
	})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, prepared.Release()) })
	snapshot, err := prepared.snapshot()
	require.NoError(t, err)
	options := probeRequestOptions(candidate, policy.values.MaxUnits,
		policy.values.ExtractHeader, policy.values.ExtractFooter)
	result, err := client.process(t.Context(), func() (preparedSnapshot, error) {
		return prepared.snapshot()
	}, options, UnitBoundNone, policy.values.MaxUnits)
	require.NoError(t, err)
	require.Positive(t, snapshot.localUnits)
	require.Positive(t, result.UnitsProcessed)
	require.Equal(t, snapshot.localUnits, result.UnitsProcessed)
	return snapshot.localUnits, result.UnitsProcessed
}

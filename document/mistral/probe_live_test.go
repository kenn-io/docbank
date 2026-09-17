//go:build mistral_probe

package mistral

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
)

// TestLiveCapabilityProbeBoundsTextFormats records sanitized provider outcomes
// for synthetic variants. The observations never change production authority.
func TestLiveCapabilityProbeBoundsTextFormats(t *testing.T) {
	apiKey := os.Getenv("MISTRAL_API_KEY")
	if apiKey == "" {
		t.Skip("MISTRAL_API_KEY is not configured")
	}

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

	msgFixture := loadOptionalMSGFixture(t)
	for _, formatID := range textProbeFormatIDs() {
		candidate, ok := CandidateFormatByID(formatID)
		if !ok {
			continue
		}
		if formatID == "msg" && len(msgFixture) == 0 {
			t.Logf("format=%s variant=fixture outcome=skip local=0 provider=0", formatID)
			continue
		}
		variants, ok := textProbeVariants(formatID, msgFixture)
		if !ok {
			continue
		}
		for _, variant := range variants {
			localUnits, providerUnits, outcome := runTextProbeVariant(t, client, policy, candidate, variant)
			t.Logf("format=%s variant=%s outcome=%s local=%d provider=%d", candidate.ID, variant.name, outcome, localUnits, providerUnits)
		}
	}
}

type textProbeVariant struct {
	name    string
	content []byte
}

func textProbeFormatIDs() []string {
	return []string{"txt", "markdown", "csv", "json", "jsonl", "yaml", "go", "python", "javascript", "rst", "latex", "xml", "eml", "msg"}
}

func textProbeVariants(formatID string, msgFixture []byte) ([]textProbeVariant, bool) {
	var primary []byte
	if formatID == "msg" {
		primary = msgFixture
	} else {
		var generated bool
		var err error
		primary, generated, err = generatedFixture(formatID)
		if err != nil || !generated {
			return nil, false
		}
	}
	if len(primary) == 0 {
		return nil, false
	}
	variants := []textProbeVariant{{name: "fixture", content: primary}}
	sentinel, _ := ProbeFixtureSentinel(formatID)
	switch formatID {
	case "txt", "markdown", "go", "python", "javascript", "rst":
		variants = append(variants,
			textProbeVariant{name: "terminated", content: lineVariant(formatID, "alpha\nbeta\n")},
			textProbeVariant{name: "unterminated", content: lineVariant(formatID, "alpha\nbeta")},
		)
	case "latex":
		variants = append(variants,
			textProbeVariant{name: "terminated", content: lineVariant(formatID, "alpha\nbeta\n")},
			textProbeVariant{name: "unterminated", content: lineVariant(formatID, "alpha\nbeta")},
		)
	case "csv":
		variants = append(variants,
			textProbeVariant{name: "records", content: []byte("name,value\nalpha,1\nbeta,2\n")},
			textProbeVariant{name: "quoted-newline", content: []byte("name,value\n\"alpha\nbeta\",1\nc,2")},
		)
	case "json":
		variants = append(variants,
			textProbeVariant{name: "pretty", content: []byte("{\n  \"items\": [1, 2, 3],\n  \"sentinel\": \"" + sentinel + "\"\n}\n")},
			textProbeVariant{name: "array", content: []byte("[\"" + sentinel + "\", 1, true, null]\n")},
			textProbeVariant{name: "scalar", content: []byte("7319\n")},
			textProbeVariant{name: "large", content: []byte("{\"sentinel\":\"" + sentinel + "\",\"body\":\"" + strings.Repeat("x", 100_000) + "\"}\n")},
			textProbeVariant{name: "deep", content: []byte(strings.Repeat("[", 64) + "\"" + sentinel + "\"" + strings.Repeat("]", 64) + "\n")},
		)
	case "jsonl":
		variants = append(variants,
			textProbeVariant{name: "records", content: []byte("{\"a\":1}\n\n[2]\ntrue")},
		)
	case "yaml":
		variants = append(variants,
			textProbeVariant{name: "documents", content: []byte("---\na: 1\n---\nb: 2\n")},
		)
	case "xml":
		variants = append(variants,
			textProbeVariant{name: "alternate-root", content: []byte("<?xml version=\"1.0\"?><alternate><item/></alternate>")},
		)
	case "eml":
		variants = append(variants,
			textProbeVariant{name: "multipart", content: []byte("From: probe@example.test\r\nTo: archive@example.test\r\nDate: Thu, 13 Aug 2026 00:00:00 +0000\r\nSubject: Synthetic multipart\r\nMIME-Version: 1.0\r\nContent-Type: multipart/mixed; boundary=docbank\r\n\r\n--docbank\r\nContent-Type: text/plain; charset=utf-8\r\n\r\n" + sentinel + "\r\n--docbank\r\nContent-Type: message/rfc822\r\n\r\nFrom: nested@example.test\r\nDate: Thu, 13 Aug 2026 00:00:00 +0000\r\n\r\nnested\r\n--docbank--\r\n")},
			textProbeVariant{name: "long", content: []byte("From: probe@example.test\r\nTo: archive@example.test\r\nDate: Thu, 13 Aug 2026 00:00:00 +0000\r\nSubject: Synthetic long message\r\nMIME-Version: 1.0\r\nContent-Type: text/plain; charset=utf-8\r\n\r\n" + sentinel + "\r\n" + strings.Repeat("long synthetic body ", 5_000) + "\r\n")},
		)
	}
	return variants, true
}

func lineVariant(formatID, body string) []byte {
	if formatID == "latex" {
		return []byte("\\documentclass{article}\n\\begin{document}\n" + body + "\\end{document}\n")
	}
	return []byte(body)
}

func loadOptionalMSGFixture(t *testing.T) []byte {
	t.Helper()
	seedDirectory := os.Getenv("MISTRAL_PROBE_SEED_DIR")
	if seedDirectory == "" {
		return nil
	}
	fixtureDirectory := newProbeFixtureDestination(t, "live-fixtures")
	if err := WriteProbeFixtures(t.Context(), fixtureDirectory, FixtureOptions{SeedDirectory: seedDirectory}); err != nil {
		return nil
	}
	fixture, err := os.ReadFile(filepath.Join(fixtureDirectory, "msg"))
	if err != nil {
		return nil
	}
	return fixture
}

func runTextProbeVariant(
	t *testing.T,
	client *Client,
	policy Policy,
	candidate CandidateFormat,
	variant textProbeVariant,
) (int, int, string) {
	t.Helper()
	localUnits, err := countLocalUnits(candidate, bytes.NewReader(variant.content), int64(len(variant.content)))
	require.NoError(t, err)
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
	options := probeRequestOptions(candidate, policy.values.MaxUnits,
		policy.values.ExtractHeader, policy.values.ExtractFooter)
	result, err := client.process(t.Context(), func() (preparedSnapshot, error) {
		return prepared.snapshot()
	}, options, UnitBoundNone, policy.values.MaxUnits)
	if err != nil {
		if errors.Is(err, ErrPermanentResponse) {
			return localUnits, 0, "reject"
		}
		return localUnits, 0, "error"
	}
	return localUnits, result.UnitsProcessed, "pass"
}

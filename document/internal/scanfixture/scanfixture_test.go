package scanfixture_test

import (
	"bytes"
	"crypto/sha256"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document/internal/formatdetect"
	"go.kenn.io/docbank/document/internal/scanfixture"
	"golang.org/x/image/webp"
)

func TestCorpusIsCompleteStableAndPageAccurate(t *testing.T) {
	corpus := scanfixture.Corpus()
	names := make([]string, 0, len(corpus))
	for _, fixture := range corpus {
		names = append(names, fixture.Name)
	}
	assert.Equal(t, []string{
		"blank", "born-digital", "encrypted", "image-only", "low-quality", "malformed",
		"mixed-above-ratio", "mixed-below-ratio", "oversized", "rotated",
		"single-image-jpeg", "single-image-png", "single-image-webp",
		"undecodable-font", "unsupported-filter", "unsupported-format",
	}, names)
	pageCounts := map[string]int64{
		"blank": 1, "born-digital": 3, "image-only": 3, "low-quality": 1,
		"mixed-above-ratio": 10, "mixed-below-ratio": 2, "rotated": 1,
		"undecodable-font": 1, "unsupported-filter": 1,
	}
	generated := 0
	for _, fixture := range corpus {
		if fixture.Generated {
			generated++
			assert.Nil(t, fixture.Bytes, fixture.Name)
			continue
		}
		require.NotEmpty(t, fixture.Bytes, fixture.Name)
		if pageCounts[fixture.Name] == 0 {
			continue
		}
		pages, err := formatdetect.CountPDFPages(fixture.Bytes)
		require.NoError(t, err, fixture.Name)
		assert.Equal(t, pageCounts[fixture.Name], pages, fixture.Name)
	}
	assert.Equal(t, 1, generated, "oversized is the only generated fixture")

	first, second := scanfixture.Corpus(), scanfixture.Corpus()
	for index := range first {
		if first[index].Generated {
			continue
		}
		assert.Equal(t, first[index].Bytes, second[index].Bytes, first[index].Name)
	}

	oversized := scanfixture.OversizedBytes()
	assert.Len(t, oversized, 134217729)
	t.Logf("oversized SHA-256: %x", sha256.Sum256(oversized))
	assert.Contains(t, string(oversized[len(oversized)-1024:]), "startxref")
	pages, err := formatdetect.CountPDFPages(oversized)
	require.NoError(t, err)
	assert.Equal(t, int64(2), pages)
}

// Replacing either frozen seed with a merely signature-valid header breaks
// the real parser boundary this assessment is meant to exercise.
func TestFrozenSeedsAreParserValid(t *testing.T) {
	var encrypted, webpFixture scanfixture.Fixture
	for _, fixture := range scanfixture.Corpus() {
		switch fixture.Name {
		case "encrypted":
			encrypted = fixture
		case "single-image-webp":
			webpFixture = fixture
		}
	}

	require.NotEmpty(t, encrypted.Bytes)
	_, err := formatdetect.CountPDFPages(encrypted.Bytes)
	require.ErrorIs(t, err, formatdetect.ErrPDFEncrypted)

	require.NotEmpty(t, webpFixture.Bytes)
	decoded, err := webp.Decode(bytes.NewReader(webpFixture.Bytes))
	require.NoError(t, err)
	assert.Equal(t, 8, decoded.Bounds().Dx())
	assert.Equal(t, 8, decoded.Bounds().Dy())
}

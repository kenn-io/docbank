package scanfixture

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestMeasureLabelsSkippedInputs(t *testing.T) {
	for _, input := range []Fixture{
		{Name: "image", MediaType: "image/png"},
		{Name: "oversized", MediaType: mediaTypePDF, Generated: true},
	} {
		require.NotEmpty(t, Measure(input, 128<<20).Skipped, input.Name)
	}
}

func TestMeasureUsesTheSourceBoundForStreamLimits(t *testing.T) {
	input := Fixture{
		Name: "bounded", MediaType: mediaTypePDF,
		Bytes: textPDF(1, "Synthetic text that exceeds a small configured stream bound."),
	}
	got := Measure(input, 16)
	withRoom := Measure(input, 128<<20)
	require.Equal(t, int64(1), got.Pages)
	require.NotEmpty(t, got.InspectError)
	require.Empty(t, withRoom.InspectError)
}

func TestMeasureRecordsOpaqueStreamsWithoutCallingThemScans(t *testing.T) {
	var input Fixture
	for _, fixture := range Corpus() {
		if fixture.Name == "image-only" {
			input = fixture
		}
	}
	require.NotEmpty(t, input.Bytes)
	got := Measure(input, 128<<20)
	require.Equal(t, int64(3), got.Pages)
	require.True(t, got.Unbounded)
	require.NotEmpty(t, got.InspectError)
}

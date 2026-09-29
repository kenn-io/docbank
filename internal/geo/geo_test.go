package geo_test

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/internal/geo"
)

func TestNaturalEarthResolveKnownCities(t *testing.T) {
	r := require.New(t)
	g, err := geo.NewNaturalEarth()
	r.NoError(err)

	type tc struct {
		name           string
		lat, lon       float64
		mustContain    []string
		mustNotContain []string
	}
	cases := []tc{
		{"Paris", 48.8566, 2.3522, []string{"France"}, nil},
		// "Manhattan" is a borough, not a populated_places city, so it
		// must NEVER appear in the label — locks the field-selection
		// contract (NAMEASCII → NAME, not e.g. an ADM2-style sub-name).
		{"NYC", 40.7128, -74.0060, []string{"New York", "United States"}, []string{"Manhattan"}},
		{"Tokyo", 35.6762, 139.6503, []string{"Japan"}, nil},
		{"Sydney", -33.8688, 151.2093, []string{"Australia"}, nil},
		{"Cape Town", -33.9249, 18.4241, []string{"South Africa"}, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := require.New(t)
			label, ok := g.Resolve(c.lat, c.lon)
			r.True(ok, "expected resolve to succeed; got label=%q ok=%v", label, ok)
			for _, sub := range c.mustContain {
				r.Contains(label, sub, "label %q missing %q", label, sub)
			}
			for _, sub := range c.mustNotContain {
				r.NotContains(label, sub,
					"label %q unexpectedly contains %q", label, sub)
			}
		})
	}
}

// TestNaturalEarthCoordOrderFootgun guards against the silent
// orb.Point{lon, lat} vs. orb.Point{lat, lon} bug. Resolving with the
// arguments swapped MUST NOT produce a Paris-shaped label — Paris's
// (lat=48.8566, lon=2.3522) swapped becomes (lat=2.3522, lon=48.8566)
// which lands in the Indian Ocean / Somalia.
func TestNaturalEarthCoordOrderFootgun(t *testing.T) {
	r := require.New(t)
	g, err := geo.NewNaturalEarth()
	r.NoError(err)
	label, _ := g.Resolve(2.3522, 48.8566)
	r.NotContains(label, "France",
		"swapped Paris coords resolved to a France-shaped label %q — "+
			"orb.Point construction order is wrong", label)
	r.NotContains(label, "Paris", "label=%q", label)
}

func TestNaturalEarthOpenOceanReturnsFalse(t *testing.T) {
	r := require.New(t)
	g, err := geo.NewNaturalEarth()
	r.NoError(err)
	_, ok := g.Resolve(0, -30) // mid-Atlantic
	r.False(ok)
}

func TestNaturalEarthSouthPole(t *testing.T) {
	r := require.New(t)
	g, err := geo.NewNaturalEarth()
	r.NoError(err)
	label, ok := g.Resolve(-89.9, 0)
	r.True(ok)
	r.Contains(label, "Antarctica")
}

func TestNaturalEarthAntimeridian(t *testing.T) {
	r := require.New(t)
	g, err := geo.NewNaturalEarth()
	r.NoError(err)

	// Russian Far East: Petropavlovsk-Kamchatsky-ish.
	label, ok := g.Resolve(53.0, 158.7)
	r.True(ok)
	r.Contains(label, "Russia")

	// Suva, Fiji — straddles antimeridian as a country, but Suva itself
	// is at lon ≈ 178.4 (just west of 180).
	label, ok = g.Resolve(-18.1416, 178.4419)
	r.True(ok)
	r.Contains(label, "Fiji")
}

// TestNaturalEarthSameCountryGate makes sure the city-threshold gate
// drops a populated-place from a different country than the resolved
// admin_0. Pick a coord just over a border: e.g., a point inside Mexico
// near the US border should NOT pull in El Paso; the label should be
// region+country (Mexico), not "El Paso, Texas, United States".
func TestNaturalEarthSameCountryGate(t *testing.T) {
	r := require.New(t)
	g, err := geo.NewNaturalEarth()
	r.NoError(err)

	// Ciudad Juárez sits right across from El Paso, TX. The resolved
	// label must contain "Mexico" and must NOT contain "United States".
	label, ok := g.Resolve(31.6904, -106.4245)
	r.True(ok)
	r.Contains(label, "Mexico")
	r.NotContains(label, "United States")
}

// TestEmbeddedDataChecksums verifies the three embedded GeoJSON files
// match the SHA256s in PROVENANCE.md. Catches accidental re-vendoring
// of a different release — see Task 1.
func TestEmbeddedDataChecksums(t *testing.T) {
	r := require.New(t)
	expected := map[string]string{
		"ne_10m_admin_0_countries.geojson":        "27db73de0818a97f9c7beda9590d39a0c39e9ff45e8f2f32fe6c9f284945d572",
		"ne_10m_admin_1_states_provinces.geojson": "ae0d6d65975daead72e054f3715273eda770e89386df5fc299b4cc198f9f4f20",
		"ne_10m_populated_places.geojson":         "91fdec1d0d1efae4d152f4ce08f11263a7e66fc7ee7a08553f7d1060e3234cc6",
	}
	for name, want := range expected {
		path := filepath.Join("data", name)
		data, err := os.ReadFile(path)
		r.NoError(err)
		sum := sha256.Sum256(data)
		got := hex.EncodeToString(sum[:])
		r.Equal(want, got, "embedded %s SHA256 drift", name)
	}
}

// BenchmarkNewNaturalEarth captures the boot-time cost of parsing
// the embedded GeoJSON. Spec §6.8: if this exceeds ~1s on the
// developer machine, escalate.
func BenchmarkNewNaturalEarth(b *testing.B) {
	r := require.New(b)
	for range b.N {
		_, err := geo.NewNaturalEarth()
		r.NoError(err)
	}
}

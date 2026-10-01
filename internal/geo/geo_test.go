package geo

import (
	"crypto/sha256"
	"encoding/hex"
	"runtime"
	"strings"
	"testing"

	"github.com/paulmach/orb"
	"github.com/stretchr/testify/require"
)

func TestNaturalEarthResolveKnownCities(t *testing.T) {
	r := require.New(t)
	g, err := NewNaturalEarth()
	r.NoError(err)

	type tc struct {
		name           string
		lat, lon       float64
		cityPrefix     string
		mustContain    []string
		mustNotContain []string
	}
	cases := []tc{
		{"Paris", 48.8566, 2.3522, "", []string{"France"}, nil},
		// "Manhattan" is a borough, not a populated_places city, so it
		// must NEVER appear in the label — locks the field-selection
		// contract (NAMEASCII → NAME, not e.g. an ADM2-style sub-name).
		{"NYC", 40.7128, -74.0060, "", []string{"New York", "United States"}, []string{"Manhattan"}},
		{"Tokyo", 35.6762, 139.6503, "", []string{"Japan"}, nil},
		{"Sydney", -33.8688, 151.2093, "", []string{"Australia"}, nil},
		{"Cape Town", -33.9249, 18.4241, "", []string{"South Africa"}, nil},
		{"George Town", 19.294748, -81.371570, "George Town", []string{"Cayman Is."}, nil},
		{"Willemstad", 12.112290, -68.872356, "Willemstad", []string{"Curaçao"}, nil},
		{"Linares", 38.083320, -3.633355, "Linares", []string{"Jaén", "Spain"}, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := require.New(t)
			label, ok := g.Resolve(c.lat, c.lon)
			r.True(ok, "expected resolve to succeed; got label=%q ok=%v", label, ok)
			if c.cityPrefix != "" {
				r.True(strings.HasPrefix(label, c.cityPrefix+", "), "label %q does not start with city %q", label, c.cityPrefix)
			}
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
	g, err := NewNaturalEarth()
	r.NoError(err)
	label, _ := g.Resolve(2.3522, 48.8566)
	r.NotContains(label, "France",
		"swapped Paris coords resolved to a France-shaped label %q — "+
			"orb.Point construction order is wrong", label)
	r.NotContains(label, "Paris", "label=%q", label)
}

func TestNaturalEarthOpenOceanReturnsFalse(t *testing.T) {
	r := require.New(t)
	g, err := NewNaturalEarth()
	r.NoError(err)
	_, ok := g.Resolve(0, -30) // mid-Atlantic
	r.False(ok)
}

func TestNaturalEarthSouthPole(t *testing.T) {
	r := require.New(t)
	g, err := NewNaturalEarth()
	r.NoError(err)
	label, ok := g.Resolve(-89.9, 0)
	r.True(ok)
	r.Contains(label, "Antarctica")
}

func TestNaturalEarthAntimeridian(t *testing.T) {
	r := require.New(t)
	g, err := NewNaturalEarth()
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
// admin_0. Tijuana and San Diego are both within the city threshold, but
// only Tijuana shares the resolved country's label.
func TestNaturalEarthSameCountryGate(t *testing.T) {
	r := require.New(t)
	g, err := NewNaturalEarth()
	r.NoError(err)

	label, ok := g.Resolve(32.52, -117.03)
	r.True(ok)
	r.Equal("Tijuana, Baja California, Mexico", label)
	r.NotContains(label, "San Diego")
	r.NotContains(label, "United States")
}

func TestNaturalEarthRegionGate(t *testing.T) {
	r := require.New(t)
	g, err := NewNaturalEarth()
	r.NoError(err)

	// This point is in Basel-Landschaft, where Basel is closer than Liestal.
	// The city must still come from the resolved admin_1 region.
	label, ok := g.Resolve(47.54, 7.61)
	r.True(ok)
	r.Equal("Liestal, Basel-Landschaft, Switzerland", label)
	r.NotContains(label, "Basel, Basel-Stadt")
}

func TestNaturalEarthRegionStaysInCountry(t *testing.T) {
	g, err := NewNaturalEarth()
	require.NoError(t, err)

	// Simplified Algerian and Tunisian borders overlap here. Each label must
	// keep a neighboring country's region out.
	cases := []struct {
		lat, lon float64
		want     string
	}{
		{36.44, 8.35, "Algeria"},
		{35.43, 8.30, "Tunisia"},
		{35.65, 8.32, "Tébessa, Algeria"},
	}
	for _, c := range cases {
		label, ok := g.Resolve(c.lat, c.lon)
		require.True(t, ok)
		require.Equal(t, c.want, label)
	}
}

func TestNearestCityMembershipGates(t *testing.T) {
	tests := []struct {
		name    string
		country string
		region  string
		cities  []cityFeature
		want    string
	}{
		{
			name:    "unknown country",
			country: "A",
			region:  "R",
			cities: []cityFeature{
				{name: "unknown", country: "", admin1: "R", point: orb.Point{0.01, 0}},
				{name: "valid", country: "A", admin1: "R", point: orb.Point{0.02, 0}},
			},
			want: "valid",
		},
		{
			name:    "unknown region",
			country: "A",
			region:  "R",
			cities: []cityFeature{
				{name: "unknown", country: "A", admin1: "", point: orb.Point{0.01, 0}},
				{name: "valid", country: "A", admin1: "R", point: orb.Point{0.02, 0}},
			},
			want: "valid",
		},
		{
			name:    "different country",
			country: "A",
			region:  "R",
			cities: []cityFeature{
				{name: "other", country: "B", admin1: "R", point: orb.Point{0.01, 0}},
				{name: "valid", country: "A", admin1: "R", point: orb.Point{0.02, 0}},
			},
			want: "valid",
		},
		{
			name:    "different region",
			country: "A",
			region:  "R",
			cities: []cityFeature{
				{name: "other", country: "A", admin1: "B", point: orb.Point{0.01, 0}},
				{name: "valid", country: "A", admin1: "R", point: orb.Point{0.02, 0}},
			},
			want: "valid",
		},
		{
			name:    "query without region",
			country: "A",
			cities: []cityFeature{
				{name: "other region", country: "A", admin1: "B", point: orb.Point{0.01, 0}},
				{name: "unknown region", country: "A", admin1: "", point: orb.Point{0.02, 0}},
			},
			want: "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := nearestCity(orb.Point{0, 0}, tt.cities, tt.country, tt.region)
			require.Equal(t, tt.want, got)
		})
	}
}

func TestNaturalEarthCityDistanceGate(t *testing.T) {
	r := require.New(t)
	g, err := NewNaturalEarth()
	r.NoError(err)

	// Alice Springs is about 28 km away, so the 25 km city limit leaves only
	// the containing region and country in the label.
	label, ok := g.Resolve(-23.45, 133.88)
	r.True(ok)
	r.Equal("Northern Territory, Australia", label)
	r.NotContains(label, "Alice Springs")
}

func TestNaturalEarthCountryPolygonHole(t *testing.T) {
	r := require.New(t)
	g, err := NewNaturalEarth()
	r.NoError(err)

	// This point falls inside a Spanish enclave cut out of the France polygon.
	// Ignoring polygon holes would return France because it sorts before Spain.
	label, ok := g.Resolve(42.46277, 1.96382)
	r.True(ok)
	r.Contains(label, "Spain")
	r.NotContains(label, "France")
}

// TestEmbeddedDataChecksums verifies that the embedded archives decompress
// to the GeoJSON bytes pinned in PROVENANCE.md.
func TestEmbeddedDataChecksums(t *testing.T) {
	r := require.New(t)
	expected := map[string]string{
		"ne_10m_admin_0_countries.geojson":        "27db73de0818a97f9c7beda9590d39a0c39e9ff45e8f2f32fe6c9f284945d572",
		"ne_10m_admin_1_states_provinces.geojson": "ae0d6d65975daead72e054f3715273eda770e89386df5fc299b4cc198f9f4f20",
		"ne_10m_populated_places.geojson":         "91fdec1d0d1efae4d152f4ce08f11263a7e66fc7ee7a08553f7d1060e3234cc6",
	}
	for name, want := range expected {
		data, err := readData(name)
		r.NoError(err)
		sum := sha256.Sum256(data)
		got := hex.EncodeToString(sum[:])
		r.Equal(want, got, "embedded %s SHA256 drift", name)
	}
}

// BenchmarkNewNaturalEarth reports the one-time parse cost and the heap the
// parsed gazetteer keeps alive.
func BenchmarkNewNaturalEarth(b *testing.B) {
	r := require.New(b)
	var retained uint64
	for range b.N {
		b.StopTimer()
		var before, after runtime.MemStats
		runtime.GC()
		runtime.ReadMemStats(&before)
		b.StartTimer()
		g, err := NewNaturalEarth()
		r.NoError(err)
		b.StopTimer()
		runtime.GC()
		runtime.ReadMemStats(&after)
		retained = after.HeapAlloc - before.HeapAlloc
		runtime.KeepAlive(g)
		b.StartTimer()
	}
	b.ReportMetric(float64(retained)/1e6, "retained-MB")
}

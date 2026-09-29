// Package geo provides an offline reverse geocoder backed by Natural
// Earth 1:10m. It produces coarse country/region/city labels suitable
// for an info panel; it does NOT produce neighborhood/street-level
// labels.
package geo

import (
	"embed"
	"fmt"
	"math"
	"sort"
	"strings"

	"github.com/paulmach/orb"
	"github.com/paulmach/orb/geojson"
	"github.com/paulmach/orb/planar"
)

//go:embed data/ne_10m_admin_0_countries.geojson
//go:embed data/ne_10m_admin_1_states_provinces.geojson
//go:embed data/ne_10m_populated_places.geojson
var dataFS embed.FS

// A city farther than 25 km from the coordinate is not included in the label.
const (
	cityMaxDistanceKm = 25.0
	earthRadiusKm     = 6371.0
)

// NaturalEarth is a parsed in-memory copy of the embedded gazetteer.
// Construct via NewNaturalEarth; safe for concurrent Resolve calls.
type NaturalEarth struct {
	countries []boundedFeature
	regions   []boundedFeature
	cities    []cityFeature
}

// boundedFeature is the polygon-shaped feature shared by countries and
// regions. The pre-computed bbox lets pointInPolygonName cheaply skip
// features whose bounding box doesn't contain the query point.
type boundedFeature struct {
	name string
	bbox orb.Bound
	geom orb.Geometry
}

type cityFeature struct {
	name    string
	country string
	admin1  string
	point   orb.Point // [lon, lat]
}

// NewNaturalEarth parses the embedded gazetteer once. Returns an error
// if the embedded data is missing or malformed (a programming/build
// error). After this returns, Resolve does no I/O.
func NewNaturalEarth() (*NaturalEarth, error) {
	g := &NaturalEarth{}

	if err := loadCountries(g); err != nil {
		return nil, fmt.Errorf("load countries: %w", err)
	}
	if err := loadRegions(g); err != nil {
		return nil, fmt.Errorf("load regions: %w", err)
	}
	if err := loadCities(g); err != nil {
		return nil, fmt.Errorf("load cities: %w", err)
	}
	return g, nil
}

func loadCountries(g *NaturalEarth) error {
	data, err := dataFS.ReadFile("data/ne_10m_admin_0_countries.geojson")
	if err != nil {
		return fmt.Errorf("read country data: %w", err)
	}
	fc, err := geojson.UnmarshalFeatureCollection(data)
	if err != nil {
		return fmt.Errorf("decode country data: %w", err)
	}
	// Stable sort by name so "first match wins" on overlap is deterministic.
	sort.SliceStable(fc.Features, func(i, j int) bool {
		return featureName(fc.Features[i], "NAME", "ADMIN") <
			featureName(fc.Features[j], "NAME", "ADMIN")
	})
	for _, f := range fc.Features {
		name := featureName(f, "NAME", "ADMIN")
		if name == "" {
			continue
		}
		g.countries = append(g.countries, boundedFeature{
			name: name,
			bbox: f.Geometry.Bound(),
			geom: f.Geometry,
		})
	}
	return nil
}

func loadRegions(g *NaturalEarth) error {
	data, err := dataFS.ReadFile("data/ne_10m_admin_1_states_provinces.geojson")
	if err != nil {
		return fmt.Errorf("read region data: %w", err)
	}
	fc, err := geojson.UnmarshalFeatureCollection(data)
	if err != nil {
		return fmt.Errorf("decode region data: %w", err)
	}
	sort.SliceStable(fc.Features, func(i, j int) bool {
		return featureName(fc.Features[i], "name", "NAME") <
			featureName(fc.Features[j], "name", "NAME")
	})
	for _, f := range fc.Features {
		// admin_1 uses lowercase 'name' only in NE 5.1.1; there is no
		// uppercase NAME on this layer (see PROVENANCE.md note).
		name := featureName(f, "name", "NAME")
		if name == "" {
			continue
		}
		g.regions = append(g.regions, boundedFeature{
			name: name,
			bbox: f.Geometry.Bound(),
			geom: f.Geometry,
		})
	}
	return nil
}

func loadCities(g *NaturalEarth) error {
	data, err := dataFS.ReadFile("data/ne_10m_populated_places.geojson")
	if err != nil {
		return fmt.Errorf("read city data: %w", err)
	}
	fc, err := geojson.UnmarshalFeatureCollection(data)
	if err != nil {
		return fmt.Errorf("decode city data: %w", err)
	}
	for _, f := range fc.Features {
		pt, ok := f.Geometry.(orb.Point)
		if !ok {
			continue
		}
		name := featureName(f, "NAMEASCII", "NAME")
		if name == "" {
			continue
		}
		country, _ := f.Properties["ADM0NAME"].(string)
		admin1, _ := f.Properties["ADM1NAME"].(string)
		g.cities = append(g.cities, cityFeature{
			name: name, country: country, admin1: admin1, point: pt,
		})
	}
	return nil
}

func featureName(f *geojson.Feature, primary, fallback string) string {
	if v, ok := f.Properties[primary].(string); ok && v != "" {
		return v
	}
	if v, ok := f.Properties[fallback].(string); ok {
		return v
	}
	return ""
}

// Resolve returns a coarse human-readable label for the input
// coordinate. Returns ("", false) when no admin_0 polygon contains
// the point (open ocean) or when the input is out of range.
//
// IMPORTANT: public API is (lat, lon); GeoJSON / orb.Point use
// [lon, lat]. Every internal orb.Point construction below reorders.
func (n *NaturalEarth) Resolve(lat, lon float64) (string, bool) {
	if lat < -90 || lat > 90 || lon < -180 || lon > 180 {
		return "", false
	}
	pt := orb.Point{lon, lat} // §6.4 — NOT {lat, lon}

	country := pointInPolygonName(pt, n.countries)
	if country == "" {
		return "", false
	}

	region := pointInPolygonName(pt, n.regions)

	city := nearestCity(pt, n.cities, country, region)

	parts := make([]string, 0, 3)
	if city != "" {
		parts = append(parts, city)
	}
	if region != "" {
		parts = append(parts, region)
	}
	parts = append(parts, country)
	return strings.Join(parts, ", "), true
}

func pointInPolygonName(pt orb.Point, fs []boundedFeature) string {
	for _, f := range fs {
		if !f.bbox.Contains(pt) {
			continue
		}
		switch g := f.geom.(type) {
		case orb.Polygon:
			if planar.PolygonContains(g, pt) {
				return f.name
			}
		case orb.MultiPolygon:
			if planar.MultiPolygonContains(g, pt) {
				return f.name
			}
		}
	}
	return ""
}

// nearestCity scans cities linearly. Honors the §6.5 gates: distance
// ≤ cityMaxDistanceKm, same country, same admin_1 (when admin_1
// resolved). Returns "" if no city qualifies.
func nearestCity(pt orb.Point, cities []cityFeature, country, region string) string {
	bestName := ""
	bestKm := cityMaxDistanceKm + 1
	for _, c := range cities {
		if country != "" && c.country != "" && c.country != country {
			continue
		}
		if region != "" && c.admin1 != "" && c.admin1 != region {
			continue
		}
		km := haversineKm(pt, c.point)
		if km > cityMaxDistanceKm {
			continue
		}
		if km < bestKm {
			bestKm = km
			bestName = c.name
		}
	}
	return bestName
}

func haversineKm(a, b orb.Point) float64 {
	lat1, lon1 := deg2rad(a[1]), deg2rad(a[0])
	lat2, lon2 := deg2rad(b[1]), deg2rad(b[0])
	dLat := lat2 - lat1
	dLon := lon2 - lon1
	h := math.Sin(dLat/2)*math.Sin(dLat/2) +
		math.Cos(lat1)*math.Cos(lat2)*math.Sin(dLon/2)*math.Sin(dLon/2)
	c := 2 * math.Atan2(math.Sqrt(h), math.Sqrt(1-h))
	return earthRadiusKm * c
}

func deg2rad(d float64) float64 { return d * math.Pi / 180 }

// The compile-time guard that *NaturalEarth implements
// ingest.PlaceResolver lives in geo_ingest_test.go — geo's production
// code must not import ingest, but a test-package file can, which lets
// the guard pin against the real interface rather than a re-declared
// shape.

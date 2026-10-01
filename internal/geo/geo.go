// Package geo provides an offline reverse geocoder backed by Natural
// Earth 1:10m. It produces coarse country/region/city labels suitable
// for an info panel; it does NOT produce neighborhood/street-level
// labels.
package geo

import (
	"bytes"
	"compress/gzip"
	"embed"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"slices"
	"sort"
	"strings"

	"github.com/paulmach/orb"
	"github.com/paulmach/orb/geojson"
	"github.com/paulmach/orb/planar"
)

// The map ships gzip-compressed; PROVENANCE.md pins the decompressed bytes.
//
//go:embed data/*.geojson.gz
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
// features whose bounding box doesn't contain the query point. A region's
// country is the country polygon containing a point inside the region, so
// overlapping simplified borders cannot pair a region with a neighbor.
type boundedFeature struct {
	name    string
	country string
	bbox    orb.Bound
	geom    orb.Geometry
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
	data, err := readData("ne_10m_admin_0_countries.geojson")
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
	data, err := readData("ne_10m_admin_1_states_provinces.geojson")
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
		country := ""
		if inside, ok := interiorPoint(f.Geometry); ok {
			country = pointInPolygonName(inside, g.countries, "")
		}
		g.regions = append(g.regions, boundedFeature{
			name:    name,
			country: country,
			bbox:    f.Geometry.Bound(),
			geom:    f.Geometry,
		})
	}
	return nil
}

func loadCities(g *NaturalEarth) error {
	data, err := readData("ne_10m_populated_places.geojson")
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
		country := pointInPolygonName(pt, g.countries, "")
		admin1 := pointInPolygonName(pt, g.regions, country)
		g.cities = append(g.cities, cityFeature{
			name: name, country: country, admin1: admin1, point: pt,
		})
	}
	return nil
}

// readData returns the decompressed bytes of one embedded GeoJSON file.
func readData(name string) ([]byte, error) {
	compressed, err := dataFS.ReadFile("data/" + name + ".gz")
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", name, err)
	}
	reader, err := gzip.NewReader(bytes.NewReader(compressed))
	if err != nil {
		return nil, fmt.Errorf("decompress %s: %w", name, err)
	}
	// The gzip trailer records the decompressed size, so one allocation holds it.
	data := make([]byte, binary.LittleEndian.Uint32(compressed[len(compressed)-4:]))
	if _, err := io.ReadFull(reader, data); err != nil {
		return nil, fmt.Errorf("decompress %s: %w", name, err)
	}
	// Reading past the recorded size must reach EOF, which verifies the checksum.
	switch extra, err := io.CopyN(io.Discard, reader, 1); {
	case extra != 0:
		return nil, fmt.Errorf("decompress %s: data exceeds its gzip trailer size", name)
	case !errors.Is(err, io.EOF):
		return nil, fmt.Errorf("decompress %s: %w", name, err)
	}
	return data, nil
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
	pt := orb.Point{lon, lat} // GeoJSON and orb use [longitude, latitude].

	country := pointInPolygonName(pt, n.countries, "")
	if country == "" {
		return "", false
	}

	region := pointInPolygonName(pt, n.regions, country)

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

// pointInPolygonName returns the first feature containing pt whose country
// matches; regions use country to stay inside the resolved country.
func pointInPolygonName(pt orb.Point, fs []boundedFeature, country string) string {
	for _, f := range fs {
		if f.country != country || !f.bbox.Contains(pt) {
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

// interiorPoint returns the middle of the widest span where a horizontal line
// through the middle of the geometry's largest polygon crosses that polygon.
func interiorPoint(geom orb.Geometry) (orb.Point, bool) {
	var polygon orb.Polygon
	switch g := geom.(type) {
	case orb.Polygon:
		polygon = g
	case orb.MultiPolygon:
		for _, part := range g {
			if planar.Area(part) > planar.Area(polygon) {
				polygon = part
			}
		}
	}
	bound := polygon.Bound()
	y := (bound.Min[1] + bound.Max[1]) / 2
	var crossings []float64
	for _, ring := range polygon {
		for i := 1; i < len(ring); i++ {
			a, b := ring[i-1], ring[i]
			if (a[1] <= y) != (b[1] <= y) {
				crossings = append(crossings, a[0]+(y-a[1])*(b[0]-a[0])/(b[1]-a[1]))
			}
		}
	}
	slices.Sort(crossings)
	var best orb.Point
	widest := 0.0
	for i := 1; i < len(crossings); i += 2 {
		if width := crossings[i] - crossings[i-1]; width > widest {
			widest = width
			best = orb.Point{(crossings[i-1] + crossings[i]) / 2, y}
		}
	}
	return best, widest > 0
}

// nearestCity scans cities linearly, applying the distance, country and
// region gates. City membership comes from the same country and region
// polygons as the query, so a city that falls in a border gap never matches a
// known country or region. Returns "" if no city qualifies.
func nearestCity(pt orb.Point, cities []cityFeature, country, region string) string {
	bestName := ""
	bestKm := cityMaxDistanceKm + 1
	for _, c := range cities {
		if country != "" && c.country != country {
			continue
		}
		if region != "" && c.admin1 != region {
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

# Natural Earth gazetteer — provenance

These GeoJSON files are vendored from Natural Earth 1:10m cultural vectors
and embedded into the `fotobank` binary by `internal/geo`. Update this
file every time the data is re-vendored.

## Release

- `ne_10m_admin_0_countries`: Natural Earth 5.1.1
- `ne_10m_admin_1_states_provinces`: Natural Earth 5.1.1
- `ne_10m_populated_places`: Natural Earth 5.1.2

## Sources

| File | Source URL | Source archive SHA256 |
|---|---|---|
| `ne_10m_admin_0_countries.geojson` | https://naciscdn.org/naturalearth/10m/cultural/ne_10m_admin_0_countries.zip | `ce1ac7036499a0edd641fbc093cd209a98f96a49d2eca8480aaacad35138a7f6` |
| `ne_10m_admin_1_states_provinces.geojson` | https://naciscdn.org/naturalearth/10m/cultural/ne_10m_admin_1_states_provinces.zip | `efc59726337323058f9446210adc96673179cd344e053666ee3d28cb58ba2b05` |
| `ne_10m_populated_places.geojson` | https://naciscdn.org/naturalearth/10m/cultural/ne_10m_populated_places.zip | `cd149186f03d2603e0410da399b980a4357d0ac32d3a2305a49ed3dffcc41d7b` |

## Conversion

Each file was produced by `ogr2ogr -f GeoJSON` with the following layer-creation
and conversion options:

- **`-lco COORDINATE_PRECISION=6`** caps coordinates at 6 decimal degrees
  (~11cm at the equator) — far finer than GPS (~5-10m) and dramatically
  smaller than GDAL's 15-decimal default.
- **`-select <fields>`** keeps only the fields the resolver consumes; the full
  Natural Earth schema includes ~150 localized
  name variants and metadata that are dead weight here.
- **`-simplify 0.01`** (admin_0, admin_1 only) applies Douglas-Peucker
  simplification at ~1 km tolerance. Acceptable for reverse-geocoding
  GPS coords against country/region polygons that are hundreds of km
  thick. Independent per-polygon simplification can leave small slivers
  between adjacent boundaries; border-adjacent photos may resolve to ""
  and the frontend renders them with coords only.

Note on field casing: Natural Earth's admin_1 layer uses lowercase field
names (`name`, `admin`, `iso_3166_2`) and does not expose an uppercase
`NAME` field. The resolver uses `name` for admin_1 (no uppercase
fallback). admin_0 and populated_places use uppercase field names.

Exact commands run:

    ogr2ogr -f GeoJSON -lco COORDINATE_PRECISION=6 -simplify 0.01 \
      -select NAME,ADMIN,NAME_LONG,NAME_EN,ISO_A2,ISO_A3 \
      ne_10m_admin_0_countries.geojson ne_10m_admin_0_countries.shp

    ogr2ogr -f GeoJSON -lco COORDINATE_PRECISION=6 -simplify 0.01 \
      -select name,admin,iso_3166_2 \
      ne_10m_admin_1_states_provinces.geojson ne_10m_admin_1_states_provinces.shp

    ogr2ogr -f GeoJSON -lco COORDINATE_PRECISION=6 \
      -select NAMEASCII,NAME,ADM0NAME,ADM1NAME,FEATURECLA \
      ne_10m_populated_places.geojson ne_10m_populated_places.shp

Tested with GDAL v3.12.x.

## Output checksums

Verified by `internal/geo/geo_test.go::TestEmbeddedDataChecksums` (added
in a later task — keep these accurate). Update when re-vendoring.

| File | SHA256 |
|---|---|
| `ne_10m_admin_0_countries.geojson` | `27db73de0818a97f9c7beda9590d39a0c39e9ff45e8f2f32fe6c9f284945d572` |
| `ne_10m_admin_1_states_provinces.geojson` | `ae0d6d65975daead72e054f3715273eda770e89386df5fc299b4cc198f9f4f20` |
| `ne_10m_populated_places.geojson` | `91fdec1d0d1efae4d152f4ce08f11263a7e66fc7ee7a08553f7d1060e3234cc6` |

## License

Natural Earth data is in the public domain. Full terms in `LICENSE` in
this directory; canonical text at https://www.naturalearthdata.com/about/terms-of-use/.

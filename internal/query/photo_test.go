package query

import (
	"github.com/stretchr/testify/require"
	"testing"
)

func TestPhotoQueryContract(t *testing.T) {
	t.Parallel()
	for _, raw := range []string{
		`{"filters":{"kinds":["photo","video"],"cameras":["Synthetic Camera"],"lenses":["Lens 雪"],"iso_min":0,"iso_max":400,"capture_after":"2024-02-29","gps_bounds":{"south":"-0.00","west":"170","north":"90","east":"-170"}}}`,
		`{"sort":{"field":"capture_time"}}`, `{"sort":{"field":"import_time"}}`,
	} {
		_, err := Parse([]byte(raw))
		require.NoError(t, err)
	}
	for _, raw := range []string{
		`{"filters":{"kinds":["image"]}}`, `{"filters":{"cameras":[""]}}`, `{"filters":{"iso_min":-1}}`, `{"filters":{"iso_min":1.0}}`, `{"filters":{"iso_min":10,"iso_max":9}}`,
		`{"filters":{"capture_after":"2023-02-29"}}`, `{"filters":{"capture_after":""}}`, `{"filters":{"capture_after":"0000-01-01"}}`, `{"filters":{"capture_after":"2024-02-01","capture_before":"2024-01-01"}}`,
		`{"filters":{"gps_bounds":{"south":"0","west":"0","north":"91","east":"0"}}}`, `{"filters":{"gps_bounds":{"south":"1e1","west":"0","north":"90","east":"0"}}}`, `{"filters":{"gps_bounds":{"south":0,"west":"0","north":"90","east":"0"}}}`, `{"filters":{"gps_bounds":{"south":"1","west":"0","north":"0","east":"0"}}}`, `{"filters":{"gps_bounds":{"south":"0","west":"0","north":"90"}}}`, `{"filters":{"asset_ids":["invalid"]}}`,
	} {
		_, err := Parse([]byte(raw))
		require.Error(t, err, raw)
	}
}

func TestPhotoCaptureTimeKey(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ value, precision, zone, offset, key string }{
		{"", "", "", "", ""}, {"2024-01-02", "date", "omitted", "", "2024-01-02T00:00:00.000000000"},
		{"2024-01-02T03:04", "minute", "omitted", "", "2024-01-02T03:04:00.000000000"},
		{"2024-01-02T3", "hour", "omitted", "", "2024-01-02T03:00:00.000000000"},
		{"2024-01-02T3:04", "minute", "omitted", "", "2024-01-02T03:04:00.000000000"},
		{"2024-01-02T3:04Z", "minute", "utc", "", "2024-01-02T03:04:00.000000000"},
		{"2024-01-02T3:04:05", "second", "omitted", "", "2024-01-02T03:04:05.000000000"},
		{"2024-01-02T3:04:05+02:30", "second", "offset", "+02:30", "2024-01-02T00:34:05.000000000"},
		{"2024-01-02T3:04:05.123Z", "fraction", "utc", "", "2024-01-02T03:04:05.123000000"},
		{"2024-01-02T03:04:05+02:30", "second", "offset", "+02:30", "2024-01-02T00:34:05.000000000"},
		{"2024-01-02T03:04:05.1234567891Z", "fraction", "utc", "", "2024-01-02T03:04:05.123456789"},
		{"2024-01-02T03:04:05.0000000000", "fraction", "omitted", "", "2024-01-02T03:04:05.000000000"},
		{"0000-01-02", "date", "omitted", "", ""}, {"0001-01-01T00:00:00+14:00", "second", "offset", "+14:00", ""},
	} {
		key, err := CaptureTimeKey(tc.value, tc.precision, tc.zone, tc.offset)
		require.NoError(t, err, tc.value)
		require.Equal(t, tc.key, key)
	}
	for _, tc := range []struct{ value, precision, zone, offset string }{
		{"2024-02-30", "date", "omitted", ""}, {"2024-01-02T03:04:05Z", "fraction", "utc", ""}, {"2024-01-02T03:04:05Z", "second", "omitted", ""}, {"2024-01-02T03:04:05+01:00", "second", "offset", "+02:00"}, {"", "date", "omitted", ""},
		{"malformed", "second", "omitted", ""}, {"2024-01-02", "unknown", "omitted", ""}, {"2024-01-02", "date", "unknown", ""}, {"2024-01-02T03:04:05+02:30", "second", "offset", "bad"},
	} {
		key, err := CaptureTimeKey(tc.value, tc.precision, tc.zone, tc.offset)
		require.NoError(t, err, tc.value)
		require.Empty(t, key, tc.value)
	}
}

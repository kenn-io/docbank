package processing

import "testing"

func TestEmailDateNumericZeroZone(t *testing.T) {
	for _, tc := range []struct {
		raw  string
		kind string
	}{
		{"10 Mar 2026 12:30 +0000", "utc"},
		{"10 Mar 2026 12:30 -0000", "omitted"},
	} {
		event, ok := rawEmailDateEvent(tc.raw)
		if !ok || string(event.TimezoneKind) != tc.kind {
			t.Errorf("%q: kind %q, parsed %v; want %q", tc.raw, event.TimezoneKind, ok, tc.kind)
		}
	}
}

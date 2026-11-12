package query

import (
	"testing"
	_ "time/tzdata"
)

func TestDocumentDateRange(t *testing.T) {
	for _, tc := range []struct {
		value string
		valid bool
	}{
		{"2026-03-10", true}, {"2026-03-01..2026-03-31", true},
		{"2026-3-1", false}, {"2026-03-1", false}, {"20260301", false}, {"2026-02-30", false}, {"2026-03-31..2026-03-01", false},
	} {
		_, _, err := ParseDocumentDate(tc.value)
		if (err == nil) != tc.valid {
			t.Errorf("%s: %v", tc.value, err)
		}
	}
}

func TestDocumentDateTimezoneValidation(t *testing.T) {
	for _, tc := range []struct {
		zone  string
		valid bool
	}{
		{"UTC", true}, {"America/Toronto", true}, {"Etc/GMT+5", true}, {"CET", false}, {"EST5EDT", false}, {"posix/America/Toronto", false}, {"", false}, {"Local", false}, {"EST", false}, {"+02:00", false},
	} {
		_, err := Parse([]byte(`{"timezone":"` + tc.zone + `"}`))
		if (err == nil) != tc.valid {
			t.Errorf("%q: %v", tc.zone, err)
		}
	}
}

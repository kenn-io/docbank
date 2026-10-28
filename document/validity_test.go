package document

import (
	"testing"
	"time"
)

func TestEvaluateValidity(t *testing.T) {
	tests := []struct {
		name, date string
		window     EffectiveWindow
		successors []SuccessorStart
		want       ValidityState
		undated    bool
	}{
		{"inclusive start", "2025-01-01", EffectiveWindow{From: "2025-01-01"}, nil, ValidityCurrent, false},
		{"inclusive end", "2025-12-31", EffectiveWindow{To: "2025-12-31"}, nil, ValidityCurrent, false},
		{"future before successor", "2024-01-01", EffectiveWindow{From: "2025-01-01"}, []SuccessorStart{{}}, ValidityNotYetEffective, false},
		{"superseded before expiry", "2025-01-01", EffectiveWindow{To: "2024-12-31"}, []SuccessorStart{{From: "2025-01-01"}}, ValiditySuperseded, false},
		{"undated", "2025-01-01", EffectiveWindow{}, []SuccessorStart{{From: "2026-01-01"}, {}}, ValiditySuperseded, true},
		{"expired", "2025-01-01", EffectiveWindow{To: "2024-12-31"}, nil, ValidityExpired, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, undated := EvaluateValidity(tt.date, tt.window, tt.successors)
			if got != tt.want || undated != tt.undated {
				t.Fatalf("got %s %v", got, undated)
			}
		})
	}
}

func FuzzEvaluateValiditySuccessorOrder(f *testing.F) {
	f.Add(uint16(365), uint16(0), uint16(730), uint8(0), []byte{0, 127, 129})
	f.Add(uint16(365), uint16(0), uint16(730), uint8(3), []byte{})
	f.Fuzz(func(t *testing.T, day, from, to uint16, open uint8, raw []byte) {
		date, window, successors := validityFuzzInputs(day, from, to, open, raw)
		state, undated := EvaluateValidity(date, window, successors)
		for i, j := 0, len(successors)-1; i < j; i, j = i+1, j-1 {
			successors[i], successors[j] = successors[j], successors[i]
		}
		secondState, secondUndated := EvaluateValidity(date, window, successors)
		if state != secondState || undated != secondUndated {
			t.Fatalf("successor order changed validity")
		}
	})
}

// Generate canonical dates and valid inclusive windows. Bound each successor
// set while allowing its size, start dates, and undated members to vary.
func validityFuzzInputs(day, from, to uint16, open uint8, raw []byte) (string, EffectiveWindow, []SuccessorStart) {
	base := time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
	date := func(days int) string { return base.AddDate(0, 0, days).Format("2006-01-02") }
	if from > to {
		from, to = to, from
	}
	window := EffectiveWindow{From: date(int(from)), To: date(int(to))}
	if open&1 != 0 {
		window.From = ""
	}
	if open&2 != 0 {
		window.To = ""
	}
	starts := make([]SuccessorStart, min(len(raw), 32))
	for i := range starts {
		if raw[i] != 0 {
			starts[i].From = date(int(day) + int(raw[i]) - 128)
		}
	}
	return date(int(day)), window, starts
}

func TestValidateEffectiveWindow(t *testing.T) {
	for _, tt := range []struct {
		w     EffectiveWindow
		valid bool
	}{
		{EffectiveWindow{}, true}, {EffectiveWindow{From: "2024-02-29", To: "2024-02-29"}, true},
		{EffectiveWindow{From: "2023-02-29"}, false}, {EffectiveWindow{From: "2024-2-01"}, false},
		{EffectiveWindow{From: "2025-01-01", To: "2024-12-31"}, false},
	} {
		if err := ValidateEffectiveWindow(tt.w); (err == nil) != tt.valid {
			t.Errorf("window %+v: %v", tt.w, err)
		}
	}
}

// A later date cannot make an already effective dated successor cease to apply.
func FuzzDatedSuccessorMonotonicity(f *testing.F) {
	f.Add(uint16(0), uint16(1), uint16(1))
	f.Fuzz(func(t *testing.T, start, first, delta uint16) {
		base := time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
		date := func(days int) string { return base.AddDate(0, 0, days).Format("2006-01-02") }
		successor := []SuccessorStart{{From: date(int(start))}}
		earlier, _ := EvaluateValidity(date(int(first)), EffectiveWindow{}, successor)
		later, _ := EvaluateValidity(date(int(first)+int(delta)), EffectiveWindow{}, successor)
		if earlier == ValiditySuperseded && later != ValiditySuperseded {
			t.Fatalf("dated successor stopped applying on a later date")
		}
	})
}

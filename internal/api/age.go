package api

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

// ParseAge parses Go durations plus a day suffix: "30d" = 30*24h. Empty
// means zero (everything). Negative ages are rejected: a future cutoff
// would silently delete the entire trash.
func ParseAge(s string) (time.Duration, error) {
	if s == "" {
		return 0, nil
	}
	var d time.Duration
	if base, ok := strings.CutSuffix(s, "d"); ok {
		days, err := strconv.Atoi(base)
		if err != nil {
			return 0, fmt.Errorf("invalid age %q (want e.g. 30d or 12h): %w", s, err)
		}
		// Guard the multiplication: a huge day count overflows int64
		// nanoseconds and can wrap to a small POSITIVE duration, which the
		// negative check below would miss — and a wrapped-small cutoff makes
		// trash empty delete far newer entries than requested.
		const maxDays = int(math.MaxInt64 / (24 * int64(time.Hour)))
		if days > maxDays || days < -maxDays {
			return 0, fmt.Errorf("invalid age %q: day count overflows a duration", s)
		}
		d = time.Duration(days) * 24 * time.Hour
	} else {
		var err error
		d, err = time.ParseDuration(s)
		if err != nil {
			return 0, fmt.Errorf("invalid age %q (want e.g. 30d or 12h): %w", s, err)
		}
	}
	if d < 0 {
		return 0, fmt.Errorf("invalid age %q: must not be negative", s)
	}
	return d, nil
}

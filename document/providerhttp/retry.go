package providerhttp

import (
	"net/http"
	"strconv"
	"strings"
	"time"
)

// ParseRetryAfter accepts seconds or an HTTP date, bounded to one hour. Invalid
// or missing guidance returns false; valid past dates return a zero delay.
func ParseRetryAfter(value string, now time.Time) (time.Duration, bool) {
	value = strings.TrimSpace(value)
	if seconds, err := strconv.ParseInt(value, 10, 64); err == nil && seconds >= 0 {
		if seconds >= int64(time.Hour/time.Second) {
			return time.Hour, true
		}
		return time.Duration(seconds) * time.Second, true
	}
	when, err := http.ParseTime(value)
	if err != nil {
		return 0, false
	}
	return min(max(when.Sub(now), 0), time.Hour), true
}

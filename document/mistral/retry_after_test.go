package mistral

import (
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestRetryAfterBoundsProviderDelay(t *testing.T) {
	tests := []struct {
		name    string
		header  string
		attempt int
		maximum time.Duration
		want    time.Duration
	}{
		{name: "negative falls back", header: "-1", attempt: 2, maximum: defaultMaxRetryAfter, want: 4 * time.Second},
		{name: "valid value above cap", header: "120", maximum: defaultMaxRetryAfter, want: defaultMaxRetryAfter},
		{name: "duration overflow falls back", header: "9223372037", attempt: 5, maximum: defaultMaxRetryAfter, want: 32 * time.Second},
		{name: "uint64 overflow falls back", header: "18446744073709551616", attempt: 4, maximum: defaultMaxRetryAfter, want: 16 * time.Second},
		{name: "malformed falls back", header: "1s", attempt: 3, maximum: defaultMaxRetryAfter, want: 8 * time.Second},
		{name: "zero is immediate", header: "0", attempt: 4, maximum: defaultMaxRetryAfter, want: 0},
		{name: "positive value honors subsecond maximum", header: "1", maximum: 10 * time.Millisecond, want: 10 * time.Millisecond},
		{name: "zero maximum uses safe default", header: "120", want: defaultMaxRetryAfter},
		{name: "negative maximum uses safe default", header: "120", maximum: -time.Second, want: defaultMaxRetryAfter},
		{name: "fallback caps", attempt: 6, want: defaultMaxRetryAfter},
		{name: "fallback honors maximum", attempt: 6, maximum: 10 * time.Millisecond, want: 10 * time.Millisecond},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, retryAfter(tt.header, tt.attempt, tt.maximum))
		})
	}
}

func TestRetryAfterFallbackAttemptBounds(t *testing.T) {
	tests := []struct {
		attempt int
		want    time.Duration
	}{
		{attempt: -1, want: time.Second},
		{attempt: 0, want: time.Second},
		{attempt: 1, want: 2 * time.Second},
		{attempt: 5, want: 32 * time.Second},
		{attempt: 6, want: defaultMaxRetryAfter},
		{attempt: 100, want: defaultMaxRetryAfter},
	}
	for _, tt := range tests {
		t.Run(strconv.Itoa(tt.attempt), func(t *testing.T) {
			assert.Equal(t, tt.want, retryAfter("", tt.attempt, defaultMaxRetryAfter))
		})
	}
}

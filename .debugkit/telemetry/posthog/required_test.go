package posthog

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRequiredSessionDuration(t *testing.T) {
	for _, disabled := range []bool{false, true} {
		reporter, client, _ := newInstallAgeTestReporter(t, postHogTestStart,
			WithAllowedEvent("session_ended", RequireProperty("duration_bucket", AllowStringValues("over_30m", "30m_to_2h", "over_2h"))))
		if disabled {
			require.NoError(t, reporter.Close())
		}
		for _, duration := range []string{"", "unknown", "over_30m", "30m_to_2h", "over_2h"} {
			body := `{"event":"session_ended","properties":{"duration_bucket":"` + duration + `"}}`
			want := http.StatusAccepted
			if duration == "" || duration == "unknown" {
				want = http.StatusBadRequest
			}
			assert.Equal(t, want, postCapture(t, NewCaptureHandler(reporter), http.MethodPost, body).Code)
		}
		assert.Equal(t, http.StatusBadRequest, postCapture(t, NewCaptureHandler(reporter), http.MethodPost, `{"event":"session_ended"}`).Code)
		if disabled {
			assert.Empty(t, client.messages)
		} else {
			require.Len(t, client.messages, 3)
			require.ErrorIs(t, reporter.Capture("session_ended", nil), ErrInvalidProperty)
		}
	}
}

func TestRequiredPropertyMerge(t *testing.T) {
	optional := AllowProperty("screen", AllowStringValues("detail"))
	required := RequireProperty("screen", AllowStringValues("queue"))
	for _, tc := range []struct {
		name       string
		properties []AllowedProperty
		required   bool
		accepted   string
	}{
		{"required then optional", []AllowedProperty{required, optional}, true, "detail"},
		{"optional then required", []AllowedProperty{optional, required}, true, "queue"},
		{"optional only", []AllowedProperty{optional}, false, "detail"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			options := []Option{WithAllowedEvent("screen", tc.properties[0])}
			if len(tc.properties) > 1 {
				options = append(options, WithAllowedEvent("screen", tc.properties[1]))
			}
			r, _, _ := newInstallAgeTestReporter(t, postHogTestStart, options...)
			_, err := r.SanitizeProperties("screen", nil)
			if tc.required {
				require.ErrorIs(t, err, ErrInvalidProperty)
			} else {
				require.NoError(t, err)
			}
			properties, err := r.SanitizeProperties("screen", map[string]any{"screen": tc.accepted})
			require.NoError(t, err)
			assert.Equal(t, tc.accepted, properties["screen"])
			if tc.required {
				_, err = r.SanitizeProperties("screen", map[string]any{"screen": "unknown"})
				require.ErrorIs(t, err, ErrInvalidProperty)
			}
		})
	}
}

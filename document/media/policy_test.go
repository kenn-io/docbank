package media_test

import (
	"bytes"
	"math"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/document/media"
	"go.kenn.io/docbank/document/media/mediatest"
)

func TestEvaluateReturnsStableReasons(t *testing.T) {
	still := media.Metadata{Format: media.FormatPNG, Kind: media.KindImage, MediaType: "image/png", Size: 100, Width: 4, Height: 3}
	animated := still
	animated.Format, animated.MediaType, animated.FrameCount, animated.Animated = media.FormatGIF, "image/gif", 2, true
	video := media.Metadata{Format: media.FormatMP4, Kind: media.KindVideo, MediaType: "video/mp4", Size: 100, Width: 640, Height: 360, DurationMS: 5000, DurationKnown: true}
	unmeasured := video
	unmeasured.DurationMS, unmeasured.DurationKnown = 0, false
	allowAll := media.Policy{AllowStill: true, AllowAnimated: true, AllowVideo: true}

	tests := []struct {
		name     string
		metadata media.Metadata
		policy   media.Policy
		want     media.Reason
	}{
		{name: "still eligible by default", metadata: still, policy: media.DefaultPolicy(), want: media.ReasonEligible},
		{name: "video eligible by default", metadata: video, policy: media.DefaultPolicy(), want: media.ReasonEligible},
		{name: "animated excluded by default", metadata: animated, policy: media.DefaultPolicy(), want: media.ReasonAnimatedNotAllowed},
		{name: "animated allowed", metadata: animated, policy: allowAll, want: media.ReasonEligible},
		{name: "still not allowed", metadata: still, policy: media.Policy{AllowAnimated: true, AllowVideo: true}, want: media.ReasonStillNotAllowed},
		{name: "video not allowed", metadata: video, policy: media.Policy{AllowStill: true}, want: media.ReasonVideoNotAllowed},
		{name: "too large", metadata: still, policy: media.Policy{MaxBytes: 99, AllowStill: true}, want: media.ReasonTooLarge},
		{name: "width over cap", metadata: still, policy: media.Policy{MaxPixels: 3, AllowStill: true}, want: media.ReasonTooManyPixels},
		{name: "product over cap", metadata: still, policy: media.Policy{MaxPixels: 11, AllowStill: true}, want: media.ReasonTooManyPixels},
		{name: "product at cap", metadata: still, policy: media.Policy{MaxPixels: 12, AllowStill: true}, want: media.ReasonEligible},
		{name: "too long", metadata: video, policy: media.Policy{MaxDurationMS: 4999, AllowVideo: true}, want: media.ReasonTooLong},
		{name: "duration at cap", metadata: video, policy: media.Policy{MaxDurationMS: 5000, AllowVideo: true}, want: media.ReasonEligible},
		{name: "unknown duration under cap", metadata: unmeasured, policy: media.Policy{MaxDurationMS: 5000, AllowVideo: true}, want: media.ReasonTooLong},
		{name: "unknown duration without cap", metadata: unmeasured, policy: media.Policy{AllowVideo: true}, want: media.ReasonEligible},
		{name: "unknown kind", metadata: media.Metadata{Size: 1, Width: 1, Height: 1}, policy: allowAll, want: media.ReasonUnsupportedMedia},
		{name: "missing dimensions", metadata: media.Metadata{Kind: media.KindImage, Size: 1}, policy: allowAll, want: media.ReasonMalformedMedia},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, media.Evaluate(tt.metadata, tt.policy))
		})
	}
}

func TestPolicyDefaultsAndValidation(t *testing.T) {
	normalized := media.Policy{AllowStill: true}.Normalized()
	assert.Equal(t, media.MaxBytes, normalized.MaxBytes)
	assert.Equal(t, media.DefaultMaxPixels, normalized.MaxPixels)
	assert.Zero(t, normalized.MaxDurationMS)

	require.NoError(t, media.DefaultPolicy().Validate())
	require.Error(t, media.Policy{MaxBytes: media.MaxBytes + 1, AllowStill: true}.Validate())
	require.Error(t, media.Policy{}.Validate(), "a policy admitting nothing is unusable")
	for name, policy := range map[string]media.Policy{
		"negative bytes":    {MaxBytes: -1, AllowStill: true},
		"negative pixels":   {MaxPixels: -1, AllowStill: true},
		"negative duration": {MaxDurationMS: -1, AllowVideo: true},
	} {
		t.Run(name, func(t *testing.T) {
			require.ErrorContains(t, policy.Validate(), "negative")
			normalized := policy.Normalized()
			assert.True(t, normalized.MaxBytes < 0 || normalized.MaxPixels < 0 || normalized.MaxDurationMS < 0,
				"negative limits are not silently replaced by defaults")
		})
	}
	still := media.Metadata{Format: media.FormatPNG, Kind: media.KindImage, Size: 10, Width: 2, Height: 2}
	assert.Equal(t, media.ReasonTooLarge, media.Evaluate(still, media.Policy{MaxBytes: -1, AllowStill: true}))
	assert.Equal(t, media.ReasonTooManyPixels, media.Evaluate(still, media.Policy{MaxPixels: -1, AllowStill: true}))
	huge := still
	huge.Width, huge.Height = 1<<40, 1<<40
	assert.Equal(t, media.ReasonTooManyPixels, media.Evaluate(huge, media.Policy{MaxPixels: 1 << 62, AllowStill: true}),
		"overflowing products stay ineligible")
	assert.Equal(t, int64(1<<62), media.Metadata{Width: 1 << 31, Height: 1 << 31}.Pixels())
	assert.Equal(t, int64(math.MaxInt64), huge.Pixels())
}

func TestInspectMapsDetectionOutcomesToReasons(t *testing.T) {
	png := mediatest.PNG(2, 2, nil)
	tests := []struct {
		name   string
		data   []byte
		policy media.Policy
		want   media.Reason
	}{
		{name: "eligible", data: png, policy: media.DefaultPolicy(), want: media.ReasonEligible},
		{name: "unsupported", data: []byte("%PDF-1.7"), policy: media.DefaultPolicy(), want: media.ReasonUnsupportedMedia},
		{name: "malformed", data: []byte("\x89PNG\r\n\x1a\nshort"), policy: media.DefaultPolicy(), want: media.ReasonMalformedMedia},
		{name: "empty", data: nil, policy: media.DefaultPolicy(), want: media.ReasonUnsupportedMedia},
		{name: "oversized", data: png, policy: media.Policy{MaxBytes: 2, AllowStill: true}, want: media.ReasonTooLarge},
		{name: "pixel limit", data: png, policy: media.Policy{MaxPixels: 3, AllowStill: true}, want: media.ReasonTooManyPixels},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			metadata, reason, err := media.Inspect(bytes.NewReader(tt.data), int64(len(tt.data)), "image/png", tt.policy)
			require.NoError(t, err)
			assert.Equal(t, tt.want, reason)
			assert.Equal(t, int64(len(tt.data)), metadata.Size)
			assert.Equal(t, "image/png", metadata.DeclaredMediaType)

			metadata, reason = media.InspectBytes(tt.data, "image/png", tt.policy)
			assert.Equal(t, tt.want, reason)
			assert.Equal(t, int64(len(tt.data)), metadata.Size)
		})
	}
}

func TestInspectRefusesOversizedInputBeforeReading(t *testing.T) {
	_, reason, err := media.Inspect(failingReaderAt{}, 3, "image/png", media.Policy{MaxBytes: 2, AllowStill: true})
	require.NoError(t, err)
	assert.Equal(t, media.ReasonTooLarge, reason)

	_, _, err = media.Inspect(failingReaderAt{}, 1, "image/png", media.DefaultPolicy())
	require.Error(t, err, "read failures are surfaced, not converted to reasons")
}

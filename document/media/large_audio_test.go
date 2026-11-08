package media_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"runtime"
	"runtime/debug"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document/media"
	"go.kenn.io/docbank/document/media/mediatest"
)

func TestInspectFourGiBGeneratedAudioBoundedMemory(t *testing.T) {
	// Serial: the memory limit and allocation measurement are process-wide.
	const size int64 = 4 << 30
	for _, test := range []struct {
		filename, mediaType string
		generate            func(int64) io.Reader
	}{
		{"synthetic.wav", "audio/wav", mediatest.SizedWAV},
		{"synthetic.m4a", "audio/mp4", mediatest.SizedM4A},
	} {
		t.Run(test.filename, func(t *testing.T) {
			hash := sha256.New()
			_, err := io.Copy(hash, test.generate(size))
			require.NoError(t, err)
			source := test.generate(size)
			policy := inspectionPolicy(mediatest.WAV(), test.filename, test.mediaType)
			policy.ExpectedBytes, policy.MaxSourceBytes = size, size
			policy.ExpectedSHA256 = hex.EncodeToString(hash.Sum(nil))
			policy.MaxDurationMS = 24 * 60 * 60 * 1000
			previous := debug.SetMemoryLimit(64 << 20)
			defer debug.SetMemoryLimit(previous)
			runtime.GC()
			var before, after runtime.MemStats
			runtime.ReadMemStats(&before)
			record, err := media.InspectCapability(t.Context(), source, policy)
			runtime.ReadMemStats(&after)
			require.NoError(t, err)
			require.True(t, record.Eligible, record.Reason)
			require.Equal(t, size, record.SourceBytes)
			require.Equal(t, policy.ExpectedSHA256, record.SourceSHA256)
			require.Less(t, after.TotalAlloc-before.TotalAlloc, uint64(8<<20))
			t.Logf("inspected %d bytes under 64 MiB Go memory limit; allocated %d bytes", size, after.TotalAlloc-before.TotalAlloc)
			policy.ExpectedBytes = size + 1
			_, err = media.InspectCapability(t.Context(), source, policy)
			require.ErrorContains(t, err, "source byte bounds")
			policy.ExpectedBytes = size
			policy.MaxSourceBytes = size - 1
			_, err = media.InspectCapability(t.Context(), source, policy)
			require.ErrorContains(t, err, "source byte bounds")
		})
	}
}

func TestAudioCeilingCoversOnlyStreamingInspectors(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		filename, mediaType string
		want                int64
	}{
		{"call.wav", "application/octet-stream", 4 << 30},
		{"CALL.MP3", "application/octet-stream", 4 << 30},
		{"recording", "audio/mpeg", 4 << 30},
		{"recording", "audio/x-wav; rate=8000", 4 << 30},
		{"call.m4a", "audio/mp4", 4 << 30},
		{"recording", "audio/x-m4a", 4 << 30},
		{"image.png", "image/png", 1 << 30},
		{"video.mp4", "video/mp4", 1 << 30},
	} {
		require.Equal(t, test.want, media.InspectionSourceByteCeiling(test.filename, test.mediaType),
			"%s %s", test.filename, test.mediaType)
	}
}

func TestAudioFourGiBPolicyKeepsDocumentBounds(t *testing.T) {
	t.Parallel()
	raw := mediatest.WAV()
	policy := inspectionPolicy(raw, "source.wav", "audio/wav")
	policy.MaxSourceBytes = 4 << 30
	policy.MaxDurationMS = 24 * 60 * 60 * 1000
	record, err := media.InspectCapability(t.Context(), bytes.NewReader(raw), policy)
	require.NoError(t, err)
	require.True(t, record.Eligible)
	policy.Filename, policy.DeclaredMediaType = "source.txt", "text/plain"
	_, err = media.InspectCapability(t.Context(), bytes.NewReader(raw), policy)
	require.ErrorContains(t, err, "source byte bounds")
}

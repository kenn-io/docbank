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

func TestInspectFourGiBGeneratedWAVBoundedMemory(t *testing.T) {
	// Serial: the memory limit and allocation measurement are process-wide.
	const size int64 = 4 << 30
	hash := sha256.New()
	_, err := io.Copy(hash, mediatest.SizedWAV(size))
	require.NoError(t, err)
	source := mediatest.SizedWAV(size)
	policy := inspectionPolicy(mediatest.WAV(), "synthetic.wav", "audio/wav")
	policy.ExpectedBytes, policy.MaxSourceBytes = size, size
	policy.ExpectedSHA256 = hex.EncodeToString(hash.Sum(nil))
	policy.MaxDurationMS = 24 * 60 * 60 * 1000
	previous := debug.SetMemoryLimit(64 << 20)
	defer debug.SetMemoryLimit(previous)
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	record, err := media.InspectCapability(source, policy)
	runtime.ReadMemStats(&after)
	require.NoError(t, err)
	require.True(t, record.Eligible, record.Reason)
	require.Equal(t, size, record.SourceBytes)
	require.Equal(t, policy.ExpectedSHA256, record.SourceSHA256)
	require.Less(t, after.TotalAlloc-before.TotalAlloc, uint64(8<<20))
	t.Logf("inspected %d bytes under 64 MiB Go memory limit; allocated %d bytes", size, after.TotalAlloc-before.TotalAlloc)
	policy.ExpectedBytes = size + 1
	_, err = media.InspectCapability(source, policy)
	require.ErrorContains(t, err, "source byte bounds")
	policy.ExpectedBytes = size
	policy.MaxSourceBytes = size - 1
	_, err = media.InspectCapability(source, policy)
	require.ErrorContains(t, err, "source byte bounds")
}

func TestAudioCeilingIncludesM4AIdentities(t *testing.T) {
	t.Parallel()
	for _, input := range [][2]string{{"call.m4a", "audio/mp4"}, {"call.m4a", "audio/x-m4a"}, {"CALL.M4A", "application/octet-stream"}} {
		require.Equal(t, int64(4<<30), media.InspectionSourceByteCeiling(input[0], input[1]))
	}
	require.Equal(t, int64(1<<30), media.InspectionSourceByteCeiling("image.png", "image/png"))
	require.Equal(t, int64(1<<30), media.InspectionSourceByteCeiling("video.mp4", "video/mp4"))
	require.Equal(t, media.MaxBytes, int64(20<<20))
}

func TestAudioFourGiBPolicyKeepsDocumentBounds(t *testing.T) {
	t.Parallel()
	raw := mediatest.WAV()
	policy := inspectionPolicy(raw, "source.wav", "audio/wav")
	policy.MaxSourceBytes = 4 << 30
	policy.MaxDurationMS = 24 * 60 * 60 * 1000
	record, err := media.InspectCapability(bytes.NewReader(raw), policy)
	require.NoError(t, err)
	require.True(t, record.Eligible)
	policy.Filename, policy.DeclaredMediaType = "source.txt", "text/plain"
	_, err = media.InspectCapability(bytes.NewReader(raw), policy)
	require.ErrorContains(t, err, "source byte bounds")
}

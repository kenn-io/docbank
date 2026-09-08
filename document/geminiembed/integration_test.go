package geminiembed

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/media"
	"go.kenn.io/docbank/document/media/mediatest"
	"go.kenn.io/docbank/document/upload"
)

func TestExecuteEmbeddingInlinePNGAndQueryUseRealAuthorizedPath(t *testing.T) {
	profile := geminiTestProfile(t, 128)
	profile.Descriptor = geminiDescriptorFor(t, profile)
	png := mediatest.PNG(2, 2, nil)
	source := authorizeGeminiFixture(t, profile, png, "private-synthetic.png", "image/png")
	var requests [][]byte
	client := newGeminiTestClient(t, profile, syntheticSecrets{"secret:gemini": "synthetic-key"}, roundTripFunc(func(request *http.Request) (*http.Response, error) {
		body, err := io.ReadAll(request.Body)
		if err != nil {
			return nil, err
		}
		requests = append(requests, body)
		return geminiJSONResponse(request, `{"embedding":{"values":[`+testVectorJSON(128)+`]}}`), nil
	}))
	inputs := []document.EmbeddingInput{
		{Key: "image", Role: document.EmbeddingRoleDocument, Kind: document.EmbeddingInputOriginalFile, Source: source},
		{Key: "query", Role: document.EmbeddingRoleQuery, Kind: document.EmbeddingInputQueryText, Text: "find red"},
	}
	authorization := geminiAuthorization(profile.Descriptor, len(inputs))
	authorization.DiscloseFilename = false

	result, err := document.ExecuteEmbedding(t.Context(), client, inputs, authorization)
	require.NoError(t, err)
	require.Len(t, result.Vectors, 2)
	require.Len(t, requests, 2)
	assert.JSONEq(t, `{"model":"models/gemini-embedding-2","content":{"parts":[{"inlineData":{"mimeType":"image/png","data":"`+
		base64.StdEncoding.EncodeToString(png)+`"}}]},"outputDimensionality":128}`, string(requests[0]))
	assert.JSONEq(t, `{"model":"models/gemini-embedding-2","content":{"parts":[{"text":"task: search result | query: find red"}]},"outputDimensionality":128}`, string(requests[1]))
	for _, request := range requests {
		assert.NotContains(t, string(request), "private-synthetic.png")
		assert.NotContains(t, string(request), "filename")
	}
}

// TestExecuteEmbeddingFilesPNGUsesExactLifecycle catches any path that skips
// core inspection/authorization, changes the proven bytes, sends a filename,
// embeds before activation, or omits confirmed deletion.
func TestExecuteEmbeddingFilesPNGUsesExactLifecycle(t *testing.T) {
	profile := geminiTestProfile(t, 128)
	profile.Transport = TransportFilesAPI
	profile.Descriptor = geminiDescriptorFor(t, profile)
	png := mediatest.PNG(2, 2, nil)
	source := authorizeGeminiFixture(t, profile, png, "private-synthetic.png", "image/png")
	lifecycle := newSuccessfulFilesLifecycle(t, png, "image/png", "private-synthetic.png")
	client := newGeminiTestClient(t, profile, syntheticSecrets{"secret:gemini": "synthetic-key"}, lifecycle)
	inputs := []document.EmbeddingInput{{
		Key: "image", Role: document.EmbeddingRoleDocument,
		Kind: document.EmbeddingInputOriginalFile, Source: source,
	}}
	authorization := geminiAuthorization(profile.Descriptor, len(inputs))
	authorization.DiscloseFilename = true

	result, err := document.ExecuteEmbedding(t.Context(), client, inputs, authorization)

	require.NoError(t, err)
	require.Len(t, result.Vectors, 1)
	assert.Len(t, result.Vectors[0].Values, 128)
	assert.InDelta(t, 1, result.Vectors[0].Values[0], 1e-6)
	requests := lifecycle.snapshot()
	require.Len(t, requests, 6)
	assert.Equal(t, []string{http.MethodPost, http.MethodPost, http.MethodGet, http.MethodGet, http.MethodPost, http.MethodDelete},
		[]string{requests[0].method, requests[1].method, requests[2].method, requests[3].method, requests[4].method, requests[5].method})
	assert.NotContains(t, fileTestRequestBodies(requests), "private-synthetic.png")
	assert.NotContains(t, fileTestRequestBodies(requests), "filename")
}

func TestExecuteEmbeddingFilesLocalProofAndReadFailuresPrecedeLifecycle(t *testing.T) {
	profile := geminiTestProfile(t, 128)
	profile.Transport = TransportFilesAPI
	profile.RequestTimeout = time.Second
	profile.Descriptor = geminiDescriptorFor(t, profile)
	data := mediatest.PNG(2, 2, nil)
	metadata, proof := issuedGeminiAuthority(t, profile, data, "local-gate.png", "image/png")

	t.Run("bytes differ under copied proof", func(t *testing.T) {
		changed := append([]byte(nil), data...)
		changed[len(changed)-1] ^= 0xff
		source := newProofUpload(changed, metadata, proof)
		client, secrets, requests := noEgressGeminiClient(t, profile)

		_, err := document.ExecuteEmbedding(t.Context(), client, directGeminiInputs(source), directGeminiAuthorization(profile, 1))

		require.ErrorContains(t, err, "checksum changed")
		assert.Zero(t, secrets.calls.Load())
		assert.Zero(t, requests.Load())
	})

	t.Run("blocked sealed read is interrupted", func(t *testing.T) {
		profile := geminiTestProfile(t, 128)
		profile.Transport = TransportFilesAPI
		profile.RequestTimeout = time.Second
		profile.Descriptor = geminiDescriptorFor(t, profile)
		metadata, proof := issuedGeminiAuthority(t, profile, data, "local-gate.png", "image/png")
		source := newProofUpload(data, metadata, proof)
		source.blockRead = true
		client, secrets, requests := noEgressGeminiClient(t, profile)
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan error, 1)
		joined := false
		defer func() {
			source.releaseOnce.Do(func() { close(source.released) })
			cancel()
			if joined {
				return
			}
			select {
			case <-done:
			case <-time.After(2 * time.Second):
				t.Error("blocked Files execution did not join during cleanup")
			}
		}()
		go func() {
			_, err := document.ExecuteEmbedding(ctx, client, directGeminiInputs(source), directGeminiAuthorization(profile, 1))
			done <- err
		}()
		select {
		case <-source.readStarted:
		case <-time.After(time.Second):
			t.Fatal("Files source read did not block")
		}
		select {
		case err := <-done:
			joined = true
			require.ErrorIs(t, err, context.DeadlineExceeded)
			require.NoError(t, ctx.Err(), "outer context must stay live until adapter timeout interrupts the sealed read")
		case <-time.After(5 * time.Second):
			t.Error("adapter timeout did not interrupt the sealed Files source read")
			return
		}
		assert.Equal(t, int32(1), source.closeCalls.Load())
		assert.Zero(t, secrets.calls.Load())
		assert.Zero(t, requests.Load())
	})
}

func TestExecuteEmbeddingSupportsEveryAuthorizedMediaTypeAndVideoCodecAcrossTransports(t *testing.T) {
	tests := []struct {
		name, filename, mediaType string
		data                      []byte
	}{
		{name: "PNG", filename: "image.png", mediaType: "image/png", data: mediatest.PNG(2, 3, nil)},
		{name: "JPEG", filename: "image.jpg", mediaType: "image/jpeg", data: mediatest.JPEG(3, 2, nil)},
		{name: "MP3", filename: "audio.mp3", mediaType: "audio/mpeg", data: mediatest.MP3()},
		{name: "WAV", filename: "audio.wav", mediaType: "audio/wav", data: geminiWAV(8_000, 80)},
		{name: "H264 MOV", filename: "h264.mov", mediaType: "video/quicktime", data: mediatest.H264MOV()},
		{name: "H265 MP4", filename: "h265.mp4", mediaType: "video/mp4", data: mediatest.H265MP4()},
		{name: "VP9 MP4", filename: "vp9.mp4", mediaType: "video/mp4", data: mediatest.VP9MP4()},
		{name: "AV1 MP4", filename: "av1.mp4", mediaType: "video/mp4", data: mediatest.AV1MP4()},
		{name: "PDF", filename: "record.pdf", mediaType: "application/pdf", data: syntheticGeminiPDFPages(1)},
	}
	for _, test := range tests {
		for _, transport := range []Transport{TransportInline, TransportFilesAPI} {
			for _, disclose := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s/disclose=%t", test.name, transport, disclose), func(t *testing.T) {
					profile := geminiTestProfile(t, 128)
					profile.Transport = transport
					profile.Descriptor = geminiDescriptorFor(t, profile)
					source := authorizeGeminiFixture(t, profile, test.data, test.filename, test.mediaType)
					var captured []byte
					var wire http.RoundTripper
					var lifecycle *successfulFilesLifecycle
					if transport == TransportInline {
						wire = roundTripFunc(func(request *http.Request) (*http.Response, error) {
							var err error
							captured, err = io.ReadAll(request.Body)
							if err != nil {
								return nil, err
							}
							return geminiJSONResponse(request, `{"embedding":{"values":[`+testVectorJSON(128)+`]}}`), nil
						})
					} else {
						lifecycle = newSuccessfulFilesLifecycle(t, test.data, test.mediaType, test.filename)
						wire = lifecycle
					}
					client := newGeminiTestClient(t, profile, syntheticSecrets{"secret:gemini": "synthetic-key"}, wire)
					authorization := geminiAuthorization(profile.Descriptor, 1)
					authorization.DiscloseFilename = disclose

					result, err := document.ExecuteEmbedding(t.Context(), client, []document.EmbeddingInput{{
						Key: "media", Role: document.EmbeddingRoleDocument,
						Kind: document.EmbeddingInputOriginalFile, Source: source,
					}}, authorization)
					require.NoError(t, err)
					require.Len(t, result.Vectors, 1)
					if transport == TransportInline {
						assert.Contains(t, string(captured), `"mimeType":"`+test.mediaType+`"`)
						assert.Contains(t, string(captured), `"data":"`+base64.StdEncoding.EncodeToString(test.data)+`"`)
						assert.NotContains(t, string(captured), test.filename)
						assert.NotContains(t, string(captured), "filename")
					} else {
						requests := lifecycle.snapshot()
						require.Len(t, requests, 6)
						assert.Equal(t, test.data, requests[1].body)
						assert.Contains(t, string(requests[4].body), `"mimeType":"`+test.mediaType+`"`)
						assert.NotContains(t, fileTestRequestBodies(requests), test.filename)
						assert.NotContains(t, fileTestRequestBodies(requests), "filename")
					}
				})
			}
		}
	}
}

func TestExecuteEmbeddingAcceptsThirtyThreeMeasuredSourceFramesAcrossTransports(t *testing.T) {
	data := geminiMappedAVCMP4(33)
	for _, transport := range []Transport{TransportInline, TransportFilesAPI} {
		t.Run(string(transport), func(t *testing.T) {
			profile := geminiTestProfile(t, 128)
			profile.Transport = transport
			profile.Descriptor = geminiDescriptorFor(t, profile)
			source, record := inspectAndAuthorizeGeminiFixture(t, profile, data, "mapped.mp4", "video/mp4")
			require.Equal(t, int64(33), record.Measurements.Frames)
			var wire http.RoundTripper = roundTripFunc(func(request *http.Request) (*http.Response, error) {
				return geminiJSONResponse(request, `{"embedding":{"values":[`+testVectorJSON(128)+`]}}`), nil
			})
			if transport == TransportFilesAPI {
				wire = newSuccessfulFilesLifecycle(t, data, "video/mp4", "mapped.mp4")
			}
			client := newGeminiTestClient(t, profile, syntheticSecrets{"secret:gemini": "synthetic-key"}, wire)

			result, err := document.ExecuteEmbedding(t.Context(), client, []document.EmbeddingInput{{
				Key: "video", Role: document.EmbeddingRoleDocument,
				Kind: document.EmbeddingInputOriginalFile, Source: source,
			}}, geminiAuthorization(profile.Descriptor, 1))

			require.NoError(t, err)
			assert.Len(t, result.Vectors, 1)
		})
	}
}

func TestExecuteEmbeddingEnforcesAuthorizedMediaMeasurementLimitsAcrossTransports(t *testing.T) {
	accepted := []struct {
		name, filename, mediaType string
		data                      []byte
		limits                    geminiFixtureLimits
	}{
		{name: "six PDF pages", filename: "six.pdf", mediaType: "application/pdf", data: syntheticGeminiPDFPages(6)},
		{name: "180000 ms audio", filename: "limit.wav", mediaType: "audio/wav", data: geminiWAV(1, 180)},
		{name: "120000 ms video", filename: "limit.mp4", mediaType: "video/mp4", data: geminiMappedAVCMP4Duration(1, 120_000)},
	}
	for _, test := range accepted {
		for _, transport := range []Transport{TransportInline, TransportFilesAPI} {
			t.Run("accept "+test.name+"/"+string(transport), func(t *testing.T) {
				profile := geminiTestProfile(t, 128)
				profile.Transport = transport
				profile.Descriptor = geminiDescriptorFor(t, profile)
				source, _ := inspectAndAuthorizeGeminiFixtureWithLimits(t, profile, test.data, test.filename, test.mediaType, test.limits.normalized(test.mediaType))
				var wire http.RoundTripper = roundTripFunc(func(request *http.Request) (*http.Response, error) {
					return geminiJSONResponse(request, `{"embedding":{"values":[`+testVectorJSON(128)+`]}}`), nil
				})
				if transport == TransportFilesAPI {
					wire = newSuccessfulFilesLifecycle(t, test.data, test.mediaType, test.filename)
				}
				client := newGeminiTestClient(t, profile, syntheticSecrets{"secret:gemini": "synthetic-key"}, wire)

				result, err := document.ExecuteEmbedding(t.Context(), client, directGeminiInputs(source), geminiAuthorization(profile.Descriptor, 1))

				require.NoError(t, err)
				assert.Len(t, result.Vectors, 1)
			})
		}
	}

	rejected := []struct {
		name, filename, mediaType string
		data                      []byte
		limits                    geminiFixtureLimits
	}{
		{name: "seven PDF pages", filename: "seven.pdf", mediaType: "application/pdf", data: syntheticGeminiPDFPages(7), limits: geminiFixtureLimits{maxPages: 7}},
		{name: "180200 ms audio", filename: "long.wav", mediaType: "audio/wav", data: geminiWAV(10, 1_802), limits: geminiFixtureLimits{maxDurationMS: 180_200}},
		{name: "120001 ms video", filename: "long.mp4", mediaType: "video/mp4", data: geminiMappedAVCMP4Duration(1, 120_001), limits: geminiFixtureLimits{maxDurationMS: 120_001}},
		{name: "video policy above 10000 frames", filename: "frames.mp4", mediaType: "video/mp4", data: geminiMappedAVCMP4(1), limits: geminiFixtureLimits{maxFrames: 10_001}},
	}
	for _, test := range rejected {
		for _, transport := range []Transport{TransportInline, TransportFilesAPI} {
			t.Run("reject "+test.name+"/"+string(transport), func(t *testing.T) {
				profile := geminiTestProfile(t, 128)
				profile.Transport = transport
				profile.Descriptor = geminiDescriptorFor(t, profile)
				source, _ := inspectAndAuthorizeGeminiFixtureWithLimits(t, profile, test.data, test.filename, test.mediaType, test.limits.normalized(test.mediaType))
				client, secrets, requests := noEgressGeminiClient(t, profile)

				_, err := document.ExecuteEmbedding(t.Context(), client, directGeminiInputs(source), geminiAuthorization(profile.Descriptor, 1))

				require.ErrorContains(t, err, "unsupported or over limit")
				assert.Zero(t, secrets.calls.Load())
				assert.Zero(t, requests.Load())
			})
		}
	}
}

func authorizeGeminiFixture(t *testing.T, profile Profile, data []byte, filename, mediaType string) document.AuthorizedUpload {
	t.Helper()
	source, _ := inspectAndAuthorizeGeminiFixture(t, profile, data, filename, mediaType)
	return source
}

func inspectAndAuthorizeGeminiFixture(t *testing.T, profile Profile, data []byte, filename, mediaType string) (document.AuthorizedUpload, media.CapabilityRecord) {
	t.Helper()
	return inspectAndAuthorizeGeminiFixtureWithLimits(t, profile, data, filename, mediaType, geminiFixtureLimits{}.normalized(mediaType))
}

type geminiFixtureLimits struct {
	maxPages      int64
	maxFrames     int64
	maxDurationMS int64
}

func (limits geminiFixtureLimits) normalized(mediaType string) geminiFixtureLimits {
	if limits.maxPages == 0 {
		limits.maxPages = 6
	}
	if limits.maxFrames == 0 {
		limits.maxFrames = 10_000
	}
	if limits.maxDurationMS == 0 {
		limits.maxDurationMS = 180_000
		if strings.HasPrefix(mediaType, "video/") {
			limits.maxDurationMS = 120_000
		}
	}
	return limits
}

func inspectAndAuthorizeGeminiFixtureWithLimits(t *testing.T, profile Profile, data []byte, filename, mediaType string, limits geminiFixtureLimits) (document.AuthorizedUpload, media.CapabilityRecord) {
	t.Helper()
	digest := sha256.Sum256(data)
	policy := media.InspectionPolicy{
		Filename: filename, DeclaredMediaType: mediaType,
		ExpectedBytes: int64(len(data)), ExpectedSHA256: hex.EncodeToString(digest[:]),
		DescriptorFingerprint: profile.Descriptor.Fingerprint,
		ProfileFingerprint:    profile.CapabilityProfileFingerprint,
		DisclosureFingerprint: profile.DisclosureFingerprint,
		InputKind:             document.RenditionInputOriginalFile,
		MaxSourceBytes:        profile.MaxInputBytes, MaxExpandedBytes: 1 << 20,
		MaxEntryBytes: 1 << 20, MaxEntries: 100, MaxNestingDepth: 1,
		MaxTextLines: 1_000, MaxCharacters: 1 << 20, MaxRecords: 10_000,
		MaxPages: limits.maxPages, MaxSlides: 100, MaxSheets: 100, MaxCells: 10_000,
		MaxSpineItems: 1_000, MaxResources: 10_000,
		MaxPixels: 16_000_000, MaxFrames: limits.maxFrames, MaxDurationMS: limits.maxDurationMS,
	}
	record, err := media.InspectCapability(bytes.NewReader(data), policy)
	require.NoError(t, err)
	require.True(t, record.Eligible, record.Reason)
	source, err := upload.Authorize(t.Context(), upload.Source{
		Reader: io.NopCloser(bytes.NewReader(data)), Directory: t.TempDir(),
	}, record, upload.UploadMetadata{Filename: filename, ProviderMetadata: []byte(strings.Repeat("m", 8))})
	require.NoError(t, err)
	return source, record
}

func geminiWAV(byteRate, dataBytes uint32) []byte {
	data := make([]byte, 44+dataBytes)
	copy(data[0:4], "RIFF")
	binary.LittleEndian.PutUint32(data[4:8], uint32(len(data)-8))
	copy(data[8:12], "WAVE")
	copy(data[12:16], "fmt ")
	binary.LittleEndian.PutUint32(data[16:20], 16)
	binary.LittleEndian.PutUint16(data[20:22], 1)
	binary.LittleEndian.PutUint16(data[22:24], 1)
	binary.LittleEndian.PutUint32(data[24:28], byteRate)
	binary.LittleEndian.PutUint32(data[28:32], byteRate)
	binary.LittleEndian.PutUint16(data[32:34], 1)
	binary.LittleEndian.PutUint16(data[34:36], 8)
	copy(data[36:40], "data")
	binary.LittleEndian.PutUint32(data[40:44], dataBytes)
	return data
}

func syntheticGeminiPDFPages(pageCount int) []byte {
	kids := make([]string, 0, pageCount)
	objects := []string{"<< /Type /Catalog /Pages 2 0 R >>", ""}
	for index := range pageCount {
		kids = append(kids, fmt.Sprintf("%d 0 R", index+3))
		objects = append(objects, "<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] >>")
	}
	objects[1] = fmt.Sprintf("<< /Type /Pages /Kids [%s] /Count %d >>", strings.Join(kids, " "), pageCount)
	var output bytes.Buffer
	_, _ = output.WriteString("%PDF-1.4\n%synthetic\n")
	offsets := make([]int, len(objects))
	for index, object := range objects {
		offsets[index] = output.Len()
		_, _ = fmt.Fprintf(&output, "%d 0 obj\n%s\nendobj\n", index+1, object)
	}
	xref := output.Len()
	_, _ = fmt.Fprintf(&output, "xref\n0 %d\n0000000000 65535 f \n", len(objects)+1)
	for _, offset := range offsets {
		_, _ = fmt.Fprintf(&output, "%010d 00000 n \n", offset)
	}
	_, _ = fmt.Fprintf(&output, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(objects)+1, xref)
	return output.Bytes()
}

func geminiMappedAVCMP4(count int) []byte {
	return geminiMappedAVCMP4Duration(count, int64(count))
}

func geminiMappedAVCMP4Duration(count int, durationMS int64) []byte {
	sample := mediatest.H264PictureSample()
	ftyp := mediatest.Box("ftyp", append([]byte("isom"), make([]byte, 12)...))
	mvhd := make([]byte, 20)
	binary.BigEndian.PutUint32(mvhd[12:16], 1_000)
	binary.BigEndian.PutUint32(mvhd[16:20], uint32(durationMS))
	track := geminiMappedAVCTrack(count, len(sample), durationMS)
	moov := mediatest.Box("moov", slices.Concat(mediatest.Box("mvhd", mvhd), track))
	offset := len(ftyp) + len(moov) + 8
	data := slices.Concat(ftyp, moov, mediatest.Box("mdat", bytes.Repeat(sample, count)))
	stco := geminiMP4BoxPayload(data, "stco")
	binary.BigEndian.PutUint32(stco[8:12], uint32(offset))
	return data
}

func geminiMappedAVCTrack(count, sampleSize int, durationMS int64) []byte {
	tkhd := make([]byte, 84)
	binary.BigEndian.PutUint32(tkhd[20:24], uint32(durationMS))
	binary.BigEndian.PutUint32(tkhd[76:80], 16<<16)
	binary.BigEndian.PutUint32(tkhd[80:84], 16<<16)
	entry := make([]byte, 78)
	binary.BigEndian.PutUint16(entry[24:26], 16)
	binary.BigEndian.PutUint16(entry[26:28], 16)
	entry = slices.Concat(entry, mediatest.Box("avcC", mediatest.AVCConfig(16, 16)))
	stsd := make([]byte, 0, 8+8+len(entry))
	stsd = append(stsd, make([]byte, 8)...)
	binary.BigEndian.PutUint32(stsd[4:8], 1)
	stsd = append(stsd, mediatest.Box("avc1", entry)...)
	stts := make([]byte, 16)
	binary.BigEndian.PutUint32(stts[4:8], 1)
	binary.BigEndian.PutUint32(stts[8:12], uint32(count))
	binary.BigEndian.PutUint32(stts[12:16], uint32(durationMS/int64(count)))
	stsc := make([]byte, 20)
	binary.BigEndian.PutUint32(stsc[4:8], 1)
	binary.BigEndian.PutUint32(stsc[8:12], 1)
	binary.BigEndian.PutUint32(stsc[12:16], uint32(count))
	binary.BigEndian.PutUint32(stsc[16:20], 1)
	stsz := make([]byte, 12)
	binary.BigEndian.PutUint32(stsz[4:8], uint32(sampleSize))
	binary.BigEndian.PutUint32(stsz[8:12], uint32(count))
	stco := make([]byte, 12)
	binary.BigEndian.PutUint32(stco[4:8], 1)
	stbl := mediatest.Box("stbl", slices.Concat(
		mediatest.Box("stsd", stsd), mediatest.Box("stts", stts), mediatest.Box("stsc", stsc),
		mediatest.Box("stsz", stsz), mediatest.Box("stco", stco),
	))
	handler := make([]byte, 12)
	copy(handler[8:12], "vide")
	mdhd := make([]byte, 24)
	binary.BigEndian.PutUint32(mdhd[12:16], 1_000)
	binary.BigEndian.PutUint32(mdhd[16:20], uint32(durationMS))
	mdia := mediatest.Box("mdia", slices.Concat(mediatest.Box("mdhd", mdhd), mediatest.Box("hdlr", handler), mediatest.Box("minf", stbl)))
	return mediatest.Box("trak", append(mediatest.Box("tkhd", tkhd), mdia...))
}

func geminiMP4BoxPayload(data []byte, kind string) []byte {
	base := bytes.Index(data, []byte(kind))
	if base < 4 {
		panic("missing synthetic MP4 box " + kind)
	}
	size := int(binary.BigEndian.Uint32(data[base-4 : base]))
	return data[base+4 : base-4+size]
}

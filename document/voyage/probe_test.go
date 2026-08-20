package voyage_test

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/document/media"
	"go.kenn.io/docbank/document/media/mediatest"
	"go.kenn.io/docbank/document/voyage"
	"go.kenn.io/kit/safefileio"
)

func privateTempDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	require.NoError(t, safefileio.EnsurePrivateDir(dir))
	return dir
}

func writeSeeds(t *testing.T) string {
	t.Helper()
	dir := privateTempDir(t)
	require.NoError(t, os.WriteFile(filepath.Join(dir, voyage.FixtureWebP), mediatest.WebP(64, 64), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, voyage.FixtureMP4), mediatest.MP4(64, 64, 2000), 0o600))
	return dir
}

func writeFixtures(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(privateTempDir(t), "fixtures")
	require.NoError(t, voyage.WriteProbeFixtures(t.Context(), dir, voyage.FixtureOptions{SeedDirectory: writeSeeds(t)}))
	return dir
}

func TestWriteProbeFixturesIsDeterministicAndRequiresSeeds(t *testing.T) {
	require := require.New(t)
	seeds := writeSeeds(t)
	first := filepath.Join(privateTempDir(t), "one")
	second := filepath.Join(privateTempDir(t), "two")
	require.NoError(voyage.WriteProbeFixtures(t.Context(), first, voyage.FixtureOptions{SeedDirectory: seeds}))
	require.NoError(voyage.WriteProbeFixtures(t.Context(), second, voyage.FixtureOptions{SeedDirectory: seeds}))
	for _, name := range []string{
		voyage.FixtureJPEG, voyage.FixturePNG, voyage.FixtureWebP, voyage.FixtureGIFStill, voyage.FixtureGIFAnimated,
		voyage.FixtureMP4, voyage.FixtureRed, voyage.FixtureBlue,
	} {
		left, err := os.ReadFile(filepath.Join(first, name))
		require.NoError(err, name)
		right, err := os.ReadFile(filepath.Join(second, name))
		require.NoError(err, name)
		require.Equal(left, right, "%s must be byte-identical across runs", name)
	}

	require.ErrorContains(voyage.WriteProbeFixtures(t.Context(), filepath.Join(t.TempDir(), "x"), voyage.FixtureOptions{}), "seed directory")
	require.ErrorContains(voyage.WriteProbeFixtures(t.Context(), "", voyage.FixtureOptions{SeedDirectory: seeds}), "destination")
	missingDestination := filepath.Join(privateTempDir(t), "x")
	require.ErrorContains(voyage.WriteProbeFixtures(t.Context(), missingDestination, voyage.FixtureOptions{SeedDirectory: privateTempDir(t)}), "read Voyage probe seed")
	assert.NoDirExists(t, missingDestination)

	wrong := privateTempDir(t)
	require.NoError(os.WriteFile(filepath.Join(wrong, voyage.FixtureWebP), mediatest.PNG(8, 8, nil), 0o600))
	require.NoError(os.WriteFile(filepath.Join(wrong, voyage.FixtureMP4), mediatest.MP4(8, 8, 1), 0o600))
	require.ErrorContains(voyage.WriteProbeFixtures(t.Context(), filepath.Join(privateTempDir(t), "x"), voyage.FixtureOptions{SeedDirectory: wrong}), "must be webp")
}

func TestWriteProbeFixturesRejectsSymlinksWithoutClobbering(t *testing.T) {
	t.Run("seed", func(t *testing.T) {
		seeds := writeSeeds(t)
		target := filepath.Join(seeds, "webp-target")
		require.NoError(t, os.Rename(filepath.Join(seeds, voyage.FixtureWebP), target))
		if err := os.Symlink(target, filepath.Join(seeds, voyage.FixtureWebP)); err != nil {
			t.Skipf("creating a symlink requires additional platform permission: %v", err)
		}
		destination := filepath.Join(privateTempDir(t), "fixtures")

		err := voyage.WriteProbeFixtures(t.Context(), destination, voyage.FixtureOptions{SeedDirectory: seeds})
		require.ErrorContains(t, err, "regular non-symlink")
		assert.NoDirExists(t, destination)
	})

	t.Run("existing destination", func(t *testing.T) {
		victim := filepath.Join(privateTempDir(t), "victim")
		require.NoError(t, os.WriteFile(victim, []byte("preserve me"), 0o600))
		destination := filepath.Join(privateTempDir(t), "fixtures")
		require.NoError(t, os.Mkdir(destination, 0o700))
		if err := os.Symlink(victim, filepath.Join(destination, voyage.FixtureJPEG)); err != nil {
			t.Skipf("creating a symlink requires additional platform permission: %v", err)
		}

		err := voyage.WriteProbeFixtures(t.Context(), destination, voyage.FixtureOptions{SeedDirectory: writeSeeds(t)})
		require.ErrorContains(t, err, "destination already exists")
		contents, readErr := os.ReadFile(victim)
		require.NoError(t, readErr)
		assert.Equal(t, []byte("preserve me"), contents)
	})
}

func TestProbeFixturesRequirePrivateDirectoriesAndRegularFiles(t *testing.T) {
	policy := testPolicy(t)
	t.Run("fixture symlink", func(t *testing.T) {
		dir := writeFixtures(t)
		target := filepath.Join(dir, "jpeg-target")
		require.NoError(t, os.Rename(filepath.Join(dir, voyage.FixtureJPEG), target))
		if err := os.Symlink(target, filepath.Join(dir, voyage.FixtureJPEG)); err != nil {
			t.Skipf("creating a symlink requires additional platform permission: %v", err)
		}

		err := voyage.ValidateProbeFixtures(t.Context(), policy, voyage.ProbeFixtureConfig{FixtureDirectory: dir})
		require.ErrorContains(t, err, "regular non-symlink")
	})

	if runtime.GOOS == "windows" {
		return
	}
	t.Run("fixture directory permissions", func(t *testing.T) {
		dir := writeFixtures(t)
		require.NoError(t, os.Chmod(dir, 0o755))
		t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

		err := voyage.ValidateProbeFixtures(t.Context(), policy, voyage.ProbeFixtureConfig{FixtureDirectory: dir})
		require.ErrorContains(t, err, "private")
	})
	t.Run("seed directory permissions", func(t *testing.T) {
		seeds := writeSeeds(t)
		require.NoError(t, os.Chmod(seeds, 0o755))
		t.Cleanup(func() { _ = os.Chmod(seeds, 0o700) })
		destination := filepath.Join(privateTempDir(t), "fixtures")

		err := voyage.WriteProbeFixtures(t.Context(), destination, voyage.FixtureOptions{SeedDirectory: seeds})
		require.ErrorContains(t, err, "private")
		assert.NoDirExists(t, destination)
	})
}

func TestValidateProbeFixturesRejectsIncompleteOrTamperedSets(t *testing.T) {
	require := require.New(t)
	policy := testPolicy(t)
	dir := writeFixtures(t)
	require.NoError(voyage.ValidateProbeFixtures(t.Context(), policy, voyage.ProbeFixtureConfig{FixtureDirectory: dir}))

	require.ErrorContains(voyage.ValidateProbeFixtures(t.Context(), policy, voyage.ProbeFixtureConfig{}), "directory is required")
	require.ErrorContains(voyage.ValidateProbeFixtures(t.Context(), voyage.Policy{}, voyage.ProbeFixtureConfig{FixtureDirectory: dir}), "use NewPolicy")

	require.NoError(os.WriteFile(filepath.Join(dir, voyage.FixtureRed), mediatest.PNG(64, 64, nil), 0o600))
	require.ErrorContains(voyage.ValidateProbeFixtures(t.Context(), policy, voyage.ProbeFixtureConfig{FixtureDirectory: dir}), "deterministic generation")

	require.NoError(os.Remove(filepath.Join(dir, voyage.FixtureRed)))
	require.ErrorContains(voyage.ValidateProbeFixtures(t.Context(), policy, voyage.ProbeFixtureConfig{FixtureDirectory: dir}), "read Voyage probe fixture")

	strict, err := voyage.NewPolicy(voyage.PolicyConfig{Media: media.Policy{MaxBytes: 16, AllowStill: true}})
	require.NoError(err)
	require.ErrorContains(voyage.ValidateProbeFixtures(t.Context(), strict, voyage.ProbeFixtureConfig{FixtureDirectory: writeFixtures(t)}), "too_large")
}

// fakeProvider answers embedding requests with vectors that encode the
// semantic content of the deterministic fixtures, so the probe's ranking,
// motion, contribution, and batch checks are meaningful.
type fakeProvider struct {
	t                    *testing.T
	fixtures             map[string][]byte
	rejects              map[string]int // fixture name -> HTTP status
	frozenGIF            bool           // animated GIF returns the still-frame vector
	swapPairs            bool           // multi-item batches return neighbors' vectors
	zeroAll              bool           // every embedding is the zero vector
	ignoreCompositeText  bool           // composite requests consume only their media part
	ignoreCompositeMedia bool           // composite requests consume only their text part
	formatOnlyComposite  bool           // composite media consumes only its container format
	calls                int
}

const fakeDimension = voyage.DefaultDimension

func (f *fakeProvider) vectorFor(kind string) []float32 {
	vector := make([]float32, fakeDimension)
	switch kind {
	case "red":
		vector[0] = 1
	case "blue":
		vector[1] = 1
	case "gif":
		vector[2] = 1
	case "animated":
		vector[2], vector[3] = 0.7, 0.7
	case "webp":
		vector[4] = 1
	case "mp4":
		vector[5] = 1
	case "png-format":
		vector[6] = 1
	case "jpeg-format":
		vector[7] = 1
	default:
		vector[9] = 1
	}
	return vector
}

func (f *fakeProvider) classify(data []byte) string {
	for name, fixture := range f.fixtures {
		if !bytes.Equal(fixture, data) {
			continue
		}
		switch name {
		case voyage.FixtureRed, voyage.FixtureJPEG:
			return "red"
		case voyage.FixtureBlue, voyage.FixturePNG:
			return "blue"
		case voyage.FixtureGIFStill:
			return "gif"
		case voyage.FixtureGIFAnimated:
			if f.frozenGIF {
				return "gif"
			}
			return "animated"
		case voyage.FixtureWebP:
			return "webp"
		case voyage.FixtureMP4:
			return "mp4"
		}
	}
	return "unknown"
}

func (f *fakeProvider) fixtureName(data []byte) string {
	for name, fixture := range f.fixtures {
		if bytes.Equal(fixture, data) {
			return name
		}
	}
	return ""
}

func (f *fakeProvider) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.calls++
	request := decodeRequest(f.t, r)
	vectors := make([][]float32, len(request.Inputs))
	for index, input := range request.Inputs {
		vector := make([]float32, fakeDimension)
		composite := len(input.Content) > 1
		for _, part := range input.Content {
			switch part.Type {
			case "text":
				if composite && f.ignoreCompositeText {
					continue
				}
				if strings.Contains(part.Text, "red") {
					add(vector, f.vectorFor("red"), 0.5)
				} else if strings.Contains(part.Text, "blue") {
					add(vector, f.vectorFor("blue"), 0.5)
				}
			case "image_base64", "video_base64":
				if composite && f.ignoreCompositeMedia {
					continue
				}
				payload := part.ImageBase64
				if payload == "" {
					payload = part.VideoBase64
				}
				_, encoded, _ := strings.Cut(payload, ";base64,")
				data, err := base64.StdEncoding.DecodeString(encoded)
				assert.NoError(f.t, err)
				if status, rejected := f.rejects[f.fixtureName(data)]; rejected {
					w.WriteHeader(status)
					_, _ = io.WriteString(w, `{"detail":"synthetic rejection"}`)
					return
				}
				if composite && f.formatOnlyComposite {
					format := "jpeg-format"
					if strings.HasPrefix(payload, "data:image/png;") {
						format = "png-format"
					}
					add(vector, f.vectorFor(format), 1)
					continue
				}
				add(vector, f.vectorFor(f.classify(data)), 1)
			}
		}
		vectors[index] = vector
	}
	if f.zeroAll {
		for index := range vectors {
			vectors[index] = make([]float32, fakeDimension)
		}
	}
	if f.swapPairs && len(vectors) > 1 {
		for index := 0; index+1 < len(vectors); index += 2 {
			vectors[index], vectors[index+1] = vectors[index+1], vectors[index]
		}
	}
	items := make([]wireItem, len(vectors))
	for index, vector := range vectors {
		items[index] = wireItem{Embedding: vector, Index: index}
	}
	w.Header().Set("Content-Type", "application/json")
	assert.NoError(f.t, json.NewEncoder(w).Encode(map[string]any{
		"model": voyage.DefaultModel, "data": items, "usage": map[string]any{"total_tokens": 3},
	}))
}

func add(target, source []float32, scale float32) {
	for index := range target {
		target[index] += source[index] * scale
	}
}

func loadFixtures(t *testing.T, dir string) map[string][]byte {
	t.Helper()
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	fixtures := make(map[string][]byte, len(entries))
	for _, entry := range entries {
		data, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		require.NoError(t, err)
		fixtures[entry.Name()] = data
	}
	return fixtures
}

func TestRunCapabilityProbeProducesAuthorizableManifest(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	policy, err := voyage.NewPolicy(voyage.PolicyConfig{Media: media.DefaultPolicy(), MaxBatchItems: 6})
	require.NoError(err)
	dir := writeFixtures(t)
	provider := &fakeProvider{t: t, fixtures: loadFixtures(t, dir)}
	server := httptest.NewTLSServer(provider)
	defer server.Close()
	client := newServerClient(t, server, policy, voyage.ClientConfig{})

	manifest, err := voyage.RunCapabilityProbe(t.Context(), client, voyage.ProbeConfig{
		Fixtures:   voyage.ProbeFixtureConfig{FixtureDirectory: dir},
		ObservedAt: time.Date(2026, 8, 18, 12, 0, 0, 0, time.UTC),
	})
	require.NoError(err)
	assert.Equal("2026-08-18", manifest.ObservedOn)
	assert.Equal(6, manifest.MaxBatchItems)
	require.Len(manifest.Results, len(voyage.Capabilities()))
	for _, result := range manifest.Results {
		assert.Equal(voyage.ProbeStatusPassed, result.Status, result.CapabilityID)
		assert.Empty(result.ReasonCode, result.CapabilityID)
		require.NotNil(result.TotalTokens, result.CapabilityID)
	}
	authorizations, err := policy.AuthorizeAll(manifest)
	require.NoError(err)
	assert.Len(authorizations, len(voyage.Capabilities()))

	var encoded bytes.Buffer
	require.NoError(voyage.EncodeCapabilityManifest(&encoded, manifest))
	assert.NotContains(encoded.String(), "base64")
	assert.NotContains(encoded.String(), "embedding")
	decoded, err := voyage.DecodeCapabilityManifest(&encoded)
	require.NoError(err)
	assert.Equal(manifest, decoded)
}

func TestRunCapabilityProbeRecordsScrubbedFailures(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	policy, err := voyage.NewPolicy(voyage.PolicyConfig{Media: media.DefaultPolicy(), MaxBatchItems: 4})
	require.NoError(err)
	dir := writeFixtures(t)
	provider := &fakeProvider{
		t: t, fixtures: loadFixtures(t, dir), frozenGIF: true,
		rejects: map[string]int{voyage.FixtureWebP: http.StatusBadRequest, voyage.FixtureMP4: http.StatusUnprocessableEntity},
	}
	server := httptest.NewTLSServer(provider)
	defer server.Close()
	client := newServerClient(t, server, policy, voyage.ClientConfig{MaxRetries: 1})

	manifest, err := voyage.RunCapabilityProbe(t.Context(), client, voyage.ProbeConfig{Fixtures: voyage.ProbeFixtureConfig{FixtureDirectory: dir}})
	require.NoError(err)
	byID := map[string]voyage.CapabilityResult{}
	for _, result := range manifest.Results {
		byID[result.CapabilityID] = result
	}
	assert.Equal(voyage.ProbeStatusRejected, byID[voyage.CapabilityImageWebP].Status)
	assert.Equal(voyage.ReasonProviderRejected, byID[voyage.CapabilityImageWebP].ReasonCode)
	assert.Nil(byID[voyage.CapabilityImageWebP].TotalTokens)
	assert.Equal(voyage.ProbeStatusRejected, byID[voyage.CapabilityVideoMP4].Status)
	assert.Equal(voyage.ProbeStatusFailed, byID[voyage.CapabilityImageGIFAnimated].Status)
	assert.Equal(voyage.ReasonMotionNotObserved, byID[voyage.CapabilityImageGIFAnimated].ReasonCode)
	assert.Equal(voyage.ProbeStatusPassed, byID[voyage.CapabilityImageGIFStill].Status)
	assert.Equal(voyage.ProbeStatusPassed, byID[voyage.CapabilityQueryText].Status)
	assert.Equal(voyage.ProbeStatusPassed, byID[voyage.CapabilityQueryImagePNG].Status)
	assert.Equal(voyage.ProbeStatusRejected, byID[voyage.CapabilityQueryImageWebP].Status, "a rejected format cannot pass as a query either")
	assert.Equal(voyage.ProbeStatusPassed, byID[voyage.CapabilityQueryTextImage].Status)
	assert.Equal(voyage.ProbeStatusPassed, byID[voyage.CapabilityBatchLimits].Status)

	_, err = policy.Authorize(manifest, voyage.CapabilityImageWebP)
	require.ErrorContains(err, "did not pass")
	_, err = policy.Authorize(manifest, voyage.CapabilityImageGIFAnimated)
	require.ErrorContains(err, "did not pass")
	_, err = policy.Authorize(manifest, voyage.CapabilityImagePNG)
	require.NoError(err)
}

func TestRunCapabilityProbeAbortsOnAuthorizationFailureAndTransientExhaustion(t *testing.T) {
	policy := testPolicy(t)
	dir := writeFixtures(t)
	t.Run("unauthorized aborts", func(t *testing.T) {
		server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusUnauthorized)
		}))
		defer server.Close()
		client := newServerClient(t, server, policy, voyage.ClientConfig{MaxRetries: 1})
		_, err := voyage.RunCapabilityProbe(t.Context(), client, voyage.ProbeConfig{Fixtures: voyage.ProbeFixtureConfig{FixtureDirectory: dir}})
		require.ErrorIs(t, err, voyage.ErrUnauthorized)
	})
	t.Run("transient failures are recorded", func(t *testing.T) {
		server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusServiceUnavailable)
		}))
		defer server.Close()
		client := newServerClient(t, server, policy, voyage.ClientConfig{MaxRetries: 1})
		manifest, err := voyage.RunCapabilityProbe(t.Context(), client, voyage.ProbeConfig{Fixtures: voyage.ProbeFixtureConfig{FixtureDirectory: dir}})
		require.NoError(t, err)
		for _, result := range manifest.Results {
			assert.Equal(t, voyage.ProbeStatusFailed, result.Status)
			assert.Equal(t, voyage.ReasonTransientExhausted, result.ReasonCode)
		}
		_, err = policy.AuthorizeAll(manifest)
		require.NoError(t, err)
		authorizations, err := policy.AuthorizeAll(manifest)
		require.NoError(t, err)
		assert.Empty(t, authorizations)
	})
	t.Run("nil client and missing fixtures", func(t *testing.T) {
		_, err := voyage.RunCapabilityProbe(t.Context(), nil, voyage.ProbeConfig{})
		require.ErrorContains(t, err, "requires a client")
		client, err := voyage.NewClient(policy, voyage.ClientConfig{APIKey: "k"})
		require.NoError(t, err)
		_, err = voyage.RunCapabilityProbe(t.Context(), client, voyage.ProbeConfig{Fixtures: voyage.ProbeFixtureConfig{FixtureDirectory: privateTempDir(t)}})
		require.ErrorContains(t, err, "read Voyage probe fixture")
	})
}

func TestRunCapabilityProbeDetectsConsistentBatchSwaps(t *testing.T) {
	policy, err := voyage.NewPolicy(voyage.PolicyConfig{Media: media.DefaultPolicy(), MaxBatchItems: 4})
	require.NoError(t, err)
	dir := writeFixtures(t)
	provider := &fakeProvider{t: t, fixtures: loadFixtures(t, dir), swapPairs: true}
	server := httptest.NewTLSServer(provider)
	defer server.Close()
	client := newServerClient(t, server, policy, voyage.ClientConfig{})
	manifest, err := voyage.RunCapabilityProbe(t.Context(), client, voyage.ProbeConfig{Fixtures: voyage.ProbeFixtureConfig{FixtureDirectory: dir}})
	require.NoError(t, err)
	for _, result := range manifest.Results {
		if result.CapabilityID == voyage.CapabilityBatchLimits {
			assert.Equal(t, voyage.ProbeStatusFailed, result.Status)
			assert.Equal(t, voyage.ReasonOrderNotObserved, result.ReasonCode)
		} else {
			assert.Equal(t, voyage.ProbeStatusPassed, result.Status, result.CapabilityID)
		}
	}
}

func TestRunCapabilityProbeRequiresBothCompositeComponents(t *testing.T) {
	policy, err := voyage.NewPolicy(voyage.PolicyConfig{Media: media.DefaultPolicy(), MaxBatchItems: 4})
	require.NoError(t, err)
	for _, tt := range []struct {
		name                 string
		ignoreCompositeText  bool
		ignoreCompositeMedia bool
		formatOnlyComposite  bool
	}{
		{name: "ignored text", ignoreCompositeText: true},
		{name: "ignored media", ignoreCompositeMedia: true},
		{name: "format sensitive but pixel blind", formatOnlyComposite: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			dir := writeFixtures(t)
			provider := &fakeProvider{
				t: t, fixtures: loadFixtures(t, dir),
				ignoreCompositeText: tt.ignoreCompositeText, ignoreCompositeMedia: tt.ignoreCompositeMedia,
				formatOnlyComposite: tt.formatOnlyComposite,
			}
			server := httptest.NewTLSServer(provider)
			defer server.Close()
			client := newServerClient(t, server, policy, voyage.ClientConfig{})
			manifest, err := voyage.RunCapabilityProbe(t.Context(), client, voyage.ProbeConfig{
				Fixtures: voyage.ProbeFixtureConfig{FixtureDirectory: dir},
			})
			require.NoError(t, err)
			byID := make(map[string]voyage.CapabilityResult, len(manifest.Results))
			for _, result := range manifest.Results {
				byID[result.CapabilityID] = result
			}
			assert.Equal(t, voyage.ProbeStatusFailed, byID[voyage.CapabilityQueryTextImage].Status)
			assert.Equal(t, voyage.ProbeStatusFailed, byID[voyage.CapabilityInterleavedPNG].Status)
		})
	}
}

func TestRunCapabilityProbeSupportsSingleItemBatchPolicy(t *testing.T) {
	policy, err := voyage.NewPolicy(voyage.PolicyConfig{Media: media.DefaultPolicy(), MaxBatchItems: 1})
	require.NoError(t, err)
	dir := writeFixtures(t)
	provider := &fakeProvider{t: t, fixtures: loadFixtures(t, dir)}
	server := httptest.NewTLSServer(provider)
	defer server.Close()
	client := newServerClient(t, server, policy, voyage.ClientConfig{})
	manifest, err := voyage.RunCapabilityProbe(t.Context(), client, voyage.ProbeConfig{Fixtures: voyage.ProbeFixtureConfig{FixtureDirectory: dir}})
	require.NoError(t, err)
	for _, result := range manifest.Results {
		assert.Equal(t, voyage.ProbeStatusPassed, result.Status, result.CapabilityID)
	}
}

func TestRunCapabilityProbeRejectsZeroVectorsAsEvidence(t *testing.T) {
	policy := testPolicy(t)
	dir := writeFixtures(t)
	provider := &fakeProvider{t: t, fixtures: loadFixtures(t, dir), zeroAll: true}
	server := httptest.NewTLSServer(provider)
	defer server.Close()
	client := newServerClient(t, server, policy, voyage.ClientConfig{MaxRetries: 1})
	manifest, err := voyage.RunCapabilityProbe(t.Context(), client, voyage.ProbeConfig{Fixtures: voyage.ProbeFixtureConfig{FixtureDirectory: dir}})
	require.NoError(t, err)
	for _, result := range manifest.Results {
		assert.NotEqual(t, voyage.ProbeStatusPassed, result.Status, "%s must not pass on zero vectors", result.CapabilityID)
	}
	authorizations, err := policy.AuthorizeAll(manifest)
	require.NoError(t, err)
	assert.Empty(t, authorizations)
}

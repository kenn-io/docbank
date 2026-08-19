package voyage

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"time"

	"go.kenn.io/docbank/document/media"
)

// ProbeConfig controls one explicit authenticated capability probe.
type ProbeConfig struct {
	Fixtures   ProbeFixtureConfig
	ObservedAt time.Time
}

const (
	// motionThreshold is the largest cosine similarity between an animated
	// GIF and its still first frame that still counts as observed motion.
	motionThreshold = 0.999
	// contributionThreshold is the largest cosine similarity between a
	// text-plus-media input and the media alone that still counts as the
	// text contributing.
	contributionThreshold = 0.9999
	// clusterThreshold is the smallest cosine similarity between a batch
	// result and the independently embedded reference of the same fixture.
	clusterThreshold = 0.99
)

// RunCapabilityProbe probes every capability serially against the live
// provider and returns sanitized observations only. An authorization failure
// aborts the probe because nothing can pass without a valid credential.
func RunCapabilityProbe(ctx context.Context, client *Client, config ProbeConfig) (CapabilityManifest, error) {
	if client == nil {
		return CapabilityManifest{}, errors.New("voyage capability probe requires a client")
	}
	if config.ObservedAt.IsZero() {
		config.ObservedAt = time.Now().UTC()
	}
	observedOn := config.ObservedAt.UTC().Format(time.DateOnly)
	if observed, err := time.Parse(time.DateOnly, observedOn); err != nil ||
		observed.After(time.Now().UTC().Add(24*time.Hour)) {
		return CapabilityManifest{}, errors.New("voyage capability probe has invalid observation date")
	}
	fixtures, err := loadProbeFixtures(ctx, client.policy, config.Fixtures)
	if err != nil {
		return CapabilityManifest{}, err
	}
	values := client.policy.values
	manifest := CapabilityManifest{
		SchemaVersion: CapabilitySchemaVersion, ProbeFixtureContract: ProbeFixtureContract,
		ObservedOn: observedOn, Endpoint: values.Endpoint, Model: values.Model,
		Dimension: values.Dimension, MaxBatchItems: values.MaxBatchItems,
		Results: make([]CapabilityResult, 0, len(capabilities)),
	}
	runner := &probeRunner{client: client, fixtures: fixtures, references: map[string][]float32{}}
	for _, capability := range capabilities {
		if err := ctx.Err(); err != nil {
			return CapabilityManifest{}, err
		}
		result := CapabilityResult{
			CapabilityID:       capability.ID,
			RequestFingerprint: requestFingerprint(values.Endpoint, values.Model, values.Dimension, capability),
		}
		observation, probeErr := runner.probe(ctx, capability)
		result.FixtureDigest = observation.fixtureDigest
		switch {
		case probeErr == nil && observation.reason == "":
			result.Status = ProbeStatusPassed
			result.TotalTokens = observation.tokens
		case probeErr == nil:
			result.Status, result.ReasonCode = ProbeStatusFailed, observation.reason
		default:
			if ctxErr := ctx.Err(); ctxErr != nil {
				return CapabilityManifest{}, ctxErr
			}
			if errors.Is(probeErr, ErrUnauthorized) {
				return CapabilityManifest{}, fmt.Errorf("voyage capability probe: %w", probeErr)
			}
			result.Status, result.ReasonCode = classifyProbeError(probeErr)
		}
		manifest.Results = append(manifest.Results, result)
	}
	if err := manifest.ValidateComplete(); err != nil {
		return CapabilityManifest{}, fmt.Errorf("voyage capability probe produced an invalid manifest: %w", err)
	}
	return manifest, nil
}

func classifyProbeError(err error) (ProbeStatus, string) {
	switch {
	case errors.Is(err, ErrBatchTooLarge):
		return ProbeStatusRejected, ReasonProviderLimit
	case errors.Is(err, ErrPermanentResponse):
		return ProbeStatusRejected, ReasonProviderRejected
	case errors.Is(err, ErrMalformedResponse):
		return ProbeStatusFailed, ReasonMalformedResponse
	case errors.Is(err, ErrTransientResponse):
		return ProbeStatusFailed, ReasonTransientExhausted
	default:
		return ProbeStatusFailed, ReasonInvalidOrLocalError
	}
}

type probeObservation struct {
	fixtureDigest string
	reason        string
	tokens        *int64
}

type probeRunner struct {
	client     *Client
	fixtures   probeFixtures
	references map[string][]float32
	allow      map[string]bool
}

// mediaPolicy keeps the policy caps but admits every media kind: the probe
// decides which kinds the provider handles; the application's Allow flags
// gate consumption afterwards.
func (r *probeRunner) mediaPolicy() media.Policy {
	policy := r.client.policy.values.Media
	policy.AllowStill, policy.AllowAnimated, policy.AllowVideo = true, true, true
	return policy
}

func (r *probeRunner) authorized() map[string]bool {
	if r.allow == nil {
		r.allow = make(map[string]bool, len(capabilities))
		for _, capability := range capabilities {
			r.allow[capability.ID] = true
		}
	}
	return r.allow
}

func (r *probeRunner) probe(ctx context.Context, capability Capability) (probeObservation, error) {
	switch capability.ID {
	case CapabilityImageJPEG:
		return r.probeDocument(ctx, capability.ID, FixtureJPEG)
	case CapabilityImagePNG:
		return r.probeDocument(ctx, capability.ID, FixturePNG)
	case CapabilityImageWebP:
		return r.probeDocument(ctx, capability.ID, FixtureWebP)
	case CapabilityImageGIFStill:
		return r.probeDocument(ctx, capability.ID, FixtureGIFStill)
	case CapabilityImageGIFAnimated:
		return r.probeAnimated(ctx)
	case CapabilityVideoMP4:
		return r.probeDocument(ctx, capability.ID, FixtureMP4)
	case CapabilityQueryText:
		return r.probeTextQuery(ctx)
	case CapabilityQueryImageJPEG:
		return r.probeImageQuery(ctx, CapabilityImageJPEG, FixtureJPEG)
	case CapabilityQueryImagePNG:
		return r.probeImageQuery(ctx, CapabilityImagePNG, FixturePNG)
	case CapabilityQueryImageWebP:
		return r.probeImageQuery(ctx, CapabilityImageWebP, FixtureWebP)
	case CapabilityQueryImageGIF:
		return r.probeImageQuery(ctx, CapabilityImageGIFStill, FixtureGIFStill)
	case CapabilityQueryTextImage:
		return r.probeTextImageQuery(ctx)
	case CapabilityInterleaved:
		return r.probeInterleaved(ctx)
	case CapabilityBatchLimits:
		return r.probeBatch(ctx)
	default:
		return probeObservation{}, fmt.Errorf("voyage capability %q has no probe", capability.ID)
	}
}

// embedFixtures embeds fixture documents with every capability allowed; the
// probe is what establishes authorization.
func (r *probeRunner) embedFixtures(ctx context.Context, inputs []Input) (Result, error) {
	wireInputs := make([]wireInput, len(inputs))
	for index, input := range inputs {
		content, err := r.client.documentContent(input, r.authorized(), r.mediaPolicy())
		if err != nil {
			return Result{}, err
		}
		wireInputs[index] = wireInput{Content: content}
	}
	vectors, usage, metrics, err := r.client.embed(ctx, wireInputs, inputTypeDocument)
	if err != nil {
		return Result{}, err
	}
	return Result{Vectors: vectors, Usage: usage, Metrics: metrics}, nil
}

func (r *probeRunner) probeDocument(ctx context.Context, capabilityID, fixture string) (probeObservation, error) {
	part, err := r.fixtures.media(fixture)
	if err != nil {
		return probeObservation{}, err
	}
	observation := probeObservation{fixtureDigest: fixtureDigest(part.Bytes)}
	result, err := r.embedFixtures(ctx, []Input{{Parts: []Part{{Media: part}}}})
	if err != nil {
		return observation, err
	}
	r.references[capabilityID] = result.Vectors[0]
	observation.tokens = usageTokens(result.Usage)
	return observation, nil
}

func (r *probeRunner) probeAnimated(ctx context.Context) (probeObservation, error) {
	animated, err := r.fixtures.media(FixtureGIFAnimated)
	if err != nil {
		return probeObservation{}, err
	}
	still, err := r.fixtures.media(FixtureGIFStill)
	if err != nil {
		return probeObservation{}, err
	}
	observation := probeObservation{fixtureDigest: fixtureDigest(animated.Bytes, still.Bytes)}
	reference, ok := r.references[CapabilityImageGIFStill]
	if !ok {
		stillResult, err := r.embedFixtures(ctx, []Input{{Parts: []Part{{Media: still}}}})
		if err != nil {
			return observation, err
		}
		reference = stillResult.Vectors[0]
	}
	result, err := r.embedFixtures(ctx, []Input{{Parts: []Part{{Media: animated}}}})
	if err != nil {
		return observation, err
	}
	if values, ok := similarities(result.Vectors[0], reference); !ok || values[0] >= motionThreshold {
		observation.reason = ReasonMotionNotObserved
		return observation, nil
	}
	observation.tokens = usageTokens(result.Usage)
	return observation, nil
}

// referenceDocuments embeds the red and blue reference fixtures, each in its
// own one-item request so no batch ordering assumption leaks into the
// references other probes compare against.
func (r *probeRunner) referenceDocuments(ctx context.Context) ([]float32, []float32, error) {
	red, err := r.reference(ctx, FixtureRed)
	if err != nil {
		return nil, nil, err
	}
	blue, err := r.reference(ctx, FixtureBlue)
	if err != nil {
		return nil, nil, err
	}
	return red, blue, nil
}

// reference returns the cached one-item document embedding of a fixture,
// embedding it on first use.
func (r *probeRunner) reference(ctx context.Context, fixture string) ([]float32, error) {
	if vector, ok := r.references[fixture]; ok {
		return vector, nil
	}
	part, err := r.fixtures.media(fixture)
	if err != nil {
		return nil, err
	}
	result, err := r.embedFixtures(ctx, []Input{{Parts: []Part{{Media: part}}}})
	if err != nil {
		return nil, err
	}
	r.references[fixture] = result.Vectors[0]
	return result.Vectors[0], nil
}

func (r *probeRunner) embedQuery(ctx context.Context, query Input) ([]float32, Usage, error) {
	content, err := r.client.queryContent(query, r.authorized(), r.mediaPolicy())
	if err != nil {
		return nil, Usage{}, err
	}
	vectors, usage, _, err := r.client.embed(ctx, []wireInput{{Content: content}}, inputTypeQuery)
	if err != nil {
		return nil, Usage{}, err
	}
	return vectors[0], usage, nil
}

// probeTextQuery checks that the red-square text query ranks the red
// reference above the blue one.
func (r *probeRunner) probeTextQuery(ctx context.Context) (probeObservation, error) {
	observation := probeObservation{fixtureDigest: fixtureDigest([]byte(ProbeQueryText), r.fixtures[FixtureRed], r.fixtures[FixtureBlue])}
	red, blue, err := r.referenceDocuments(ctx)
	if err != nil {
		return observation, err
	}
	vector, usage, err := r.embedQuery(ctx, Input{Parts: []Part{{Text: ProbeQueryText}}})
	if err != nil {
		return observation, err
	}
	if values, ok := similarities(vector, red, blue); !ok || values[0] <= values[1] {
		observation.reason = ReasonRankingNotObserved
		return observation, nil
	}
	observation.tokens = usageTokens(usage)
	return observation, nil
}

// probeImageQuery embeds one format's document fixture as a query and checks
// that its own document embedding is at least tied for the closest reference
// and strictly closer than the farther one.
func (r *probeRunner) probeImageQuery(ctx context.Context, documentCapability, fixture string) (probeObservation, error) {
	part, err := r.fixtures.media(fixture)
	if err != nil {
		return probeObservation{}, err
	}
	observation := probeObservation{fixtureDigest: fixtureDigest(part.Bytes, r.fixtures[FixtureRed], r.fixtures[FixtureBlue])}
	red, blue, err := r.referenceDocuments(ctx)
	if err != nil {
		return observation, err
	}
	same, ok := r.references[documentCapability]
	if !ok {
		same, err = r.reference(ctx, fixture)
		if err != nil {
			return observation, err
		}
	}
	vector, usage, err := r.embedQuery(ctx, Input{Parts: []Part{{Media: part}}})
	if err != nil {
		return observation, err
	}
	values, ok := similarities(vector, same, red, blue)
	if !ok {
		observation.reason = ReasonRankingNotObserved
		return observation, nil
	}
	toSame, toRed, toBlue := values[0], values[1], values[2]
	if toSame < toRed || toSame < toBlue || toSame <= min(toRed, toBlue) {
		observation.reason = ReasonRankingNotObserved
		return observation, nil
	}
	observation.tokens = usageTokens(usage)
	return observation, nil
}

// probeTextImageQuery checks that a red text plus red JPEG query ranks the
// red reference above the blue one.
func (r *probeRunner) probeTextImageQuery(ctx context.Context) (probeObservation, error) {
	image, err := r.fixtures.media(FixtureJPEG)
	if err != nil {
		return probeObservation{}, err
	}
	observation := probeObservation{fixtureDigest: fixtureDigest([]byte(ProbeQueryText), image.Bytes, r.fixtures[FixtureRed], r.fixtures[FixtureBlue])}
	red, blue, err := r.referenceDocuments(ctx)
	if err != nil {
		return observation, err
	}
	vector, usage, err := r.embedQuery(ctx, Input{Parts: []Part{{Text: ProbeQueryText}, {Media: image}}})
	if err != nil {
		return observation, err
	}
	if values, ok := similarities(vector, red, blue); !ok || values[0] <= values[1] {
		observation.reason = ReasonRankingNotObserved
		return observation, nil
	}
	observation.tokens = usageTokens(usage)
	return observation, nil
}

func (r *probeRunner) probeInterleaved(ctx context.Context) (probeObservation, error) {
	blue, err := r.fixtures.media(FixtureBlue)
	if err != nil {
		return probeObservation{}, err
	}
	observation := probeObservation{fixtureDigest: fixtureDigest([]byte(ProbeInterleavedText), blue.Bytes)}
	_, blueOnly, err := r.referenceDocuments(ctx)
	if err != nil {
		return observation, err
	}
	result, err := r.embedFixtures(ctx, []Input{{Parts: []Part{{Text: ProbeInterleavedText}, {Media: blue}}}})
	if err != nil {
		return observation, err
	}
	if values, ok := similarities(result.Vectors[0], blueOnly); !ok || values[0] >= contributionThreshold {
		observation.reason = ReasonOrderNotObserved
		return observation, nil
	}
	observation.tokens = usageTokens(result.Usage)
	return observation, nil
}

func (r *probeRunner) probeBatch(ctx context.Context) (probeObservation, error) {
	red, err := r.fixtures.media(FixtureRed)
	if err != nil {
		return probeObservation{}, err
	}
	blue, err := r.fixtures.media(FixtureBlue)
	if err != nil {
		return probeObservation{}, err
	}
	observation := probeObservation{fixtureDigest: fixtureDigest(red.Bytes, blue.Bytes)}
	// References are embedded outside the batch so a consistent permutation
	// of results inside the batch cannot pass as correct ordering.
	redReference, blueReference, err := r.referenceDocuments(ctx)
	if err != nil {
		return observation, err
	}
	size := r.client.policy.values.MaxBatchItems
	inputs := make([]Input, size)
	for index := range inputs {
		part := red
		if index%2 == 1 {
			part = blue
		}
		inputs[index] = Input{Parts: []Part{{Media: part}}}
	}
	result, err := r.embedFixtures(ctx, inputs)
	if err != nil {
		return observation, err
	}
	for index, vector := range result.Vectors {
		expected, other := redReference, blueReference
		if index%2 == 1 {
			expected, other = blueReference, redReference
		}
		values, ok := similarities(vector, expected, other)
		if !ok || values[0] < clusterThreshold || values[0] <= values[1] {
			observation.reason = ReasonOrderNotObserved
			return observation, nil
		}
	}
	observation.tokens = usageTokens(result.Usage)
	return observation, nil
}

func usageTokens(usage Usage) *int64 {
	if !usage.Available {
		return nil
	}
	tokens := usage.TotalTokens
	return &tokens
}

// fixtureDigest is the short digest over length-prefixed fixture inputs.
func fixtureDigest(inputs ...[]byte) string {
	hasher := sha256.New()
	var length [8]byte
	for _, input := range inputs {
		binary.BigEndian.PutUint64(length[:], uint64(len(input)))
		hasher.Write(length[:])
		hasher.Write(input)
	}
	return hex.EncodeToString(hasher.Sum(nil)[:fixtureDigestLen/2])
}

// cosine returns the cosine similarity of two vectors, or false when either
// is empty, mismatched, or zero-norm; callers treat false as failed evidence
// rather than as any particular similarity.
func cosine(left, right []float32) (float64, bool) {
	if len(left) != len(right) || len(left) == 0 {
		return 0, false
	}
	var dot, leftNorm, rightNorm float64
	for index := range left {
		dot += float64(left[index]) * float64(right[index])
		leftNorm += float64(left[index]) * float64(left[index])
		rightNorm += float64(right[index]) * float64(right[index])
	}
	if leftNorm == 0 || rightNorm == 0 {
		return 0, false
	}
	return dot / (math.Sqrt(leftNorm) * math.Sqrt(rightNorm)), true
}

// similarities returns the cosine of probe against each reference, or false
// when any pair is not comparable.
func similarities(probe []float32, references ...[]float32) ([]float64, bool) {
	out := make([]float64, len(references))
	for index, reference := range references {
		value, ok := cosine(probe, reference)
		if !ok {
			return nil, false
		}
		out[index] = value
	}
	return out, true
}

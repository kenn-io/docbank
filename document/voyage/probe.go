package voyage

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"slices"
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

// textVariantSuffix keys the cached other-text composite each interleaved
// probe proved distinct from its baseline; the batch probe uses it as the
// composite member's contrast partner.
const textVariantSuffix = ":text-variant"

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
	runner := &probeRunner{client: client, fixtures: fixtures, references: map[string][]float32{}, passed: map[string]bool{}}
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
			runner.passed[capability.ID] = true
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
	passed     map[string]bool
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
	case CapabilityInterleavedJPEG:
		return r.probeInterleaved(ctx, CapabilityInterleavedJPEG, FixtureJPEG)
	case CapabilityInterleavedPNG:
		return r.probeInterleaved(ctx, CapabilityInterleavedPNG, FixturePNG)
	case CapabilityInterleavedWebP:
		return r.probeInterleaved(ctx, CapabilityInterleavedWebP, FixtureWebP)
	case CapabilityInterleavedGIFStill:
		return r.probeInterleaved(ctx, CapabilityInterleavedGIFStill, FixtureGIFStill)
	case CapabilityInterleavedGIFAnimated:
		return r.probeInterleaved(ctx, CapabilityInterleavedGIFAnimated, FixtureGIFAnimated)
	case CapabilityInterleavedMP4:
		return r.probeInterleaved(ctx, CapabilityInterleavedMP4, FixtureMP4)
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
	r.references[CapabilityImageGIFAnimated] = result.Vectors[0]
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

// probeTextImageQuery varies text and media independently. A provider must
// change its embedding for each counterfactual and rank the aligned baseline
// correctly, so consuming only one component cannot authorize the shape.
func (r *probeRunner) probeTextImageQuery(ctx context.Context) (probeObservation, error) {
	redImage, err := r.fixtures.media(FixtureRed)
	if err != nil {
		return probeObservation{}, err
	}
	blueImage, err := r.fixtures.media(FixtureBlue)
	if err != nil {
		return probeObservation{}, err
	}
	observation := probeObservation{fixtureDigest: fixtureDigest(
		[]byte(ProbeQueryText), []byte(ProbeBlueText), redImage.Bytes, blueImage.Bytes,
		r.fixtures[FixtureRed], r.fixtures[FixtureBlue],
	)}
	red, blue, err := r.referenceDocuments(ctx)
	if err != nil {
		return observation, err
	}
	baseline, baselineUsage, err := r.embedQuery(ctx, Input{Parts: []Part{{Text: ProbeQueryText}, {Media: redImage}}})
	if err != nil {
		return observation, err
	}
	mediaChanged, mediaUsage, err := r.embedQuery(ctx, Input{Parts: []Part{{Text: ProbeQueryText}, {Media: blueImage}}})
	if err != nil {
		return observation, err
	}
	textChanged, textUsage, err := r.embedQuery(ctx, Input{Parts: []Part{{Text: ProbeBlueText}, {Media: redImage}}})
	if err != nil {
		return observation, err
	}
	if values, ok := similarities(baseline, red, blue); !ok || values[0] <= values[1] {
		observation.reason = ReasonRankingNotObserved
		return observation, nil
	}
	if values, ok := similarities(baseline, mediaChanged, textChanged); !ok ||
		values[0] >= contributionThreshold || values[1] >= contributionThreshold {
		observation.reason = ReasonRankingNotObserved
		return observation, nil
	}
	observation.tokens = aggregateUsageTokens(baselineUsage, mediaUsage, textUsage)
	return observation, nil
}

// probeInterleaved demonstrates that a text-then-media document of one
// format embeds and that both parts contribute: swapping the media against a
// contrasting cached composite and swapping the text each move the vector.
// probeInterleaved demonstrates that a text-then-media document of one
// format embeds and that both parts contribute: swapping the media between
// contrasting fixtures of the SAME format and animation state moves the
// vector, and swapping the text moves it again. Cross-format comparisons are
// never used, so container-only sensitivity cannot pass as pixel
// contribution.
func (r *probeRunner) probeInterleaved(ctx context.Context, capabilityID, fixture string) (probeObservation, error) {
	part, err := r.fixtures.media(fixture)
	if err != nil {
		return probeObservation{}, err
	}
	variant, err := r.fixtures.media(fixtureVariants[fixture])
	if err != nil {
		return probeObservation{}, err
	}
	observation := probeObservation{fixtureDigest: fixtureDigest(
		[]byte(ProbeInterleavedText), []byte(ProbeBlueText), part.Bytes, variant.Bytes,
	)}
	baseline, err := r.embedFixtures(ctx, []Input{{Parts: []Part{{Text: ProbeInterleavedText}, {Media: part}}}})
	if err != nil {
		return observation, err
	}
	// The batch probe reuses this composite's one-item embedding as the
	// reference for the same composite inside a batch, and the other-text
	// composite below as its proven-distinct contrast partner.
	r.references[capabilityID] = baseline.Vectors[0]
	mediaChanged, err := r.embedFixtures(ctx, []Input{{Parts: []Part{{Text: ProbeInterleavedText}, {Media: variant}}}})
	if err != nil {
		return observation, err
	}
	textChanged, err := r.embedFixtures(ctx, []Input{{Parts: []Part{{Text: ProbeBlueText}, {Media: part}}}})
	if err != nil {
		return observation, err
	}
	r.references[capabilityID+textVariantSuffix] = textChanged.Vectors[0]
	if values, ok := similarities(baseline.Vectors[0], mediaChanged.Vectors[0], textChanged.Vectors[0]); !ok ||
		values[0] >= contributionThreshold || values[1] >= contributionThreshold {
		observation.reason = ReasonOrderNotObserved
		return observation, nil
	}
	observation.tokens = aggregateUsageTokens(baseline.Usage, mediaChanged.Usage, textChanged.Usage)
	return observation, nil
}

// probeBatch demonstrates index association for mixed batches. For every
// format that passed its own probe, batches hold the primary fixture, its
// contrasting same-format variant, and — when the interleaved capability
// passed — both independently proven text composites, so same-format
// reordering, duplication, and text dropping inside batches are all
// observable: a provider that drops composite text cannot satisfy both
// composites' contradictory contrast inequalities. One additional batch
// interleaves members across formats so a multi-format request is observed
// even when the item limit is too small for the grouped batches to mix.
// Batches run at the largest size the item AND request-byte limits allow, so
// oversized fixtures can never force an unbounded request. Every member must
// match an independently embedded one-item reference of its identical input
// and beat its planted contrast partner; cross-format inequalities are never
// asserted, because different containers of the same picture may embed alike.
func (r *probeRunner) probeBatch(ctx context.Context) (probeObservation, error) {
	documentFixtures := []struct {
		capability  string
		interleaved string
		fixture     string
	}{
		{CapabilityImagePNG, CapabilityInterleavedPNG, FixturePNG},
		{CapabilityImageJPEG, CapabilityInterleavedJPEG, FixtureJPEG},
		{CapabilityImageWebP, CapabilityInterleavedWebP, FixtureWebP},
		{CapabilityImageGIFStill, CapabilityInterleavedGIFStill, FixtureGIFStill},
		{CapabilityImageGIFAnimated, CapabilityInterleavedGIFAnimated, FixtureGIFAnimated},
		{CapabilityVideoMP4, CapabilityInterleavedMP4, FixtureMP4},
	}
	formatMembers := make([][]batchMember, 0, len(documentFixtures))
	for _, candidate := range documentFixtures {
		if !r.passed[candidate.capability] {
			continue
		}
		part, err := r.fixtures.media(candidate.fixture)
		if err != nil {
			return probeObservation{}, err
		}
		reference, ok := r.references[candidate.capability]
		if !ok {
			reference, err = r.reference(ctx, candidate.fixture)
			if err != nil {
				return probeObservation{}, err
			}
		}
		variant, err := r.fixtures.media(fixtureVariants[candidate.fixture])
		if err != nil {
			return probeObservation{}, err
		}
		variantReference, err := r.reference(ctx, fixtureVariants[candidate.fixture])
		if err != nil {
			return probeObservation{}, err
		}
		group := []batchMember{
			{input: Input{Parts: []Part{{Media: part}}}, reference: reference, contrast: variantReference, digest: part.Bytes},
			{input: Input{Parts: []Part{{Media: variant}}}, reference: variantReference, contrast: reference, digest: variant.Bytes},
		}
		if r.passed[candidate.interleaved] {
			compositeReference, ok := r.references[candidate.interleaved]
			textVariant, variantOK := r.references[candidate.interleaved+textVariantSuffix]
			if !ok || !variantOK {
				return probeObservation{}, fmt.Errorf("voyage batch probe is missing the %s references", candidate.interleaved)
			}
			group = append(group,
				batchMember{
					input:     Input{Parts: []Part{{Text: ProbeInterleavedText}, {Media: part}}},
					reference: compositeReference, contrast: textVariant,
					digest: append([]byte(ProbeInterleavedText), part.Bytes...),
				},
				batchMember{
					input:     Input{Parts: []Part{{Text: ProbeBlueText}, {Media: part}}},
					reference: textVariant, contrast: compositeReference,
					digest: append([]byte(ProbeBlueText), part.Bytes...),
				},
			)
		}
		formatMembers = append(formatMembers, group)
	}
	if len(formatMembers) == 0 {
		// No document capability passed. Attempt the reference batch anyway
		// so this result records the provider's actual failure mode instead
		// of a synthetic one.
		blue, err := r.fixtures.media(FixtureBlue)
		if err != nil {
			return probeObservation{}, err
		}
		observation := probeObservation{fixtureDigest: fixtureDigest(blue.Bytes)}
		reference, err := r.reference(ctx, FixtureBlue)
		if err != nil {
			return observation, err
		}
		formatMembers = append(formatMembers, []batchMember{{
			input:     Input{Parts: []Part{{Media: blue}}},
			reference: reference, digest: blue.Bytes,
		}})
	}
	grouped := make([]batchMember, 0, 4*len(formatMembers))
	for _, group := range formatMembers {
		grouped = append(grouped, group...)
	}
	digestInputs := make([][]byte, 0, len(grouped))
	for _, candidate := range grouped {
		digestInputs = append(digestInputs, candidate.digest)
	}
	observation := probeObservation{fixtureDigest: fixtureDigest(digestInputs...)}

	batches := r.chunkBatchMembers(grouped)
	if len(formatMembers) >= 2 {
		// Column-wise across formats: the first fitting batch mixes formats
		// even when the item limit is smaller than one format's group.
		mixed := make([]batchMember, 0, len(grouped))
		for column := 0; ; column++ {
			row := false
			for _, group := range formatMembers {
				if column < len(group) {
					mixed = append(mixed, group[column])
					row = true
				}
			}
			if !row {
				break
			}
		}
		if mixedBatches := r.chunkBatchMembers(mixed); len(mixedBatches) > 0 {
			batches = append(batches, mixedBatches[0])
		}
	}
	multiItem := false
	for _, batch := range batches {
		if len(batch) >= 2 {
			multiItem = true
			break
		}
	}
	if !multiItem && len(grouped) >= 2 {
		// The fixtures cannot form any multi-item request under the policy's
		// byte limit, so multi-item batches have no evidence.
		observation.reason = ReasonProviderLimit
		return observation, nil
	}

	zero := int64(0)
	tokens := &zero
	for _, batch := range batches {
		inputs := make([]Input, len(batch))
		for index, candidate := range batch {
			inputs[index] = candidate.input
		}
		result, err := r.embedFixtures(ctx, inputs)
		if err != nil {
			return observation, err
		}
		for index, vector := range result.Vectors {
			values, ok := similarities(vector, batch[index].reference)
			if !ok || values[0] < clusterThreshold {
				observation.reason = ReasonOrderNotObserved
				return observation, nil
			}
			if batch[index].contrast == nil {
				continue
			}
			contrastValues, contrastOK := similarities(vector, batch[index].contrast)
			if !contrastOK || values[0] <= contrastValues[0] {
				observation.reason = ReasonOrderNotObserved
				return observation, nil
			}
		}
		if tokens != nil {
			batchTokens := usageTokens(result.Usage)
			if batchTokens == nil {
				tokens = nil
			} else {
				*tokens += *batchTokens
			}
		}
	}
	observation.tokens = tokens
	return observation, nil
}

// batchMember is one probe batch slot: an input, the reference embedding of
// the identical one-item input, and the planted contrast partner it must
// beat.
type batchMember struct {
	input     Input
	reference []float32
	contrast  []float32
	digest    []byte
}

// chunkBatchMembers packs members into batches bounded by the policy's item
// limit and its estimated request-byte limit, checked before any request
// body is built.
func (r *probeRunner) chunkBatchMembers(members []batchMember) [][]batchMember {
	maxItems := r.client.policy.values.MaxBatchItems
	maxBytes := r.client.policy.values.MaxRequestBytes
	estimate := func(batch []batchMember) int64 {
		inputs := make([]Input, len(batch))
		for index, candidate := range batch {
			inputs[index] = candidate.input
		}
		return estimatedRequestBytes(inputs)
	}
	var batches [][]batchMember
	var current []batchMember
	for _, candidate := range members {
		proposed := append(slices.Clone(current), candidate)
		if len(current) > 0 && (len(proposed) > maxItems || estimate(proposed) > maxBytes) {
			batches = append(batches, current)
			current = nil
			proposed = []batchMember{candidate}
		}
		if len(current) == 0 && estimate(proposed) > maxBytes {
			// A member that cannot fit alone can never be sent; skip it
			// rather than building an unsendable request.
			continue
		}
		current = proposed
	}
	if len(current) > 0 {
		batches = append(batches, current)
	}
	return batches
}

func usageTokens(usage Usage) *int64 {
	if !usage.Available {
		return nil
	}
	tokens := usage.TotalTokens
	return &tokens
}

func aggregateUsageTokens(usages ...Usage) *int64 {
	var total int64
	for _, usage := range usages {
		if !usage.Available || usage.TotalTokens > math.MaxInt64-total {
			return nil
		}
		total += usage.TotalTokens
	}
	return &total
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
